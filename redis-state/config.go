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
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon/turbo/shards"
)

// Config holds the configuration for the Redis integration
type Config struct {
	// Enable Redis integration
	Enabled bool 
	
	// Redis connection details
	RedisURL   string
	RedisDB    int
	RedisPass  string
	
	// Redis client options
	PoolSize   int
	MaxRetries int
	Timeout    time.Duration
	
	// Integration options
	LogLevel string
}

// RedisIntegration manages the integration between Erigon and Redis
type RedisIntegration struct {
	config      Config
	client      *redis.Client
	logger      log.Logger
	blockWriter *BlockHeaderProcessor
	ctx         context.Context
	cancel      context.CancelFunc
}

// NewRedisIntegration creates a new RedisIntegration instance
func NewRedisIntegration(config Config, logger log.Logger) (*RedisIntegration, error) {
	if !config.Enabled {
		return nil, nil
	}

	// Set default values if not provided
	if config.PoolSize <= 0 {
		config.PoolSize = 10
	}
	
	if config.MaxRetries <= 0 {
		config.MaxRetries = 3
	}
	
	if config.Timeout <= 0 {
		config.Timeout = 5 * time.Second
	}

	// Setup Redis client options
	opts, err := redis.ParseURL(config.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Redis URL: %w", err)
	}
	
	// Override with custom options
	opts.Password = config.RedisPass
	opts.DB = config.RedisDB
	opts.PoolSize = config.PoolSize
	opts.MaxRetries = config.MaxRetries
	
	client := redis.NewClient(opts)

	// Test the Redis connection
	ctx, cancel := context.WithTimeout(context.Background(), config.Timeout)
	defer cancel()
	
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}
	
	ctx, cancel = context.WithCancel(context.Background())
	
	return &RedisIntegration{
		config:      config,
		client:      client,
		logger:      logger,
		blockWriter: NewBlockHeaderProcessor(client, logger),
		ctx:         ctx,
		cancel:      cancel,
	}, nil
}

// Close closes the Redis client and cancels any pending operations
func (ri *RedisIntegration) Close() error {
	if ri == nil {
		return nil
	}
	
	if ri.cancel != nil {
		ri.cancel()
	}
	
	if ri.client != nil {
		return ri.client.Close()
	}
	
	return nil
}

// IsEnabled returns whether the Redis integration is enabled
func (ri *RedisIntegration) IsEnabled() bool {
	return ri != nil && ri.client != nil
}

// WrapAccumulator wraps a state change accumulator to also write to Redis
func (ri *RedisIntegration) WrapAccumulator(accumulator interface{}, blockNum uint64) *shards.Accumulator {
	if !ri.IsEnabled() {
		return accumulator.(*shards.Accumulator)
	}
	
	// Check if it's a shards.Accumulator
	if acc, ok := accumulator.(*shards.Accumulator); ok {
		// Create a Redis accumulator wrapper and use it properly
		redisAcc := NewRedisAccumulator(acc, ri.client, blockNum, ri.logger)
		
		// Log successful wrapping
		ri.logger.Info("Successfully wrapped accumulator for Redis state mirroring", 
			"blockNum", blockNum)
			
		// Return the Redis accumulator, which embeds and extends the original
		return redisAcc.Accumulator
	}
	
	// If it's not a known accumulator type, return as is
	ri.logger.Warn("Unknown accumulator type, not wrapping for Redis mirroring", "type", fmt.Sprintf("%T", accumulator))
	if acc, ok := accumulator.(*shards.Accumulator); ok {
		return acc
	}
	return nil
}

// GetBlockWriter returns the block writer
func (ri *RedisIntegration) GetBlockWriter() *BlockHeaderProcessor {
	if ri == nil {
		return nil
	}
	return ri.blockWriter
}

// GetClient returns the Redis client
func (ri *RedisIntegration) GetClient() *redis.Client {
	if ri == nil {
		return nil
	}
	return ri.client
}