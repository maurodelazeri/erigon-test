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

	"github.com/erigontech/erigon-lib/kv"
	"github.com/erigontech/erigon/core/rawdb"
	"github.com/erigontech/erigon/core/state"
	"github.com/erigontech/erigon/core/types"
)

// RegisterAllHooks registers all Redis hooks to capture data from Erigon
func RegisterAllHooks(integration *RedisIntegration) {
	if integration == nil || !integration.IsEnabled() {
		return
	}
	
	integration.logger.Error("REDIS INTEGRATION: Registering all hooks to capture state changes, blocks, and transactions")
	
	// Register the state writer hook
	state.RegisterStateWriterWrapper(integration.WrapStateWriter)
	
	// Register block, header, and transaction hooks
	rawdb.RegisterBlockWriteHook(integration.BlockWriteHook)
	rawdb.RegisterHeaderWriteHook(integration.HeaderWriteHook)
	rawdb.RegisterTransactionWriteHook(integration.TransactionWriteHook)
	
	integration.logger.Error("REDIS INTEGRATION: Successfully registered all hooks")
}

// BlockWriteHook is called after a block is written to the database
func (ri *RedisIntegration) BlockWriteHook(db kv.RwTx, block *types.Block) error {
	if !ri.IsEnabled() {
		return nil
	}
	
	// Process the block with the Redis writer
	blockNum := block.NumberU64()
	
	// Get block receipts if available
	receipts := rawdb.ReadRawReceipts(db, blockNum)
	// If no receipts, create an empty slice
	if receipts == nil {
		receipts = types.Receipts{}
		ri.logger.Warn("No receipts found for block", "number", blockNum)
	}
	
	// Process the block
	if err := ri.blockProcessor.HandleBlock(block, receipts); err != nil {
		ri.logger.Error("Failed to process block", "number", blockNum, "hash", block.Hash().Hex(), "err", err)
		return err
	}
	
	// Log success
	ri.logger.Info("Successfully processed block", "number", blockNum, "hash", block.Hash().Hex())
	
	// Write a test key to verify Redis write
	ctx, cancel := context.WithTimeout(ri.ctx, 5*time.Second)
	defer cancel()
	
	testKey := fmt.Sprintf("redis_block_hook_%d", blockNum)
	testValue := fmt.Sprintf("Block %d processed at %s", blockNum, time.Now().Format(time.RFC3339))
	
	if err := ri.client.Set(ctx, testKey, testValue, 24*time.Hour).Err(); err != nil {
		ri.logger.Error("Failed to write test key", "key", testKey, "err", err)
	} else {
		ri.logger.Info("Successfully wrote test key", "key", testKey)
	}
	
	return nil
}

// HeaderWriteHook is called after a header is written to the database
func (ri *RedisIntegration) HeaderWriteHook(db kv.RwTx, header *types.Header) error {
	if !ri.IsEnabled() {
		return nil
	}
	
	// Process the header with the Redis writer
	blockNum := header.Number.Uint64()
	
	// Process the header
	err := ri.blockProcessor.ProcessBlockHeader(header)
	if err != nil {
		ri.logger.Error("Failed to process header", "number", blockNum, "hash", header.Hash().Hex(), "err", err)
		return err
	}
	
	// Log success
	ri.logger.Info("Successfully processed header", "number", blockNum, "hash", header.Hash().Hex())
	
	return nil
}

// TransactionWriteHook is called after transactions are written to the database
func (ri *RedisIntegration) TransactionWriteHook(db kv.RwTx, txs []types.Transaction, txnID uint64) error {
	if !ri.IsEnabled() {
		return nil
	}
	
	// Process each transaction
	for i, tx := range txs {
		currentTxID := txnID + uint64(i)
		hash := tx.Hash()
		
		// Get tx block number if available
		blockNumPtr, _, err := rawdb.ReadTxLookupEntry(db, hash)
		var blockNum uint64
		if err != nil || blockNumPtr == nil {
			ri.logger.Warn("Failed to read tx lookup entry", "hash", hash.Hex(), "err", err)
			// Use current transaction ID as fallback for block number if not available
			blockNum = txnID
		} else {
			blockNum = *blockNumPtr
		}
		
		// Log that we're processing this transaction
		ri.logger.Info("REDIS: Processing transaction from hook", "hash", hash.Hex(), "blockNum", blockNum, "txID", currentTxID)
		
		// Use the BlockHeaderProcessor to handle the transaction
		// This calls the HandleTransaction method we just added to BlockHeaderProcessor
		if err := ri.blockProcessor.HandleTransaction(tx, blockNum); err != nil {
			ri.logger.Error("Failed to process transaction", "hash", hash.Hex(), "err", err)
			// Continue with next transaction
			continue
		}
		
		// Log success
		ri.logger.Debug("Successfully processed transaction", "hash", hash.Hex(), "txID", currentTxID)
	}
	
	return nil
}