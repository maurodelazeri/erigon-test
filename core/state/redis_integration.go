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

package state

import (
	"context"
	"sync"

	"github.com/redis/go-redis/v9"

	"github.com/erigontech/erigon-lib/log/v3"
)

// RedisWriter interface represents a redis writer that can be used to sync state
type RedisWriter interface {
	StateWriter
	GetBlockNum() uint64
	SetTxNum(txNum uint64)
	GetTxNum() uint64
}

// RedisHistoricalWriter extends RedisWriter with historical data capabilities
type RedisHistoricalWriter interface {
	RedisWriter
	WriteChangeSets() error
	WriteHistory() error
	WriteBlockStart(blockNum uint64) error
	HandleTransaction(tx interface{}, receipt interface{}, blockNum uint64, txIndex uint64) error
}

var (
	// Global Redis client and writer instances - will be initialized on startup if Redis is enabled
	redisClient          *redis.Client
	redisStateWriter     StateWriter
	redisHistoryWriter   RedisWriter
	redisIntegrationOnce sync.Once
)

// InitializeRedisClient initializes the Redis client with the provided connection parameters
func InitializeRedisClient(ctx context.Context, url, password string, logger log.Logger) error {
	var err error
	redisIntegrationOnce.Do(func() {
		// Parse Redis URL
		opts, err := redis.ParseURL(url)
		if err != nil {
			logger.Error("Failed to parse Redis URL", "url", url, "err", err)
			return
		}

		// Set password if provided
		if password != "" {
			opts.Password = password
		}

		// Create Redis client
		redisClient = redis.NewClient(opts)

		// Test connection
		if err := redisClient.Ping(ctx).Err(); err != nil {
			logger.Error("Failed to connect to Redis", "err", err)
			return
		}

		logger.Info("Successfully connected to Redis", "url", url)
	})

	return err
}

// GetRedisStateWriter returns a RedisWriter for the current block number or creates a new one if not initialized
func GetRedisStateWriter(blockNum uint64) RedisWriter {
	if redisClient == nil {
		return nil
	}

	// We'll initialize the actual writer through a factory in the redis-state package
	// to be provided by the main Erigon process, avoiding circular dependencies
	if redisHistoryWriter == nil || redisHistoryWriter.GetBlockNum() != blockNum {
		// Need to create a new writer via factory
		// The factory pattern will be used outside this package to create and set the writer
		return nil
	}

	return redisHistoryWriter
}

// SetRedisStateWriter sets the Redis writer instance
func SetRedisStateWriter(writer RedisWriter) {
	redisHistoryWriter = writer
}

// IsRedisEnabled returns true if Redis integration is enabled
func IsRedisEnabled() bool {
	return redisClient != nil
}

// GetRedisClient returns the Redis client
func GetRedisClient() *redis.Client {
	return redisClient
}
