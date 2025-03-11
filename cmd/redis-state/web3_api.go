package main

import "github.com/erigontech/erigon-lib/log/v3"

type Web3API struct {
	logger log.Logger
}

// ClientVersion returns the client version
func (api *Web3API) ClientVersion() string {
	return "Redis/v0.1.0"
}
