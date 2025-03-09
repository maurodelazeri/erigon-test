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
	"sync"

	"github.com/holiman/uint256"
	"github.com/redis/go-redis/v9"

	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/crypto"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon-lib/types/accounts"
	"github.com/erigontech/erigon/turbo/shards"
)

// RedisAccumulator wraps the regular accumulator to also write state changes to Redis
type RedisAccumulator struct {
	*shards.Accumulator
	redisClient *redis.Client
	redisWriter *RedisStateWriter
	logger      log.Logger
	ctx         context.Context
	mu          sync.RWMutex // Protect against concurrent access
}

// NewRedisAccumulator creates a new RedisAccumulator
func NewRedisAccumulator(accumulator *shards.Accumulator, redisClient *redis.Client, blockNum uint64, logger log.Logger) *RedisAccumulator {
	ctx, _ := context.WithCancel(context.Background())
	
	// Write a test key to Redis to verify connection works
	testCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	
	testKey := "redis_accumulator_initialized"
	err := redisClient.Set(testCtx, testKey, fmt.Sprintf("Initialized at %s with block %d", time.Now().Format(time.RFC3339), blockNum), 24*time.Hour).Err()
	if err != nil {
		logger.Error("Failed to write test key to Redis", "err", err)
	} else {
		logger.Info("Successfully wrote test key to Redis", "key", testKey)
		
		// Also log the Redis client connection details
		logger.Info("Redis client connection",
			"address", redisClient.Options().Addr,
			"db", redisClient.Options().DB,
			"poolSize", redisClient.Options().PoolSize)
	}
	
	return &RedisAccumulator{
		Accumulator: accumulator,
		redisClient: redisClient,
		redisWriter: NewRedisStateWriterWithLogger(redisClient, blockNum, logger),
		logger:      logger,
		ctx:         ctx,
	}
}

// GetRedisIntegrationFromAccumulator extracts the Redis integration from a RedisAccumulator
// This is used by the executor to get access to the Redis client and block writer
func GetRedisIntegrationFromAccumulator(acc *RedisAccumulator) (*RedisIntegration, error) {
	if acc == nil {
		return nil, fmt.Errorf("nil accumulator")
	}
	
	// Create a minimal Redis integration with just the client and block writer
	integration := &RedisIntegration{
		client:      acc.redisClient,
		logger:      acc.logger,
		blockWriter: NewBlockHeaderProcessor(acc.redisClient, acc.logger),
	}
	
	return integration, nil
}

// ChangeAccount overrides the accumulator's ChangeAccount method
func (ra *RedisAccumulator) ChangeAccount(address libcommon.Address, incarnation uint64, data []byte) {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	
	// Log that this method was called - this confirms our wrapper is active
	ra.logger.Info("RedisAccumulator.ChangeAccount called", 
		"address", address.Hex(), 
		"incarnation", incarnation,
		"dataSize", len(data))
	
	// Call original method
	ra.Accumulator.ChangeAccount(address, incarnation, data)

	// Mirror to Redis
	var acc accounts.Account
	if err := accounts.DeserialiseV3(&acc, data); err == nil {
		// Create a reader to get the original account data if available
		reader := NewRedisStateReaderWithLogger(ra.redisClient, ra.logger)
		original, err := reader.ReadAccountData(address)
		if err != nil {
			ra.logger.Debug("Could not read original account data, creating new", "address", address, "err", err)
			// Continue with nil original, not a critical error
		}
		
		// Check if this change is part of a canonical block
		// We'll use the blockNum for this check
		blockNum := ra.redisWriter.blockNum
		
		// Get block hash for this block number
		ctx := context.Background()
		blockKey := fmt.Sprintf("block:%d", blockNum)
		blockHash, err := ra.redisClient.HGet(ctx, blockKey, "hash").Result()
		
		// Check if this block is in the canonical chain
		isCanonical := true // Default to true
		if err == nil && blockHash != "" {
			canonicalKey := "canonicalChain"
			_, err := ra.redisClient.ZScore(ctx, canonicalKey, blockHash).Result()
			if err == redis.Nil {
				// This block is not in the canonical chain
				isCanonical = false
				ra.logger.Warn("Updating account state for non-canonical block", 
					"address", address, 
					"block", blockNum,
					"hash", blockHash)
			}
		}
		
		// Proceed with the update
		if err := ra.redisWriter.UpdateAccountData(address, original, &acc); err != nil {
			ra.logger.Error("Failed to write account to Redis", 
				"address", address, 
				"block", ra.redisWriter.blockNum, 
				"canonical", isCanonical,
				"err", err)
		} else {
			ra.logger.Debug("Account updated in Redis", 
				"address", address, 
				"block", ra.redisWriter.blockNum,
				"canonical", isCanonical)
		}
	} else {
		ra.logger.Error("Failed to deserialize account", "address", address, "err", err)
	}
}

// ChangeStorage overrides the accumulator's ChangeStorage method
func (ra *RedisAccumulator) ChangeStorage(address libcommon.Address, incarnation uint64, location libcommon.Hash, data []byte) {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	
	// Call original method
	ra.Accumulator.ChangeStorage(address, incarnation, location, data)

	// Mirror to Redis
	value := uint256.NewInt(0)
	if len(data) > 0 {
		value.SetBytes(data)
	}

	// Get original value if available
	reader := NewRedisStateReaderWithLogger(ra.redisClient, ra.logger)
	storageData, err := reader.ReadAccountStorage(address, incarnation, &location)
	var originalValue *uint256.Int
	if err == nil && len(storageData) > 0 {
		originalValue = uint256.NewInt(0).SetBytes(storageData)
	}

	// Check if this change is part of a canonical block
	// We'll use the blockNum for this check
	blockNum := ra.redisWriter.blockNum
	
	// Get block hash for this block number
	ctx := context.Background()
	blockKey := fmt.Sprintf("block:%d", blockNum)
	blockHash, err := ra.redisClient.HGet(ctx, blockKey, "hash").Result()
	
	// Check if this block is in the canonical chain
	isCanonical := true // Default to true
	if err == nil && blockHash != "" {
		canonicalKey := "canonicalChain"
		_, err := ra.redisClient.ZScore(ctx, canonicalKey, blockHash).Result()
		if err == redis.Nil {
			// This block is not in the canonical chain
			isCanonical = false
			ra.logger.Warn("Updating storage for non-canonical block", 
				"address", address, 
				"location", location.Hex(),
				"block", blockNum,
				"hash", blockHash)
		}
	}

	// Write the storage change to Redis with canonical status tracking
	if err := ra.redisWriter.WriteAccountStorage(address, incarnation, &location, originalValue, value); err != nil {
		ra.logger.Error("Failed to write storage to Redis", 
			"address", address, 
			"location", location.Hex(), 
			"block", ra.redisWriter.blockNum,
			"canonical", isCanonical,
			"err", err)
	} else {
		ra.logger.Debug("Storage updated in Redis", 
			"address", address, 
			"location", location.Hex(), 
			"block", ra.redisWriter.blockNum,
			"canonical", isCanonical)
	}
}

// ChangeCode overrides the accumulator's ChangeCode method
func (ra *RedisAccumulator) ChangeCode(address libcommon.Address, incarnation uint64, code []byte) {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	
	// Call original method
	ra.Accumulator.ChangeCode(address, incarnation, code)

	// Skip empty code
	if len(code) == 0 {
		return
	}

	// Calculate code hash
	codeHash := libcommon.BytesToHash(crypto.Keccak256(code))

	// Check if this change is part of a canonical block
	// We'll use the blockNum for this check
	blockNum := ra.redisWriter.blockNum
	
	// Get block hash for this block number
	ctx := context.Background()
	blockKey := fmt.Sprintf("block:%d", blockNum)
	blockHash, err := ra.redisClient.HGet(ctx, blockKey, "hash").Result()
	
	// Check if this block is in the canonical chain
	isCanonical := true // Default to true
	if err == nil && blockHash != "" {
		canonicalKey := "canonicalChain"
		_, err := ra.redisClient.ZScore(ctx, canonicalKey, blockHash).Result()
		if err == redis.Nil {
			// This block is not in the canonical chain
			isCanonical = false
			ra.logger.Warn("Updating code for non-canonical block", 
				"address", address, 
				"codeHash", codeHash.Hex(),
				"block", blockNum,
				"hash", blockHash)
		}
	}

	// Mirror to Redis
	if err := ra.redisWriter.UpdateAccountCode(address, incarnation, codeHash, code); err != nil {
		ra.logger.Error("Failed to write code to Redis", 
			"address", address, 
			"codeHash", codeHash.Hex(), 
			"codeSize", len(code), 
			"block", ra.redisWriter.blockNum,
			"canonical", isCanonical,
			"err", err)
	} else {
		ra.logger.Debug("Code updated in Redis", 
			"address", address, 
			"codeHash", codeHash.Hex(), 
			"codeSize", len(code), 
			"block", ra.redisWriter.blockNum,
			"canonical", isCanonical)
	}
}

// DeleteAccount overrides the accumulator's DeleteAccount method
func (ra *RedisAccumulator) DeleteAccount(address libcommon.Address) {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	
	// Get the original account before deletion if possible
	reader := NewRedisStateReaderWithLogger(ra.redisClient, ra.logger)
	original, err := reader.ReadAccountData(address)
	if err != nil {
		ra.logger.Debug("Could not read original account before deletion", "address", address, "err", err)
		// Continue with nil original, not a critical error
	}
	
	// Call original method
	ra.Accumulator.DeleteAccount(address)

	// Check if this change is part of a canonical block
	// We'll use the blockNum for this check
	blockNum := ra.redisWriter.blockNum
	
	// Get block hash for this block number
	ctx := context.Background()
	blockKey := fmt.Sprintf("block:%d", blockNum)
	blockHash, err := ra.redisClient.HGet(ctx, blockKey, "hash").Result()
	
	// Check if this block is in the canonical chain
	isCanonical := true // Default to true
	if err == nil && blockHash != "" {
		canonicalKey := "canonicalChain"
		_, err := ra.redisClient.ZScore(ctx, canonicalKey, blockHash).Result()
		if err == redis.Nil {
			// This block is not in the canonical chain
			isCanonical = false
			ra.logger.Warn("Deleting account in non-canonical block", 
				"address", address, 
				"block", blockNum,
				"hash", blockHash)
		}
	}

	// Mirror to Redis with the original account data
	if err := ra.redisWriter.DeleteAccount(address, original); err != nil {
		ra.logger.Error("Failed to delete account in Redis", 
			"address", address, 
			"block", ra.redisWriter.blockNum,
			"canonical", isCanonical,
			"err", err)
	} else {
		ra.logger.Debug("Account deleted in Redis", 
			"address", address, 
			"block", ra.redisWriter.blockNum,
			"canonical", isCanonical)
	}
}
