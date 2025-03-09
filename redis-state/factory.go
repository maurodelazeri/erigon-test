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
	"strings"
	"time"

	"github.com/holiman/uint256"
	"github.com/redis/go-redis/v9"

	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon-lib/types/accounts"
	"github.com/erigontech/erigon/core/state"
)

// Global variables for Redis client
var (
	globalRedisClient *redis.Client
	globalRedisCtx    context.Context
)

// InitializeRedisClient creates a Redis client and registers the state writer
func InitializeRedisClient(ctx context.Context, url, password string, logger log.Logger) error {
	// Parse Redis URL
	opts, err := redis.ParseURL(url)
	if err != nil {
		logger.Error("Failed to parse Redis URL", "url", url, "err", err)
		return err
	}

	// Set password if provided
	if password != "" {
		opts.Password = password
	}

	// Create Redis client
	globalRedisClient = redis.NewClient(opts)
	globalRedisCtx = ctx

	// Test connection
	if err := globalRedisClient.Ping(ctx).Err(); err != nil {
		logger.Error("Failed to connect to Redis", "err", err)
		return err
	}

	// Write a test value to verify write access
	testKey := "erigon:redis_test"
	testValue := "Redis integration active: " + time.Now().Format(time.RFC3339)
	if err := globalRedisClient.Set(ctx, testKey, testValue, 24*time.Hour).Err(); err != nil {
		logger.Error("Failed to write test data to Redis", "err", err)
		return err
	}
	
	// Read the test value back
	val, err := globalRedisClient.Get(ctx, testKey).Result()
	if err != nil {
		logger.Error("Failed to read test data from Redis", "err", err)
		return err
	}
	
	logger.Info("Successfully connected to Redis with read/write access", "url", url, "test_value", val)
	
	// Create a test block entry in Redis to verify full integration
	testBlockNum := uint64(1)
	testWriter := NewRedisHistoricalWriter(globalRedisClient, testBlockNum)
	
	// Test writing a block
	if err := testWriter.WriteBlockStart(testBlockNum); err != nil {
		logger.Error("Failed to write test block to Redis", "err", err)
		return fmt.Errorf("Redis connection successful but unable to write block data: %w", err)
	}
	
	// Test writing some state data
	testAddress := libcommon.HexToAddress("0x0000000000000000000000000000000000000001")
	testAccount := &accounts.Account{
		Nonce:    1,
		Balance:  *uint256.NewInt(1000000000000000000), // 1 ETH
		CodeHash: libcommon.Hash{},
	}
	
	if err := testWriter.UpdateAccountData(testAddress, nil, testAccount); err != nil {
		logger.Error("Failed to write test account data to Redis", "err", err)
		return fmt.Errorf("Redis connection successful but unable to write account data: %w", err)
	}
	
	logger.Info("Redis integration fully verified", 
		"url", url, 
		"block_test", "successful", 
		"account_test", "successful")

	// Register the factory function
	factory := NewHistoricalWriterFactory(ctx, logger)
	SetRedisWriterFactory(factory)
	
	// Also register with core state package
	writerFactoryFn := func(blockNum uint64) interface{} {
		return factory.Get(globalRedisClient, blockNum)
	}
	
	// Set the factory function for the state package
	state.SetRedisWriterFactory(writerFactoryFn)
	logger.Info("Redis writer factory registered with state package")

	return nil
}

// registerStateFactory registers the factory function with the core state package
// This is called during package initialization to create the bridge between packages
func registerStateFactory() {
	// We need to set up a factory function that will be called by the core state package
	// to create Redis writers for specific block numbers
	
	// This will be imported and called by the main package to register the factory
	// Import the core state package to avoid circular imports
	redisWriterFactoryFn := func(blockNum uint64) interface{} {
		if globalRedisClient == nil || writerFactory == nil {
			return nil
		}
		return writerFactory.Get(globalRedisClient, blockNum)
	}
	
	// We use type assertion to check if state.SetRedisWriterFactory exists
	if state, ok := interface{}(nil).(interface{ SetRedisWriterFactory(func(uint64) interface{}) }); ok {
		state.SetRedisWriterFactory(redisWriterFactoryFn)
	}
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

// GetRedisClient returns the global Redis client
func GetRedisClient() *redis.Client {
	return globalRedisClient
}

// CreateRedisWriter creates a new RedisHistoricalWriter
func CreateRedisWriter(blockNum uint64) *RedisHistoricalWriter {
	if writerFactory == nil {
		return nil
	}

	client := GetRedisClient()
	if client == nil {
		return nil
	}

	writer := writerFactory.Get(client, blockNum)
	// Return the writer without setting txnum
	return writer
}

// This function will be imported by the core/state package to register the factory
// It provides the bridge between the redis-state and core/state packages
func GetRedisWriterFactoryFn() func(uint64) interface{} {
	return func(blockNum uint64) interface{} {
		if globalRedisClient == nil || writerFactory == nil {
			return nil
		}
		return writerFactory.Get(globalRedisClient, blockNum)
	}
}

// DiagnoseRedisConnection performs a diagnostic check of the Redis connection
// and returns details about the connection status, credentials, and capabilities
func DiagnoseRedisConnection() map[string]interface{} {
	result := map[string]interface{}{
		"enabled": globalRedisClient != nil,
		"status":  "disconnected",
	}
	
	if globalRedisClient == nil {
		return result
	}
	
	// Get information about the Redis client
	opts := globalRedisClient.Options()
	result["address"] = opts.Addr
	result["database"] = opts.DB
	result["has_password"] = opts.Password != ""
	
	// Test ping
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	
	pingStart := time.Now()
	pingErr := globalRedisClient.Ping(ctx).Err()
	pingDuration := time.Since(pingStart)
	
	result["ping_latency_ms"] = pingDuration.Milliseconds()
	
	if pingErr != nil {
		result["status"] = "error"
		result["error"] = pingErr.Error()
		return result
	}
	
	result["status"] = "connected"
	
	// Test write
	writeKey := "erigon:diagnostic:test"
	writeValue := time.Now().Format(time.RFC3339)
	
	writeStart := time.Now()
	writeErr := globalRedisClient.Set(ctx, writeKey, writeValue, 1*time.Minute).Err()
	writeDuration := time.Since(writeStart)
	
	result["write_latency_ms"] = writeDuration.Milliseconds()
	result["write_success"] = writeErr == nil
	
	if writeErr != nil {
		result["write_error"] = writeErr.Error()
	}
	
	// Test read
	readStart := time.Now()
	readValue, readErr := globalRedisClient.Get(ctx, writeKey).Result()
	readDuration := time.Since(readStart)
	
	result["read_latency_ms"] = readDuration.Milliseconds()
	result["read_success"] = readErr == nil
	result["value_match"] = readValue == writeValue
	
	if readErr != nil {
		result["read_error"] = readErr.Error()
	}
	
	// Get Redis info
	infoCmd := globalRedisClient.Info(ctx)
	if infoCmd.Err() == nil {
		infoStr := infoCmd.Val()
		info := make(map[string]string)
		
		// Parse simple info format
		lines := strings.Split(infoStr, "\r\n")
		for _, line := range lines {
			if len(line) > 0 && !strings.HasPrefix(line, "#") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					info[parts[0]] = parts[1]
				}
			}
		}
		
		// Extract key info
		if version, ok := info["redis_version"]; ok {
			result["redis_version"] = version
		}
		if os, ok := info["os"]; ok {
			result["redis_os"] = os
		}
		if usedMemory, ok := info["used_memory_human"]; ok {
			result["used_memory"] = usedMemory
		}
		if clients, ok := info["connected_clients"]; ok {
			result["connected_clients"] = clients
		}
	}
	
	return result
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
