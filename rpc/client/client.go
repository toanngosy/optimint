package client

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/config"
	tmbytes "github.com/tendermint/tendermint/libs/bytes"
	tmmath "github.com/tendermint/tendermint/libs/math"
	tmpubsub "github.com/tendermint/tendermint/libs/pubsub"
	tmquery "github.com/tendermint/tendermint/libs/pubsub/query"
	"github.com/tendermint/tendermint/proxy"
	rpcclient "github.com/tendermint/tendermint/rpc/client"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	"github.com/tendermint/tendermint/types"

	abciconv "github.com/celestiaorg/optimint/conv/abci"
	"github.com/celestiaorg/optimint/mempool"
	"github.com/celestiaorg/optimint/node"
)

const (
	defaultPerPage = 30
	maxPerPage     = 100

	// TODO(tzdybal): make this configurable
	subscribeTimeout = 5 * time.Second
)

var (
	ErrConsensusStateNotAvailable = errors.New("consensus state not available in Optimint")
)

var _ rpcclient.Client = &Client{}

type Client struct {
	*types.EventBus
	config *config.RPCConfig

	node *node.Node
}

func NewClient(node *node.Node) *Client {
	return &Client{
		EventBus: node.EventBus(),
		config:   config.DefaultRPCConfig(),
		node:     node,
	}
}

func (c *Client) ABCIInfo(ctx context.Context) (*ctypes.ResultABCIInfo, error) {
	resInfo, err := c.query().InfoSync(proxy.RequestInfo)
	if err != nil {
		return nil, err
	}
	return &ctypes.ResultABCIInfo{Response: *resInfo}, nil
}

func (c *Client) ABCIQuery(ctx context.Context, path string, data tmbytes.HexBytes) (*ctypes.ResultABCIQuery, error) {
	return c.ABCIQueryWithOptions(ctx, path, data, rpcclient.DefaultABCIQueryOptions)
}

func (c *Client) ABCIQueryWithOptions(ctx context.Context, path string, data tmbytes.HexBytes, opts rpcclient.ABCIQueryOptions) (*ctypes.ResultABCIQuery, error) {
	resQuery, err := c.query().QuerySync(abci.RequestQuery{
		Path:   path,
		Data:   data,
		Height: opts.Height,
		Prove:  opts.Prove,
	})
	if err != nil {
		return nil, err
	}
	c.Logger.Info("ABCIQuery", "path", path, "data", data, "result", resQuery)
	return &ctypes.ResultABCIQuery{Response: *resQuery}, nil
}

// BroadcastTxCommit returns with the responses from CheckTx and DeliverTx.
// More: https://docs.tendermint.com/master/rpc/#/Tx/broadcast_tx_commit
func (c *Client) BroadcastTxCommit(ctx context.Context, tx types.Tx) (*ctypes.ResultBroadcastTxCommit, error) {
	// This implementation corresponds to Tendermints implementation from rpc/core/mempool.go.
	// ctx.RemoteAddr godoc: If neither HTTPReq nor WSConn is set, an empty string is returned.
	// This code is a local client, so we can assume that subscriber is ""
	subscriber := "" //ctx.RemoteAddr()

	if c.EventBus.NumClients() >= c.config.MaxSubscriptionClients {
		return nil, fmt.Errorf("max_subscription_clients %d reached", c.config.MaxSubscriptionClients)
	} else if c.EventBus.NumClientSubscriptions(subscriber) >= c.config.MaxSubscriptionsPerClient {
		return nil, fmt.Errorf("max_subscriptions_per_client %d reached", c.config.MaxSubscriptionsPerClient)
	}

	// Subscribe to tx being committed in block.
	subCtx, cancel := context.WithTimeout(ctx, subscribeTimeout)
	defer cancel()
	q := types.EventQueryTxFor(tx)
	deliverTxSub, err := c.EventBus.Subscribe(subCtx, subscriber, q)
	if err != nil {
		err = fmt.Errorf("failed to subscribe to tx: %w", err)
		c.Logger.Error("Error on broadcast_tx_commit", "err", err)
		return nil, err
	}
	defer func() {
		if err := c.EventBus.Unsubscribe(context.Background(), subscriber, q); err != nil {
			c.Logger.Error("Error unsubscribing from eventBus", "err", err)
		}
	}()

	// add to mempool and wait for CheckTx result
	checkTxResCh := make(chan *abci.Response, 1)
	err = c.node.Mempool.CheckTx(tx, func(res *abci.Response) {
		checkTxResCh <- res
	}, mempool.TxInfo{})
	if err != nil {
		c.Logger.Error("Error on broadcastTxCommit", "err", err)
		return nil, fmt.Errorf("error on broadcastTxCommit: %v", err)
	}
	checkTxResMsg := <-checkTxResCh
	checkTxRes := checkTxResMsg.GetCheckTx()
	if checkTxRes.Code != abci.CodeTypeOK {
		return &ctypes.ResultBroadcastTxCommit{
			CheckTx:   *checkTxRes,
			DeliverTx: abci.ResponseDeliverTx{},
			Hash:      tx.Hash(),
		}, nil
	}

	// broadcast tx
	err = c.node.P2P.GossipTx(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("tx added to local mempool but failure to broadcast: %w", err)
	}

	// Wait for the tx to be included in a block or timeout.
	select {
	case msg := <-deliverTxSub.Out(): // The tx was included in a block.
		deliverTxRes := msg.Data().(types.EventDataTx)
		return &ctypes.ResultBroadcastTxCommit{
			CheckTx:   *checkTxRes,
			DeliverTx: deliverTxRes.Result,
			Hash:      tx.Hash(),
			Height:    deliverTxRes.Height,
		}, nil
	case <-deliverTxSub.Cancelled():
		var reason string
		if deliverTxSub.Err() == nil {
			reason = "Tendermint exited"
		} else {
			reason = deliverTxSub.Err().Error()
		}
		err = fmt.Errorf("deliverTxSub was cancelled (reason: %s)", reason)
		c.Logger.Error("Error on broadcastTxCommit", "err", err)
		return &ctypes.ResultBroadcastTxCommit{
			CheckTx:   *checkTxRes,
			DeliverTx: abci.ResponseDeliverTx{},
			Hash:      tx.Hash(),
		}, err
	case <-time.After(c.config.TimeoutBroadcastTxCommit):
		err = errors.New("timed out waiting for tx to be included in a block")
		c.Logger.Error("Error on broadcastTxCommit", "err", err)
		return &ctypes.ResultBroadcastTxCommit{
			CheckTx:   *checkTxRes,
			DeliverTx: abci.ResponseDeliverTx{},
			Hash:      tx.Hash(),
		}, err
	}
}

// BroadcastTxAsync returns right away, with no response. Does not wait for
// CheckTx nor DeliverTx results.
// More: https://docs.tendermint.com/master/rpc/#/Tx/broadcast_tx_async
func (c *Client) BroadcastTxAsync(ctx context.Context, tx types.Tx) (*ctypes.ResultBroadcastTx, error) {
	err := c.node.Mempool.CheckTx(tx, nil, mempool.TxInfo{})
	if err != nil {
		return nil, err
	}
	// gossipTx optimistically
	err = c.node.P2P.GossipTx(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("tx added to local mempool but failed to gossip: %w", err)
	}
	return &ctypes.ResultBroadcastTx{Hash: tx.Hash()}, nil
}

// BroadcastTxSync returns with the response from CheckTx. Does not wait for
// DeliverTx result.
// More: https://docs.tendermint.com/master/rpc/#/Tx/broadcast_tx_sync
func (c *Client) BroadcastTxSync(ctx context.Context, tx types.Tx) (*ctypes.ResultBroadcastTx, error) {
	resCh := make(chan *abci.Response, 1)
	err := c.node.Mempool.CheckTx(tx, func(res *abci.Response) {
		resCh <- res
	}, mempool.TxInfo{})
	if err != nil {
		return nil, err
	}
	res := <-resCh
	r := res.GetCheckTx()

	// gossip the transaction if it's in the mempool.
	// Note: we have to do this here because, unlike the tendermint mempool reactor, there
	// is no routine that gossips transactions after they enter the pool
	if r.Code == abci.CodeTypeOK {
		err = c.node.P2P.GossipTx(ctx, tx)
		if err != nil {
			// the transaction must be removed from the mempool if it cannot be gossiped.
			// if this does not occur, then the user will not be able to try again using
			// this node, as the CheckTx call above will return an error indicating that
			// the tx is already in the mempool
			c.node.Mempool.RemoveTxByKey(mempool.TxKey(tx), true)
			return nil, fmt.Errorf("valid tra: %w", err)
		}
	}

	return &ctypes.ResultBroadcastTx{
		Code:      r.Code,
		Data:      r.Data,
		Log:       r.Log,
		Codespace: r.Codespace,
		Hash:      tx.Hash(),
	}, nil
}

func (c *Client) Subscribe(ctx context.Context, subscriber, query string, outCapacity ...int) (out <-chan ctypes.ResultEvent, err error) {
	q, err := tmquery.New(query)
	if err != nil {
		return nil, fmt.Errorf("failed to parse query: %w", err)
	}

	outCap := 1
	if len(outCapacity) > 0 {
		outCap = outCapacity[0]
	}

	var sub types.Subscription
	if outCap > 0 {
		sub, err = c.EventBus.Subscribe(ctx, subscriber, q, outCap)
	} else {
		sub, err = c.EventBus.SubscribeUnbuffered(ctx, subscriber, q)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe: %w", err)
	}

	outc := make(chan ctypes.ResultEvent, outCap)
	go c.eventsRoutine(sub, subscriber, q, outc)

	return outc, nil
}

func (c *Client) Unsubscribe(ctx context.Context, subscriber, query string) error {
	q, err := tmquery.New(query)
	if err != nil {
		return fmt.Errorf("failed to parse query: %w", err)
	}
	return c.EventBus.Unsubscribe(ctx, subscriber, q)
}

func (c *Client) Genesis(_ context.Context) (*ctypes.ResultGenesis, error) {
	return &ctypes.ResultGenesis{Genesis: c.node.GetGenesis()}, nil
}

func (c *Client) GenesisChunked(context context.Context, id uint) (*ctypes.ResultGenesisChunk, error) {
	// needs genesis provider
	panic("GenesisChunked - not implemented!")
}

func (c *Client) BlockchainInfo(ctx context.Context, minHeight, maxHeight int64) (*ctypes.ResultBlockchainInfo, error) {
	// needs block store
	panic("BlockchainInfo - not implemented!")
}

func (c *Client) NetInfo(ctx context.Context) (*ctypes.ResultNetInfo, error) {
	// needs P2P layer

	res := ctypes.ResultNetInfo{
		Listening: true,
	}
	for _, ma := range c.node.P2P.Addrs() {
		res.Listeners = append(res.Listeners, ma.String())
	}
	peers := c.node.P2P.Peers()
	res.NPeers = len(peers)
	for _, peer := range peers {
		res.Peers = append(res.Peers, ctypes.Peer{
			NodeInfo:         peer.NodeInfo,
			IsOutbound:       peer.IsOutbound,
			ConnectionStatus: peer.ConnectionStatus,
			RemoteIP:         peer.RemoteIP,
		})
	}

	return &res, nil
}

func (c *Client) DumpConsensusState(ctx context.Context) (*ctypes.ResultDumpConsensusState, error) {
	return nil, ErrConsensusStateNotAvailable
}

func (c *Client) ConsensusState(ctx context.Context) (*ctypes.ResultConsensusState, error) {
	return nil, ErrConsensusStateNotAvailable
}

func (c *Client) ConsensusParams(ctx context.Context, height *int64) (*ctypes.ResultConsensusParams, error) {
	// needs state storage
	panic("ConsensusParams - not implemented!")
}

func (c *Client) Health(ctx context.Context) (*ctypes.ResultHealth, error) {
	return &ctypes.ResultHealth{}, nil
}

func (c *Client) Block(ctx context.Context, height *int64) (*ctypes.ResultBlock, error) {
	// needs block store
	var h uint64
	if height == nil {
		h = c.node.Store.Height()
	} else {
		h = uint64(*height)
	}

	block, err := c.node.Store.LoadBlock(h)
	if err != nil {
		return nil, err
	}
	hash := block.Hash()
	abciBlock, err := abciconv.ToABCIBlock(block)
	if err != nil {
		return nil, err
	}
	return &ctypes.ResultBlock{
		BlockID: types.BlockID{
			Hash: hash[:],
			PartSetHeader: types.PartSetHeader{
				Total: 0,
				Hash:  nil,
			},
		},
		Block: abciBlock,
	}, nil
}

func (c *Client) BlockByHash(ctx context.Context, hash []byte) (*ctypes.ResultBlock, error) {
	var h [32]byte
	copy(h[:], hash)

	block, err := c.node.Store.LoadBlockByHash(h)
	if err != nil {
		return nil, err
	}

	abciBlock, err := abciconv.ToABCIBlock(block)
	if err != nil {
		return nil, err
	}
	return &ctypes.ResultBlock{
		BlockID: types.BlockID{
			Hash: h[:],
			PartSetHeader: types.PartSetHeader{
				Total: 0,
				Hash:  nil,
			},
		},
		Block: abciBlock,
	}, nil
}

func (c *Client) BlockResults(ctx context.Context, height *int64) (*ctypes.ResultBlockResults, error) {
	var h uint64
	if height == nil {
		h = c.node.Store.Height()
	} else {
		h = uint64(*height)
	}
	resp, err := c.node.Store.LoadBlockResponses(h)
	if err != nil {
		return nil, err
	}

	return &ctypes.ResultBlockResults{
		Height:                int64(h),
		TxsResults:            resp.DeliverTxs,
		BeginBlockEvents:      resp.BeginBlock.Events,
		EndBlockEvents:        resp.EndBlock.Events,
		ValidatorUpdates:      resp.EndBlock.ValidatorUpdates,
		ConsensusParamUpdates: resp.EndBlock.ConsensusParamUpdates,
	}, nil
}

func (c *Client) Commit(ctx context.Context, height *int64) (*ctypes.ResultCommit, error) {
	// needs block store
	panic("Commit - not implemented!")
}

func (c *Client) Validators(ctx context.Context, height *int64, page, perPage *int) (*ctypes.ResultValidators, error) {
	panic("Validators - not implemented!")
}

func (c *Client) Tx(ctx context.Context, hash []byte, prove bool) (*ctypes.ResultTx, error) {
	// needs block store, tx index (?)
	panic("Tx - not implemented!")
}

func (c *Client) TxSearch(ctx context.Context, query string, prove bool, pagePtr, perPagePtr *int, orderBy string) (*ctypes.ResultTxSearch, error) {
	q, err := tmquery.New(query)
	if err != nil {
		return nil, err
	}

	results, err := c.node.TxIndexer.Search(ctx, q)
	if err != nil {
		return nil, err
	}

	// sort results (must be done before pagination)
	switch orderBy {
	case "desc":
		sort.Slice(results, func(i, j int) bool {
			if results[i].Height == results[j].Height {
				return results[i].Index > results[j].Index
			}
			return results[i].Height > results[j].Height
		})
	case "asc", "":
		sort.Slice(results, func(i, j int) bool {
			if results[i].Height == results[j].Height {
				return results[i].Index < results[j].Index
			}
			return results[i].Height < results[j].Height
		})
	default:
		return nil, errors.New("expected order_by to be either `asc` or `desc` or empty")
	}

	// paginate results
	totalCount := len(results)
	perPage := validatePerPage(perPagePtr)

	page, err := validatePage(pagePtr, perPage, totalCount)
	if err != nil {
		return nil, err
	}

	skipCount := validateSkipCount(page, perPage)
	pageSize := tmmath.MinInt(perPage, totalCount-skipCount)

	apiResults := make([]*ctypes.ResultTx, 0, pageSize)
	for i := skipCount; i < skipCount+pageSize; i++ {
		r := results[i]

		var proof types.TxProof
		/*if prove {
			block := nil                               //env.BlockStore.LoadBlock(r.Height)
			proof = block.Data.Txs.Proof(int(r.Index)) // XXX: overflow on 32-bit machines
		}*/

		apiResults = append(apiResults, &ctypes.ResultTx{
			Hash:     types.Tx(r.Tx).Hash(),
			Height:   r.Height,
			Index:    r.Index,
			TxResult: r.Result,
			Tx:       r.Tx,
			Proof:    proof,
		})
	}

	return &ctypes.ResultTxSearch{Txs: apiResults, TotalCount: totalCount}, nil
}

// BlockSearch defines a method to search for a paginated set of blocks by
// BeginBlock and EndBlock event search criteria.
func (c *Client) BlockSearch(ctx context.Context, query string, page, perPage *int, orderBy string) (*ctypes.ResultBlockSearch, error) {
	panic("BlockSearch - not implemented!")
}

func (c *Client) Status(ctx context.Context) (*ctypes.ResultStatus, error) {
	latest, err := c.node.Store.LoadBlock(c.node.Store.Height())
	if err != nil {
		// TODO(tzdybal): extract error
		return nil, fmt.Errorf("failed to find latest block: %w", err)
	}

	latestBlockHash := latest.Header.DataHash
	latestAppHash := latest.Header.AppHash
	latestHeight := latest.Header.Height
	latestBlockTimeNano := latest.Header.Time

	result := &ctypes.ResultStatus{
		// TODO(tzdybal): NodeInfo, ValidatorInfo
		SyncInfo: ctypes.SyncInfo{
			LatestBlockHash:   latestBlockHash[:],
			LatestAppHash:     latestAppHash[:],
			LatestBlockHeight: int64(latestHeight),
			LatestBlockTime:   time.Unix(0, int64(latestBlockTimeNano)),
			// TODO(tzdybal): add missing fields
			//EarliestBlockHash:   earliestBlockHash,
			//EarliestAppHash:     earliestAppHash,
			//EarliestBlockHeight: earliestBlockHeight,
			//EarliestBlockTime:   time.Unix(0, earliestBlockTimeNano),
			//CatchingUp:          env.ConsensusReactor.WaitSync(),
		},
	}
	return result, nil
}

func (c *Client) BroadcastEvidence(ctx context.Context, evidence types.Evidence) (*ctypes.ResultBroadcastEvidence, error) {
	// needs evidence pool?
	panic("BroadcastEvidence - not implemented!")
}

func (c *Client) NumUnconfirmedTxs(ctx context.Context) (*ctypes.ResultUnconfirmedTxs, error) {
	return &ctypes.ResultUnconfirmedTxs{
		Count:      c.node.Mempool.Size(),
		Total:      c.node.Mempool.Size(),
		TotalBytes: c.node.Mempool.TxsBytes(),
	}, nil

}

func (c *Client) UnconfirmedTxs(ctx context.Context, limitPtr *int) (*ctypes.ResultUnconfirmedTxs, error) {
	// reuse per_page validator
	limit := validatePerPage(limitPtr)

	txs := c.node.Mempool.ReapMaxTxs(limit)
	return &ctypes.ResultUnconfirmedTxs{
		Count:      len(txs),
		Total:      c.node.Mempool.Size(),
		TotalBytes: c.node.Mempool.TxsBytes(),
		Txs:        txs}, nil
}

func (c *Client) CheckTx(ctx context.Context, tx types.Tx) (*ctypes.ResultCheckTx, error) {
	res, err := c.mempool().CheckTxSync(abci.RequestCheckTx{Tx: tx})
	if err != nil {
		return nil, err
	}
	return &ctypes.ResultCheckTx{ResponseCheckTx: *res}, nil
}

func (c *Client) eventsRoutine(sub types.Subscription, subscriber string, q tmpubsub.Query, outc chan<- ctypes.ResultEvent) {
	for {
		select {
		case msg := <-sub.Out():
			result := ctypes.ResultEvent{Query: q.String(), Data: msg.Data(), Events: msg.Events()}
			if cap(outc) == 0 {
				outc <- result
			} else {
				select {
				case outc <- result:
				default:
					c.Logger.Error("wanted to publish ResultEvent, but out channel is full", "result", result, "query", result.Query)
				}
			}
		case <-sub.Cancelled():
			if sub.Err() == tmpubsub.ErrUnsubscribed {
				return
			}

			c.Logger.Error("subscription was cancelled, resubscribing...", "err", sub.Err(), "query", q.String())
			sub = c.resubscribe(subscriber, q)
			if sub == nil { // client was stopped
				return
			}
		case <-c.Quit():
			return
		}
	}
}

// Try to resubscribe with exponential backoff.
func (c *Client) resubscribe(subscriber string, q tmpubsub.Query) types.Subscription {
	attempts := 0
	for {
		if !c.IsRunning() {
			return nil
		}

		sub, err := c.EventBus.Subscribe(context.Background(), subscriber, q)
		if err == nil {
			return sub
		}

		attempts++
		time.Sleep((10 << uint(attempts)) * time.Millisecond) // 10ms -> 20ms -> 40ms
	}
}

func (c *Client) consensus() proxy.AppConnConsensus {
	return c.node.ProxyApp().Consensus()
}

func (c *Client) mempool() proxy.AppConnMempool {
	return c.node.ProxyApp().Mempool()
}

func (c *Client) query() proxy.AppConnQuery {
	return c.node.ProxyApp().Query()
}

func (c *Client) snapshot() proxy.AppConnSnapshot {
	return c.node.ProxyApp().Snapshot()
}

func validatePerPage(perPagePtr *int) int {
	if perPagePtr == nil { // no per_page parameter
		return defaultPerPage
	}

	perPage := *perPagePtr
	if perPage < 1 {
		return defaultPerPage
	} else if perPage > maxPerPage {
		return maxPerPage
	}
	return perPage
}

func validatePage(pagePtr *int, perPage, totalCount int) (int, error) {
	if perPage < 1 {
		panic(fmt.Sprintf("zero or negative perPage: %d", perPage))
	}

	if pagePtr == nil { // no page parameter
		return 1, nil
	}

	pages := ((totalCount - 1) / perPage) + 1
	if pages == 0 {
		pages = 1 // one page (even if it's empty)
	}
	page := *pagePtr
	if page <= 0 || page > pages {
		return 1, fmt.Errorf("page should be within [1, %d] range, given %d", pages, page)
	}

	return page, nil
}

func validateSkipCount(page, perPage int) int {
	skipCount := (page - 1) * perPage
	if skipCount < 0 {
		return 0
	}

	return skipCount
}
