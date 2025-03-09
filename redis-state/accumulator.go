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
	"time"

	"github.com/holiman/uint256"
	"github.com/redis/go-redis/v9"

	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/crypto"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon-lib/types/accounts"
	"github.com/erigontech/erigon/turbo/shards"
)

// RedisAccumulator is a wrapper for shards.Accumulator that adds Redis functionality
// It implements all methods from the original shards.Accumulator, forwarding calls to
// the inner accumulator, but adding Redis write operations.
// The key is that we don't embed shards.Accumulator - we wrap it and delegate all
// operations to the underlying accumulator after handling Redis operations.
type RedisAccumulator struct {
	acc         *shards.Accumulator // The original accumulator we delegate to
	redisClient *redis.Client
	redisWriter *RedisStateWriter
	logger      log.Logger
	ctx         context.Context
	mu          sync.RWMutex // Protect against concurrent access
	
	// Stores a slice of account change interceptors we've applied for debugging
	accountChangeCount int
}

// We intentionally don't do a compile-time type assertion here
// since RedisAccumulator wraps but isn't an exact *Accumulator

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
	
	// Create Redis accumulator that will capture state changes
	redisAcc := &RedisAccumulator{
		acc:         accumulator,
		redisClient: redisClient,
		redisWriter: NewRedisStateWriterWithLogger(redisClient, blockNum, logger),
		logger:      logger,
		ctx:         ctx,
	}
	
	// Log that the accumulator was created
	logger.Error("REDIS: Created RedisAccumulator wrapper for capturing state changes", 
		"blockNum", blockNum,
		"client", redisClient != nil)
		
	fmt.Printf("\n\n!!! REDIS ACCUMULATOR CREATED !!!\n\n")
		
	return redisAcc
}

// GetRedisIntegrationFromAccumulator extracts the Redis integration from a RedisAccumulator
// This is used by the executor to get access to the Redis client and block processor
func GetRedisIntegrationFromAccumulator(acc *RedisAccumulator) (*RedisIntegration, error) {
	if acc == nil {
		return nil, fmt.Errorf("nil accumulator")
	}
	
	// Create a minimal Redis integration with just the client and block processor
	integration := &RedisIntegration{
		client:         acc.redisClient,
		logger:         acc.logger,
		blockProcessor: NewBlockHeaderProcessor(acc.redisClient, acc.logger),
	}
	
	return integration, nil
}

//
// Delegation methods for shards.Accumulator with Redis additions
//

// Reset delegates to the wrapped accumulator
func (ra *RedisAccumulator) Reset(plainStateID uint64) {
	ra.acc.Reset(plainStateID)
}

// SendAndReset delegates to the wrapped accumulator
func (ra *RedisAccumulator) SendAndReset(ctx context.Context, c shards.StateChangeConsumer, pendingBaseFee uint64, pendingBlobFee uint64, blockGasLimit uint64, finalizedBlock uint64) {
	ra.acc.SendAndReset(ctx, c, pendingBaseFee, pendingBlobFee, blockGasLimit, finalizedBlock)
}

// SetStateID delegates to the wrapped accumulator
func (ra *RedisAccumulator) SetStateID(stateID uint64) {
	ra.acc.SetStateID(stateID)
}

// StartChange delegates to the wrapped accumulator
func (ra *RedisAccumulator) StartChange(blockHeight uint64, blockHash libcommon.Hash, txs [][]byte, unwind bool) {
	ra.acc.StartChange(blockHeight, blockHash, txs, unwind)
}

// ChangeAccount overrides the accumulator's ChangeAccount method
func (ra *RedisAccumulator) ChangeAccount(address libcommon.Address, incarnation uint64, data []byte) {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	
	// Super aggressive logging to make sure we see this
	fmt.Printf("\n\n!!! REDIS ACCOUNT CHANGE DETECTED: %s !!!\n\n", address.Hex())
	
	// Log that this method was called - this confirms our wrapper is active
	ra.logger.Error("REDIS DEBUG: RedisAccumulator.ChangeAccount called", 
		"address", address.Hex(), 
		"incarnation", incarnation,
		"dataSize", len(data))
	
	// CRITICAL: Store account change directly in Redis first, before anything else
	// This ensures we don't miss any account changes even if there's an error elsewhere
	if len(data) > 0 {
		ctx, cancel := context.WithTimeout(ra.ctx, 5*time.Second)
		defer cancel()
		
		// Store the raw account data as a separate record for safety
		rawAccountKey := fmt.Sprintf("account_raw:%s", address.Hex())
		if err := ra.redisClient.Set(ctx, rawAccountKey, data, 0).Err(); err != nil { // Store permanently
			ra.logger.Error("CRITICAL: Failed to store raw account data in Redis", 
				"address", address.Hex(), 
				"err", err)
		} else {
			ra.logger.Error("REDIS: Stored raw account data", "address", address.Hex())
		}
	}
	
	// Increment counter for debugging
	ra.accountChangeCount++
	
	// Call the delegated method
	ra.acc.ChangeAccount(address, incarnation, data)

	// Mirror to Redis with proper account format - this is the main account data store
	var account accounts.Account
	if err := accounts.DeserialiseV3(&account, data); err == nil {
		// Create a reader to get the original account data if available
		reader := NewRedisStateReaderWithLogger(ra.redisClient, ra.logger)
		original, err := reader.ReadAccountData(address)
		if err != nil {
			ra.logger.Debug("Could not read original account data, creating new", 
				"address", address, 
				"err", err)
			// Continue with nil original, not a critical error
		}

		// Store basic account info for quick access - critical info like balance
		ctx, cancel := context.WithTimeout(ra.ctx, 5*time.Second)
		defer cancel()
		
		// Store account summary for fast access
		accountSummary := map[string]string{
			"address":  address.Hex(),
			"balance":  account.Balance.Hex(),
			"nonce":    fmt.Sprintf("%d", account.Nonce),
			"codeHash": account.CodeHash.Hex(),
			"blockNum": fmt.Sprintf("%d", ra.redisWriter.blockNum),
		}
		
		// Use a hash for this account's fast lookup data
		summaryKey := fmt.Sprintf("account_summary:%s", address.Hex())
		if err := ra.redisClient.HMSet(ctx, summaryKey, accountSummary).Err(); err != nil {
			ra.logger.Error("Failed to store account summary", 
				"address", address.Hex(), 
				"err", err)
		} else {
			ra.logger.Error("REDIS: Stored account summary", "address", address.Hex())
			fmt.Printf("\n\n!!! REDIS STORED ACCOUNT: %s, BALANCE: %s !!!\n\n", 
				address.Hex(), account.Balance.Hex())
		}

		// Proceed with the update to the proper format
		if err := ra.redisWriter.UpdateAccountData(address, original, &account); err != nil {
			ra.logger.Error("Failed to write account to Redis", 
				"address", address, 
				"block", ra.redisWriter.blockNum,
				"err", err)
		} else {
			ra.logger.Error("REDIS: Account successfully updated", 
				"address", address, 
				"block", ra.redisWriter.blockNum,
				"balance", account.Balance.Hex())
		}
	} else {
		ra.logger.Error("Failed to deserialize account", "address", address, "err", err)
	}
}

// ChangeStorage overrides the accumulator's ChangeStorage method
func (ra *RedisAccumulator) ChangeStorage(address libcommon.Address, incarnation uint64, location libcommon.Hash, data []byte) {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	
	// Super aggressive logging
	fmt.Printf("\n\n!!! REDIS STORAGE CHANGE DETECTED: %s, slot: %s !!!\n\n", address.Hex(), location.Hex())
	
	// Log that this method was called
	ra.logger.Error("REDIS DEBUG: RedisAccumulator.ChangeStorage called", 
		"address", address.Hex(), 
		"location", location.Hex(),
		"incarnation", incarnation,
		"dataSize", len(data))
	
	// CRITICAL: Store storage change directly in Redis first, before anything else
	// This ensures we don't miss any storage changes even if there's an error elsewhere
	if len(data) > 0 {
		ctx, cancel := context.WithTimeout(ra.ctx, 5*time.Second)
		defer cancel()
		
		// Store the raw storage data as a separate record for safety
		rawStorageKey := fmt.Sprintf("storage_raw:%s:%s", address.Hex(), location.Hex())
		if err := ra.redisClient.Set(ctx, rawStorageKey, data, 0).Err(); err != nil { // Store permanently
			ra.logger.Error("CRITICAL: Failed to store raw storage data in Redis", 
				"address", address.Hex(), 
				"location", location.Hex(),
				"err", err)
		} else {
			ra.logger.Error("REDIS: Stored raw storage data", 
				"address", address.Hex(), 
				"location", location.Hex())
		}
		
		// Also save a reference to this storage slot in a set for this account
		// This helps with enumeration of all storage keys for an account
		storageKeysSet := fmt.Sprintf("account:%s:storage_keys", address.Hex())
		if err := ra.redisClient.SAdd(ctx, storageKeysSet, location.Hex()).Err(); err != nil {
			ra.logger.Error("Failed to add storage key to account's storage keys set", 
				"address", address.Hex(), 
				"location", location.Hex(),
				"err", err)
		}
	}
	
	// Call delegated method
	ra.acc.ChangeStorage(address, incarnation, location, data)

	// Mirror to Redis with proper storage format
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

	// Store storage value summary for fast access
	ctx, cancel := context.WithTimeout(ra.ctx, 5*time.Second)
	defer cancel()
	
	// Store storage summary for fast access using a hash
	storageSummaryKey := fmt.Sprintf("storage_summary:%s:%s", address.Hex(), location.Hex())
	storageSummary := map[string]string{
		"address":  address.Hex(),
		"slot":     location.Hex(),
		"value":    value.Hex(),
		"blockNum": fmt.Sprintf("%d", ra.redisWriter.blockNum),
	}
	
	if err := ra.redisClient.HMSet(ctx, storageSummaryKey, storageSummary).Err(); err != nil {
		ra.logger.Error("Failed to store storage summary", 
			"address", address.Hex(), 
			"location", location.Hex(),
			"err", err)
	} else {
		ra.logger.Error("REDIS: Stored storage summary", 
			"address", address.Hex(), 
			"location", location.Hex())
		fmt.Printf("\n\n!!! REDIS STORED STORAGE: %s:%s = %s !!!\n\n", 
			address.Hex(), location.Hex(), value.Hex())
	}

	// Write the storage change to Redis in the proper format
	if err := ra.redisWriter.WriteAccountStorage(address, incarnation, &location, originalValue, value); err != nil {
		ra.logger.Error("Failed to write storage to Redis", 
			"address", address, 
			"location", location.Hex(), 
			"block", ra.redisWriter.blockNum,
			"err", err)
	} else {
		ra.logger.Error("REDIS: Storage successfully updated", 
			"address", address, 
			"location", location.Hex(), 
			"block", ra.redisWriter.blockNum,
			"value", value.Hex())
	}
}

// ChangeCode overrides the accumulator's ChangeCode method
func (ra *RedisAccumulator) ChangeCode(address libcommon.Address, incarnation uint64, code []byte) {
	ra.mu.Lock()
	defer ra.mu.Unlock()

	// Log this call
	ra.logger.Error("REDIS DEBUG: RedisAccumulator.ChangeCode called",
		"address", address.Hex(),
		"incarnation", incarnation,
		"codeSize", len(code))
	
	fmt.Printf("\n\n!!! REDIS CODE CHANGE DETECTED: %s !!!\n\n", address.Hex())
	
	// CRITICAL: Store code directly in Redis first, before anything else
	// This ensures we don't miss any code changes even if there's an error elsewhere
	if len(code) > 0 {
		ctx, cancel := context.WithTimeout(ra.ctx, 5*time.Second)
		defer cancel()
		
		// Calculate code hash
		codeHash := libcommon.BytesToHash(crypto.Keccak256(code))
		
		// Store the raw code as a separate record for safety
		rawCodeKey := fmt.Sprintf("code_raw:%s", address.Hex())
		if err := ra.redisClient.Set(ctx, rawCodeKey, code, 0).Err(); err != nil { // Store permanently
			ra.logger.Error("CRITICAL: Failed to store raw code in Redis", 
				"address", address.Hex(), 
				"codeSize", len(code),
				"err", err)
		} else {
			ra.logger.Error("REDIS: Stored raw code", 
				"address", address.Hex(), 
				"codeSize", len(code))
		}
		
		// Store a code summary for quick access
		codeSummaryKey := fmt.Sprintf("code_summary:%s", address.Hex())
		codeSummary := map[string]string{
			"address":    address.Hex(),
			"codeHash":   codeHash.Hex(),
			"codeSize":   fmt.Sprintf("%d", len(code)),
			"incarnation": fmt.Sprintf("%d", incarnation),
			"blockNum":   fmt.Sprintf("%d", ra.redisWriter.blockNum),
		}
		
		if err := ra.redisClient.HMSet(ctx, codeSummaryKey, codeSummary).Err(); err != nil {
			ra.logger.Error("Failed to store code summary", 
				"address", address.Hex(), 
				"codeSize", len(code),
				"err", err)
		} else {
			ra.logger.Error("REDIS: Stored code summary", 
				"address", address.Hex(), 
				"codeSize", len(code))
			fmt.Printf("\n\n!!! REDIS STORED CODE: %s, SIZE: %d !!!\n\n", 
				address.Hex(), len(code))
		}
		
		// Store all contracts in a set for easy enumeration
		if err := ra.redisClient.SAdd(ctx, "contracts", address.Hex()).Err(); err != nil {
			ra.logger.Error("Failed to add contract to contracts set", 
				"address", address.Hex(),
				"err", err)
		}
	}
	
	// Call delegated method
	ra.acc.ChangeCode(address, incarnation, code)

	// Skip empty code
	if len(code) == 0 {
		return
	}

	// Calculate code hash
	codeHash := libcommon.BytesToHash(crypto.Keccak256(code))

	// Mirror to Redis with proper format
	if err := ra.redisWriter.UpdateAccountCode(address, incarnation, codeHash, code); err != nil {
		ra.logger.Error("Failed to write code to Redis", 
			"address", address, 
			"codeHash", codeHash.Hex(), 
			"codeSize", len(code), 
			"block", ra.redisWriter.blockNum,
			"err", err)
	} else {
		ra.logger.Error("REDIS: Code successfully updated", 
			"address", address, 
			"codeHash", codeHash.Hex(), 
			"codeSize", len(code), 
			"block", ra.redisWriter.blockNum)
	}
}

// DeleteAccount overrides the accumulator's DeleteAccount method
func (ra *RedisAccumulator) DeleteAccount(address libcommon.Address) {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	
	// Log this call
	ra.logger.Error("REDIS DEBUG: RedisAccumulator.DeleteAccount called",
		"address", address.Hex())
		
	fmt.Printf("\n\n!!! REDIS ACCOUNT DELETE DETECTED: %s !!!\n\n", address.Hex())
	
	// CRITICAL: Store account deletion directly in Redis first
	// Mark the account as deleted in our summary data
	ctx, cancel := context.WithTimeout(ra.ctx, 5*time.Second)
	defer cancel()
	
	// Store the account deletion event
	deletionKey := fmt.Sprintf("account_deleted:%s", address.Hex())
	deletionValue := fmt.Sprintf("Deleted at block %d, time %s", ra.redisWriter.blockNum, time.Now().Format(time.RFC3339))
	if err := ra.redisClient.Set(ctx, deletionKey, deletionValue, 0).Err(); err != nil { // Store permanently
		ra.logger.Error("CRITICAL: Failed to store account deletion in Redis", 
			"address", address.Hex(), 
			"err", err)
	} else {
		ra.logger.Error("REDIS: Stored account deletion", 
			"address", address.Hex())
	}
	
	// Update the account summary to mark it as deleted
	summaryKey := fmt.Sprintf("account_summary:%s", address.Hex())
	if err := ra.redisClient.HSet(ctx, summaryKey, "deleted", "true", "deleted_at_block", fmt.Sprintf("%d", ra.redisWriter.blockNum)).Err(); err != nil {
		ra.logger.Error("Failed to update account summary for deletion", 
			"address", address.Hex(), 
			"err", err)
	} else {
		ra.logger.Error("REDIS: Updated account summary for deletion", 
			"address", address.Hex())
		fmt.Printf("\n\n!!! REDIS STORED ACCOUNT DELETION: %s !!!\n\n", 
			address.Hex())
	}
	
	// Get the original account before deletion if possible
	reader := NewRedisStateReaderWithLogger(ra.redisClient, ra.logger)
	original, err := reader.ReadAccountData(address)
	if err != nil {
		ra.logger.Debug("Could not read original account before deletion", "address", address, "err", err)
		// Continue with nil original, not a critical error
	}
	
	// Call delegated method
	ra.acc.DeleteAccount(address)

	// Mirror to Redis with the original account data using proper format
	if err := ra.redisWriter.DeleteAccount(address, original); err != nil {
		ra.logger.Error("Failed to delete account in Redis", 
			"address", address, 
			"block", ra.redisWriter.blockNum,
			"err", err)
	} else {
		ra.logger.Error("REDIS: Account successfully deleted", 
			"address", address, 
			"block", ra.redisWriter.blockNum)
	}
}
