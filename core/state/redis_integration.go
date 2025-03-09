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
	libcommon "github.com/erigontech/erigon-lib/common"
)

// redisWriterFactory is a function type for creating Redis writers
type redisWriterFactory func(blockNum uint64) RedisWriter

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
	StoreBlockInfo(header interface{}, root libcommon.Hash) error
}

var (
	// Global Redis client and writer instances - will be initialized on startup if Redis is enabled
	redisClient          *redis.Client
	redisStateWriter     StateWriter
	redisHistoryWriter   RedisWriter
	redisIntegrationOnce sync.Once
	
	// Factory function to create Redis writers - will be set by the redis-state package
	redisWriterFactoryFn redisWriterFactory
	
	// Instance cache to avoid creating new instances for the same block
	redisWriterCache     map[uint64]RedisWriter
	redisWriterCacheMux  sync.RWMutex
)

func init() {
	redisWriterCache = make(map[uint64]RedisWriter)
}

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

// SetRedisWriterFactory sets the factory function for creating Redis writers
func SetRedisWriterFactory(factory interface{}) {
	// Accept either our internal type or a more generic factory function from redis-state
	if typedFactory, ok := factory.(redisWriterFactory); ok {
		redisWriterFactoryFn = typedFactory
	} else if fn, ok := factory.(func(uint64) interface{}); ok {
		// Bridge from redis-state package
		redisWriterFactoryFn = func(blockNum uint64) RedisWriter {
			writer := fn(blockNum)
			if writer == nil {
				return nil
			}
			
			// Try to cast to RedisWriter
			if redisWriter, ok := writer.(RedisWriter); ok {
				return redisWriter
			}
			
			return nil
		}
	}
}

// GetRedisStateWriter returns a RedisWriter for the given block number or creates a new one
func GetRedisStateWriter(blockNum uint64) RedisWriter {
	if !IsRedisEnabled() || redisWriterFactoryFn == nil {
		return nil
	}

	// Check cache first with read lock
	redisWriterCacheMux.RLock()
	writer, exists := redisWriterCache[blockNum]
	redisWriterCacheMux.RUnlock()
	
	if exists {
		return writer
	}
	
	// Create new writer with write lock
	redisWriterCacheMux.Lock()
	defer redisWriterCacheMux.Unlock()
	
	// Double-check pattern
	if writer, exists = redisWriterCache[blockNum]; exists {
		return writer
	}
	
	// Call factory method to create writer
	writer = redisWriterFactoryFn(blockNum)
	if writer != nil {
		redisWriterCache[blockNum] = writer
	}
	
	return writer
}

// ClearRedisWriterCache clears the Redis writer cache
func ClearRedisWriterCache() {
	redisWriterCacheMux.Lock()
	defer redisWriterCacheMux.Unlock()
	
	// Clear cache
	redisWriterCache = make(map[uint64]RedisWriter)
}

// SetRedisStateWriter sets the Redis writer instance directly
// Used primarily for testing
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

// ShutdownRedis properly closes the Redis client connection
func ShutdownRedis() {
	if redisClient != nil {
		redisClient.Close()
		redisClient = nil
	}
	
	// Clear cache
	ClearRedisWriterCache()
}
