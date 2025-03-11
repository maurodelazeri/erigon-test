package main

import "github.com/erigontech/erigon-lib/log/v3"

type NetAPI struct {
	logger log.Logger
}

// Version returns the network identifier
func (api *NetAPI) Version() string {
	return "1" // Mainnet
}
