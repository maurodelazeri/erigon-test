// Copyright 2024 The Erigon Authors
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package redisstate

import (
	"context"

	"github.com/redis/go-redis/v9"

	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon/core/state"
)

// InitializeRedisClient creates a Redis client and registers the state writer in the state package
func InitializeRedisClient(ctx context.Context, url, password string, logger log.Logger) error {
	if err := state.InitializeRedisClient(ctx, url, password, logger); err != nil {
		return err
	}

	// Register the factory function
	SetRedisWriterFactory(NewHistoricalWriterFactory(ctx, logger))
	
	return nil
}

// HistoricalWriterFactory creates RedisHistoricalWriter instances
type HistoricalWriterFactory struct {
	ctx    context.Context
	logger log.Logger
	client *redis.Client
}

// NewHistoricalWriterFactory creates a new factory for RedisHistoricalWriter
func NewHistoricalWriterFactory(ctx context.Context, logger log.Logger) *HistoricalWriterFactory {
	return &HistoricalWriterFactory{
		ctx:    ctx,
		logger: logger,
	}
}

// Get creates a new RedisHistoricalWriter for the given block number
func (f *HistoricalWriterFactory) Get(client *redis.Client, blockNum uint64) *RedisHistoricalWriter {
	writer := NewRedisHistoricalWriter(client, blockNum)
	return writer
}

var writerFactory *HistoricalWriterFactory

// SetRedisWriterFactory sets the global writer factory
func SetRedisWriterFactory(factory *HistoricalWriterFactory) {
	writerFactory = factory
}

// GetRedisWriterFactory returns the global writer factory
func GetRedisWriterFactory() *HistoricalWriterFactory {
	return writerFactory
}

// GetRedisClient returns the client from the state package
func GetRedisClient() *redis.Client {
	return state.GetRedisClient()
}

// CreateRedisWriter creates a new RedisHistoricalWriter
func CreateRedisWriter(blockNum uint64) state.RedisWriter {
	if writerFactory == nil {
		return nil
	}
	
	client := GetRedisClient()
	if client == nil {
		return nil
	}
	
	writer := writerFactory.Get(client, blockNum)
	state.SetRedisStateWriter(writer)
	return writer
}

// HandleChainReorg handles chain reorganization by updating the Redis database
// to reflect the new canonical chain
func HandleChainReorg(from, to uint64, logger log.Logger) error {
	if writerFactory == nil || GetRedisClient() == nil {
		return nil
	}
	
	logger.Info("Handling Redis state reorg", "from", from, "to", to)
	
	// For reorgs, we mark the block with txNum 0 in Redis to indicate it's part of a reorg
	// The actual state will be updated when we process the new canonical blocks
	for blockNum := from; blockNum > to; blockNum-- {
		writer := CreateRedisWriter(blockNum)
		if writer != nil {
			writer.SetTxNum(0) // 0 is a special marker for reorged blocks
			logger.Debug("Marked Redis state for reorg", "block", blockNum)
		}
	}
	
	return nil
}