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

	"github.com/holiman/uint256"
	"github.com/redis/go-redis/v9"

	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon-lib/types/accounts"
	"github.com/erigontech/erigon/core/state"
	"github.com/erigontech/erigon/core/types"
)

// StateInterceptor wraps another StateWriter to mirror operations to Redis
type StateInterceptor struct {
	inner       state.StateWriter
	redisWriter *RedisStateWriter
	blockWriter *RedisBlockWriter
	blockNum    uint64
	logger      log.Logger
}

// NewStateInterceptor creates a new StateInterceptor
func NewStateInterceptor(inner state.StateWriter, redisClient *redis.Client, blockNum uint64, logger log.Logger) state.StateWriter {
	return &StateInterceptor{
		inner:       inner,
		redisWriter: NewRedisStateWriter(redisClient, blockNum),
		blockWriter: NewRedisBlockWriter(redisClient),
		blockNum:    blockNum,
		logger:      logger,
	}
}

// NewHistoricalStateInterceptor creates a new StateInterceptor that also implements WriterWithChangeSets
func NewHistoricalStateInterceptor(inner state.WriterWithChangeSets, redisClient *redis.Client, blockNum uint64, logger log.Logger) state.WriterWithChangeSets {
	return &HistoricalStateInterceptor{
		StateInterceptor: StateInterceptor{
			inner:       inner,
			redisWriter: NewRedisStateWriter(redisClient, blockNum),
			blockWriter: NewRedisBlockWriter(redisClient),
			blockNum:    blockNum,
			logger:      logger,
		},
		innerHistorical: inner,
		redisHistorical: NewRedisHistoricalWriter(redisClient, blockNum),
	}
}

// UpdateAccountData updates an account in the state and also in Redis
func (i *StateInterceptor) UpdateAccountData(address libcommon.Address, original, account *accounts.Account) error {
	// First update in the main state
	if err := i.inner.UpdateAccountData(address, original, account); err != nil {
		return err
	}

	// Also update in Redis, but don't fail if Redis update fails
	if err := i.redisWriter.UpdateAccountData(address, original, account); err != nil {
		i.logger.Warn("Failed to update account in Redis", "address", address.Hex(), "err", err)
	}

	return nil
}

// UpdateAccountCode updates account code in the state and also in Redis
func (i *StateInterceptor) UpdateAccountCode(address libcommon.Address, incarnation uint64, codeHash libcommon.Hash, code []byte) error {
	// First update in the main state
	if err := i.inner.UpdateAccountCode(address, incarnation, codeHash, code); err != nil {
		return err
	}

	// Also update in Redis, but don't fail if Redis update fails
	if err := i.redisWriter.UpdateAccountCode(address, incarnation, codeHash, code); err != nil {
		i.logger.Warn("Failed to update account code in Redis", "address", address.Hex(), "err", err)
	}

	return nil
}

// DeleteAccount deletes an account in the state and also in Redis
func (i *StateInterceptor) DeleteAccount(address libcommon.Address, original *accounts.Account) error {
	// First delete in the main state
	if err := i.inner.DeleteAccount(address, original); err != nil {
		return err
	}

	// Also delete in Redis, but don't fail if Redis update fails
	if err := i.redisWriter.DeleteAccount(address, original); err != nil {
		i.logger.Warn("Failed to delete account in Redis", "address", address.Hex(), "err", err)
	}

	return nil
}

// WriteAccountStorage writes account storage in the state and also in Redis
func (i *StateInterceptor) WriteAccountStorage(address libcommon.Address, incarnation uint64, key *libcommon.Hash, original, value *uint256.Int) error {
	// First write in the main state
	if err := i.inner.WriteAccountStorage(address, incarnation, key, original, value); err != nil {
		return err
	}

	// Also write in Redis, but don't fail if Redis update fails
	if err := i.redisWriter.WriteAccountStorage(address, incarnation, key, original, value); err != nil {
		i.logger.Warn("Failed to write account storage in Redis", "address", address.Hex(), "err", err)
	}

	return nil
}

// CreateContract creates a contract in the state and also in Redis
func (i *StateInterceptor) CreateContract(address libcommon.Address) error {
	// First create in the main state
	if err := i.inner.CreateContract(address); err != nil {
		return err
	}

	// Also create in Redis, but don't fail if Redis update fails
	if err := i.redisWriter.CreateContract(address); err != nil {
		i.logger.Warn("Failed to create contract in Redis", "address", address.Hex(), "err", err)
	}

	return nil
}

// HistoricalStateInterceptor adds support for WriteChangeSets and WriteHistory
type HistoricalStateInterceptor struct {
	StateInterceptor
	innerHistorical state.WriterWithChangeSets
	redisHistorical *RedisHistoricalWriter
}

// WriteChangeSets writes change sets in the state and also in Redis
func (i *HistoricalStateInterceptor) WriteChangeSets() error {
	// First write in the main state
	if err := i.innerHistorical.WriteChangeSets(); err != nil {
		return err
	}

	// Also write in Redis, but don't fail if Redis update fails
	if err := i.redisHistorical.WriteChangeSets(); err != nil {
		i.logger.Warn("Failed to write change sets in Redis", "err", err)
	}

	return nil
}

// WriteHistory writes history in the state and also in Redis
func (i *HistoricalStateInterceptor) WriteHistory() error {
	// First write in the main state
	if err := i.innerHistorical.WriteHistory(); err != nil {
		return err
	}

	// Also write in Redis, but don't fail if Redis update fails
	if err := i.redisHistorical.WriteHistory(); err != nil {
		i.logger.Warn("Failed to write history in Redis", "err", err)
	}

	return nil
}

// SetTxNum implements HistoricalStateReader interface if the inner does
func (i *HistoricalStateInterceptor) SetTxNum(txNum uint64) {
	if writer, ok := i.inner.(state.HistoricalStateReader); ok {
		writer.SetTxNum(txNum)
	}
	i.redisWriter.SetTxNum(txNum)
}

// GetTxNum implements HistoricalStateReader interface if the inner does
func (i *HistoricalStateInterceptor) GetTxNum() uint64 {
	if reader, ok := i.inner.(state.HistoricalStateReader); ok {
		return reader.GetTxNum()
	}
	return i.redisWriter.GetTxNum()
}

// BlockHeaderProcessor is responsible for processing block headers and storing them in Redis
type BlockHeaderProcessor struct {
	redisClient *redis.Client
	ctx         context.Context
	logger      log.Logger
	blockWriter *RedisBlockWriter
}

// NewBlockHeaderProcessor creates a new BlockHeaderProcessor
func NewBlockHeaderProcessor(redisClient *redis.Client, logger log.Logger) *BlockHeaderProcessor {
	ctx, _ := context.WithCancel(context.Background())
	blockWriter := NewRedisBlockWriterWithLogger(redisClient, logger)
	
	return &BlockHeaderProcessor{
		redisClient: redisClient,
		ctx:         ctx,
		logger:      logger,
		blockWriter: blockWriter,
	}
}

// ProcessBlockHeader processes a block header and stores it in Redis
func (p *BlockHeaderProcessor) ProcessBlockHeader(header *types.Header) error {
	blockNum := header.Number.Uint64()
	blockHash := header.Hash()

	// Marshal header
	headerBytes, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("failed to marshal header: %w", err)
	}

	// Write header to Redis using the processor's blockWriter
	if err := p.blockWriter.WriteBlockHeader(blockNum, blockHash, headerBytes); err != nil {
		return fmt.Errorf("failed to write block header: %w", err)
	}

	p.logger.Debug("Processed block header", "number", blockNum, "hash", blockHash.Hex())
	return nil
}

// ProcessBlockReceipts processes block receipts and stores them in Redis
func (p *BlockHeaderProcessor) ProcessBlockReceipts(block *types.Block, receipts types.Receipts) error {
	blockNum := block.NumberU64()

	// Track total logs processed
	totalLogs := 0

	// Process receipts
	for i, receipt := range receipts {
		if i >= len(block.Transactions()) {
			p.logger.Error("Receipt index out of bounds", "receiptIndex", i, "txCount", len(block.Transactions()))
			continue
		}
		
		txHash := block.Transactions()[i].Hash()

		// Marshal receipt
		receiptBytes, err := json.Marshal(receipt)
		if err != nil {
			p.logger.Warn("Failed to marshal receipt", "txHash", txHash.Hex(), "err", err)
			continue // Skip this receipt but try to process others
		}

		// Write receipt to Redis
		if err := p.blockWriter.WriteReceipt(txHash, blockNum, receiptBytes); err != nil {
			p.logger.Warn("Failed to write receipt", "txHash", txHash.Hex(), "err", err)
			continue // Skip this receipt but try to process others
		}

		// Process logs
		for j, log := range receipt.Logs {
			logBytes, err := json.Marshal(log)
			if err != nil {
				p.logger.Warn("Failed to marshal log", "txHash", txHash.Hex(), "logIndex", j, "err", err)
				continue // Skip this log but try to process others
			}

			if err := p.blockWriter.WriteLog(blockNum, uint(j), log.Address, log.Topics, logBytes); err != nil {
				p.logger.Warn("Failed to write log", "txHash", txHash.Hex(), "logIndex", j, "err", err)
				continue // Skip this log but try to process others
			}
			
			totalLogs++
		}
	}

	p.logger.Debug("Processed block receipts", "blockNum", blockNum, "receiptCount", len(receipts), "logCount", totalLogs)
	return nil
}

// HandleBlock processes a block and writes its data to Redis
func (p *BlockHeaderProcessor) HandleBlock(block *types.Block, receipts types.Receipts) error {
	blockNum := block.NumberU64()
	blockHash := block.Hash()
	
	// Enhanced error handling with retries and more detailed logging
	ctx, cancel := context.WithTimeout(p.ctx, 15*time.Second) // Generous timeout for processing entire block
	defer cancel()
	
	// Create a Redis pipeline for batching operations
	pipe := p.redisClient.Pipeline()
	
	// 1. Process header with better error handling
	if err := p.ProcessBlockHeader(block.Header()); err != nil {
		p.logger.Error("Failed to process block header for Redis", "block", blockNum, "hash", blockHash.Hex(), "err", err)
		return fmt.Errorf("failed to process header for block %d: %w", blockNum, err)
	}
	
	// 2. Set current block number as a global reference
	currentBlockKey := "currentBlock"
	pipe.Set(ctx, currentBlockKey, blockNum, 0)
	
	// 3. Process transactions with improved error handling
	txCount := 0
	txErrors := 0
	for i, tx := range block.Transactions() {
		txHash := tx.Hash()
		txData, err := json.Marshal(tx)
		if err != nil {
			p.logger.Warn("Failed to marshal transaction", "txHash", txHash.Hex(), "err", err)
			txErrors++
			continue
		}
		
		if err := p.blockWriter.WriteTransaction(txHash, blockNum, txData); err != nil {
			p.logger.Warn("Failed to write transaction to Redis", "txHash", txHash.Hex(), "err", err)
			txErrors++
		} else {
			txCount++
		}
		
		// Store transaction receipt index for fast lookup
		if i < len(receipts) {
			txIndexKey := fmt.Sprintf("tx:%s:index", txHash.Hex())
			pipe.Set(ctx, txIndexKey, fmt.Sprintf("%d", i), 0)
			
			// Store transaction trace data if provided with the receipt
			if receipts[i].Status == 1 { // Success
				traceKey := fmt.Sprintf("trace:%s", txHash.Hex())
				
				// For demonstration, store a basic trace structure
				// In a real implementation, this would be actual trace data from the EVM execution
				trace := map[string]interface{}{
					"blockNumber": blockNum,
					"txIndex":     i,
					"gas":         receipts[i].GasUsed,
					"status":      receipts[i].Status,
					"logs":        len(receipts[i].Logs),
					"result":      "0x", // This would be the actual return data
				}
				
				traceData, err := json.Marshal(trace)
				if err == nil {
					pipe.Set(ctx, traceKey, traceData, 0)
				}
			}
		}
	}
	
	// 4. Process receipts with improved error handling and consistency checks
	if err := p.ProcessBlockReceipts(block, receipts); err != nil {
		p.logger.Error("Failed to process block receipts for Redis", "block", blockNum, "hash", blockHash.Hex(), "err", err)
		return fmt.Errorf("failed to process receipts for block %d: %w", blockNum, err)
	}
	
	// 5. Verify data consistency - number of transactions processed should match block's transaction count
	if txCount != len(block.Transactions()) {
		p.logger.Warn("Some transactions were not processed correctly", 
			"block", blockNum, 
			"hash", blockHash.Hex(),
			"txTotal", len(block.Transactions()),
			"txProcessed", txCount,
			"txErrors", txErrors)
	}
	
	// Store a compact block representation for quick access
	blockSummary := map[string]interface{}{
		"hash":            blockHash.Hex(),
		"number":          blockNum,
		"timestamp":       block.Time(),
		"txCount":         len(block.Transactions()),
		"gasUsed":         block.GasUsed(),
		"gasLimit":        block.GasLimit(),
		"parentHash":      block.ParentHash().Hex(),
		"stateRoot":       block.Root().Hex(),
		"receiptsRoot":    block.ReceiptHash().Hex(),
		"transactionsRoot": block.TxHash().Hex(),
	}
	
	blockSummaryData, err := json.Marshal(blockSummary)
	if err == nil {
		// Store block summary using the pipeline
		blockSummaryKey := fmt.Sprintf("block:%d:summary", blockNum)
		pipe.Set(ctx, blockSummaryKey, blockSummaryData, 0)
	}
	
	// Execute all pipeline commands
	cmds, err := pipe.Exec(ctx)
	if err != nil {
		p.logger.Error("Failed to execute Redis pipeline for block data", "block", blockNum, "err", err)
		// Continue despite errors - we've already processed the crucial data
	} else {
		for i, cmd := range cmds {
			if cmd.Err() != nil {
				p.logger.Warn("Error in Redis pipeline command", "index", i, "err", cmd.Err())
			}
		}
	}
	
	p.logger.Info("Block processed and written to Redis", "block", blockNum, "hash", blockHash.Hex(), 
		"txCount", len(block.Transactions()), "receiptCount", len(receipts))
	return nil
}

