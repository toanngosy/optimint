package registry

import (
	"github.com/celestiaorg/optimint/da"
	"github.com/celestiaorg/optimint/da/grpc"
	"github.com/celestiaorg/optimint/da/mock"
)

// this is a central registry for all Data Availability Layer Clients
var clients = map[string]func() da.DataAvailabilityLayerClient{
	"mock": func() da.DataAvailabilityLayerClient { return &mock.MockDataAvailabilityLayerClient{} },
	"grpc": func() da.DataAvailabilityLayerClient { return &grpc.DataAvailabilityLayerClient{} },
}

// GetClient returns client identified by name.
func GetClient(name string) da.DataAvailabilityLayerClient {
	return clients[name]()
}
