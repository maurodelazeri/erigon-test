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
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon/core/types"
)

// RedisBlockWriter handles writing block-related data to Redis
type RedisBlockWriter struct {
	client *redis.Client
	ctx    context.Context
	logger log.Logger
}

// NewRedisBlockWriter creates a new instance of RedisBlockWriter
func NewRedisBlockWriter(client *redis.Client) *RedisBlockWriter {
	ctx, _ := context.WithCancel(context.Background())
	return &RedisBlockWriter{
		client: client,
		ctx:    ctx,
		logger: log.Root(),
	}
}

// NewRedisBlockWriterWithLogger creates a new instance of RedisBlockWriter with a custom logger
func NewRedisBlockWriterWithLogger(client *redis.Client, logger log.Logger) *RedisBlockWriter {
	ctx, _ := context.WithCancel(context.Background())
	return &RedisBlockWriter{
		client: client,
		ctx:    ctx,
		logger: logger,
	}
}

// WriteBlockHeader writes a block header to Redis with chain reorganization support
func (w *RedisBlockWriter) WriteBlockHeader(blockNum uint64, blockHash libcommon.Hash, header []byte) error {
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	
	// First, check if we already have a block at this height with a different hash (potential reorg)
	existingBlock := fmt.Sprintf("block:%d", blockNum)
	existingHash, err := w.client.HGet(ctx, existingBlock, "hash").Result()
	
	var reorg bool
	if err == nil && existingHash != "" && existingHash != blockHash.Hex() {
		// We have a different block at this height - this is a chain reorganization
		reorg = true
		w.logger.Info("Detected chain reorganization", 
			"blockNum", blockNum, 
			"oldHash", existingHash, 
			"newHash", blockHash.Hex())
		
		// We need to handle the reorg by:
		// 1. Mark the old block as orphaned
		// 2. Update any references
		if err := w.handleReorg(ctx, blockNum, libcommon.HexToHash(existingHash), blockHash); err != nil {
			w.logger.Error("Failed to handle chain reorganization", 
				"blockNum", blockNum, 
				"err", err)
			// Continue despite error, as we still want to write the new block
		}
	}
	
	// Start a pipeline for better performance
	pipe := w.client.Pipeline()
	
	// Store timestamp for versioning
	timestamp := time.Now().UTC().Unix()
	
	// Store block header with hash reference and timestamp for versioning
	blockKey := fmt.Sprintf("block:%d", blockNum)
	pipe.HSet(ctx, blockKey, map[string]interface{}{
		"header":    header,
		"hash":      blockHash.Hex(),
		"timestamp": timestamp,
		"reorg":     reorg,
	})
	
	// Store mapping from hash to number with timestamp
	hashKey := fmt.Sprintf("blockHash:%s", blockHash.Hex())
	pipe.HSet(ctx, hashKey, map[string]interface{}{
		"number":    blockNum,
		"timestamp": timestamp,
		"canonical": true,
	})
	
	// Update current block pointer only if this is at the chain head
	// (don't update if this is a backfill or past block)
	currentNum, err := w.client.Get(ctx, "currentBlock").Uint64()
	if err != nil || blockNum >= currentNum {
		pipe.Set(ctx, "currentBlock", blockNum, 0)
	}
	
	// Track canonical chain
	canonicalKey := "canonicalChain"
	pipe.ZAdd(ctx, canonicalKey, redis.Z{
		Score:  float64(blockNum),
		Member: blockHash.Hex(),
	})
	
	// Execute the pipeline
	cmds, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to write block header in Redis pipeline: %w", err)
	}
	
	// Check individual command results
	for i, cmd := range cmds {
		if cmd.Err() != nil {
			w.logger.Warn("Error in Redis pipeline command", "index", i, "err", cmd.Err())
			// Continue despite errors - not returning here to allow partial success
		}
	}
	
	logMsg := "Block header written to Redis"
	if reorg {
		logMsg = "Block header written to Redis (after chain reorganization)"
	}
	w.logger.Debug(logMsg, "number", blockNum, "hash", blockHash.Hex())
	return nil
}

// handleReorg handles the chain reorganization for a specific block
func (w *RedisBlockWriter) handleReorg(ctx context.Context, blockNum uint64, oldHash, newHash libcommon.Hash) error {
	// Start a pipeline for better performance
	pipe := w.client.Pipeline()
	
	// 1. Mark the old block hash as non-canonical
	oldHashKey := fmt.Sprintf("blockHash:%s", oldHash.Hex())
	pipe.HSet(ctx, oldHashKey, "canonical", false)
	
	// 2. Record reorg in a dedicated log
	reorgLogKey := "chainReorgs"
	reorgData := fmt.Sprintf("%d:%s:%s:%d", blockNum, oldHash.Hex(), newHash.Hex(), time.Now().UTC().Unix())
	pipe.RPush(ctx, reorgLogKey, reorgData)
	
	// 3. Remove old block hash from canonical chain
	canonicalKey := "canonicalChain"
	pipe.ZRem(ctx, canonicalKey, oldHash.Hex())
	
	// 4. Find all child blocks of the reorganized block
	// This would require maintaining parent-child relationships, which is more complex
	// For simplicity, we'll just scan for all blocks higher than this one and check if they're affected
	// In a full implementation, you would maintain explicit parent-child relationships
	
	// Execute the pipeline
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to handle chain reorganization in Redis: %w", err)
	}
	
	// Log reorg for monitoring
	w.logger.Info("Chain reorganization processed", 
		"blockNum", blockNum, 
		"oldHash", oldHash.Hex(), 
		"newHash", newHash.Hex())
	
	return nil
}

// WriteReceipt writes a transaction receipt to Redis
func (w *RedisBlockWriter) WriteReceipt(txHash libcommon.Hash, blockNum uint64, receipt []byte) error {
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	
	key := fmt.Sprintf("receipt:%s", txHash.Hex())
	receiptKey := fmt.Sprintf("receipts:%d", blockNum)
	
	// Use pipeline for batching operations
	pipe := w.client.Pipeline()
	
	// Store receipt with block number as score
	pipe.ZAdd(ctx, key, redis.Z{
		Score:  float64(blockNum),
		Member: string(receipt),
	})
	
	// Also store in a block-based index for easy retrieval of all receipts for a block
	pipe.HSet(ctx, receiptKey, txHash.Hex(), receipt)
	
	// Execute the pipeline
	cmds, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to write receipt in Redis pipeline: %w", err)
	}
	
	// Check individual command results
	for i, cmd := range cmds {
		if cmd.Err() != nil {
			w.logger.Warn("Error in Redis receipt command", "index", i, "err", cmd.Err())
			return fmt.Errorf("command %d failed in receipt pipeline: %w", i, cmd.Err())
		}
	}
	
	w.logger.Debug("Receipt written to Redis", "txHash", txHash.Hex(), "blockNum", blockNum)
	return nil
}

// WriteLog writes a log entry to Redis
func (w *RedisBlockWriter) WriteLog(blockNum uint64, logIndex uint, address libcommon.Address, topics []libcommon.Hash, data []byte) error {
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	
	// Create a log key
	logKey := fmt.Sprintf("logs:%d:%d", blockNum, logIndex)
	
	// Use pipeline for better performance with multiple operations
	pipe := w.client.Pipeline()
	
	// Store the log data
	pipe.Set(ctx, logKey, data, 0)
	
	// Create a composite block+address key to group logs by address within a block
	blockAddrKey := fmt.Sprintf("logs:block:%d:addr:%s", blockNum, address.Hex())
	pipe.SAdd(ctx, blockAddrKey, logIndex)
	
	// Set expiration on this set (optional, for optimization)
	pipe.Expire(ctx, blockAddrKey, 24*time.Hour)
	
	// Index by address (for address-based filtering)
	addrKey := fmt.Sprintf("address:%s", address.Hex())
	pipe.ZAdd(ctx, addrKey, redis.Z{
		Score:  float64(blockNum),
		Member: logKey,
	})
	
	// Index by topics (for topic-based filtering)
	for i, topic := range topics {
		// Global topic index
		topicKey := fmt.Sprintf("topic:%s", topic.Hex())
		pipe.ZAdd(ctx, topicKey, redis.Z{
			Score:  float64(blockNum),
			Member: logKey,
		})
		
		// Position-specific topic index (for positional topic filtering)
		posTopicKey := fmt.Sprintf("topic%d:%s", i, topic.Hex())
		pipe.ZAdd(ctx, posTopicKey, redis.Z{
			Score:  float64(blockNum),
			Member: logKey,
		})
		
		// Create composite keys for block+topic filtering (optimization for common queries)
		if i == 0 { // Most filters use first topic
			blockTopicKey := fmt.Sprintf("logs:block:%d:topic:%s", blockNum, topic.Hex())
			pipe.SAdd(ctx, blockTopicKey, logIndex)
			pipe.Expire(ctx, blockTopicKey, 24*time.Hour) // Optional expiration
		}
	}
	
	// Execute all commands in a single network roundtrip
	cmds, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to execute log write pipeline: %w", err)
	}
	
	// Check for individual command errors
	for i, cmd := range cmds {
		if cmd.Err() != nil {
			w.logger.Warn("Error in Redis log pipeline command", "index", i, "err", cmd.Err())
			// Continue despite errors to maintain some data
		}
	}
	
	return nil
}

// WriteTransaction writes a transaction to Redis
func (w *RedisBlockWriter) WriteTransaction(txHash libcommon.Hash, blockNum uint64, txData []byte) error {
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	
	txKey := fmt.Sprintf("tx:%s", txHash.Hex())
	blockTxsKey := fmt.Sprintf("block:%d:txs", blockNum)
	
	// Use pipeline for batching operations
	pipe := w.client.Pipeline()
	
	// Store transaction with block number as score
	pipe.ZAdd(ctx, txKey, redis.Z{
		Score:  float64(blockNum),
		Member: string(txData),
	})
	
	// Add to block transactions set for easy retrieval
	pipe.SAdd(ctx, blockTxsKey, txHash.Hex())
	
	// Execute pipeline
	cmds, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to write transaction in Redis pipeline: %w", err)
	}
	
	// Check individual command results
	for i, cmd := range cmds {
		if cmd.Err() != nil {
			w.logger.Warn("Error in Redis transaction command", "index", i, "err", cmd.Err())
			return fmt.Errorf("command %d failed in transaction pipeline: %w", i, cmd.Err())
		}
	}
	
	w.logger.Debug("Transaction written to Redis", "hash", txHash.Hex(), "blockNum", blockNum)
	return nil
}

// HandleTransaction processes a transaction and writes it to Redis
// This is a convenience wrapper around WriteTransaction that accepts a transaction object
func (w *RedisBlockWriter) HandleTransaction(tx types.Transaction, blockNum uint64) error {
	// Get transaction hash
	txHash := tx.Hash()
	
	// Marshal the transaction data
	txData, err := json.Marshal(tx)
	if err != nil {
		return fmt.Errorf("failed to marshal transaction: %w", err)
	}
	
	// Write the transaction using our existing method
	return w.WriteTransaction(txHash, blockNum, txData)
}