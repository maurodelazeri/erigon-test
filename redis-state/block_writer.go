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

	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/log/v3"
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

// WriteBlockHeader writes a block header to Redis
func (w *RedisBlockWriter) WriteBlockHeader(blockNum uint64, blockHash libcommon.Hash, header []byte) error {
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	
	// Start a pipeline for better performance
	pipe := w.client.Pipeline()
	
	// Store block header
	blockKey := fmt.Sprintf("block:%d", blockNum)
	pipe.HSet(ctx, blockKey, "header", header)
	
	// Store mapping from hash to number
	hashKey := fmt.Sprintf("blockHash:%s", blockHash.Hex())
	pipe.Set(ctx, hashKey, blockNum, 0)
	
	// Update current block pointer
	pipe.Set(ctx, "currentBlock", blockNum, 0)
	
	// Execute the pipeline
	cmds, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to write block header in Redis pipeline: %w", err)
	}
	
	// Check individual command results
	for i, cmd := range cmds {
		if cmd.Err() != nil {
			w.logger.Warn("Error in Redis pipeline command", "index", i, "err", cmd.Err())
			return fmt.Errorf("command %d failed in block header pipeline: %w", i, cmd.Err())
		}
	}
	
	w.logger.Debug("Block header written to Redis", "number", blockNum, "hash", blockHash.Hex())
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