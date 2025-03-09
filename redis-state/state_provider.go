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
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/holiman/uint256"
	"github.com/redis/go-redis/v9"

	"github.com/erigontech/erigon-lib/chain"
	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon-lib/types/accounts"
	"github.com/erigontech/erigon/consensus"
	"github.com/erigontech/erigon/core/state"
	"github.com/erigontech/erigon/core/tracing"
	"github.com/erigontech/erigon/core/types"
	"github.com/erigontech/erigon/core/vm/evmtypes"
	"github.com/erigontech/erigon/params"
	"github.com/erigontech/erigon/rpc"
)

// RedisStateProvider implements the state provider interface for Erigon's RPC API
type RedisStateProvider struct {
	client *redis.Client
	ctx    context.Context
	logger log.Logger
}

// NewRedisStateProvider creates a new instance of RedisStateProvider
func NewRedisStateProvider(client *redis.Client, logger log.Logger) *RedisStateProvider {
	provider := &RedisStateProvider{
		client: client,
		ctx:    context.Background(),
		logger: logger,
	}

	// Initialize canonicalChain sorted set if it doesn't exist
	provider.ensureCanonicalChainExists()

	return provider
}

// ensureCanonicalChainExists makes sure the canonicalChain sorted set exists
// This is critical for tracking the canonical chain and handling reorgs
func (p *RedisStateProvider) ensureCanonicalChainExists() {
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	// Check if the canonical chain key exists
	canonicalKey := "canonicalChain"
	exists, err := p.client.Exists(ctx, canonicalKey).Result()
	if err != nil {
		p.logger.Warn("Failed to check if canonical chain exists", "err", err)
		return
	}

	if exists == 0 {
		// The canonical chain key doesn't exist - initialize it
		// We'll need to find the current head block and work backwards
		currentBlockStr, err := p.client.Get(ctx, "currentBlock").Result()
		if err != nil {
			p.logger.Warn("Failed to get current block", "err", err)
			return
		}

		currentBlock, err := strconv.ParseUint(currentBlockStr, 10, 64)
		if err != nil {
			p.logger.Warn("Failed to parse current block", "err", err)
			return
		}

		// Create a pipeline to add blocks to the canonical chain
		pipe := p.client.Pipeline()
		count := 0

		// Add the current block first
		blockKey := fmt.Sprintf("block:%d", currentBlock)
		blockHashBytes, err := p.client.HGet(ctx, blockKey, "hash").Result()
		if err == nil && blockHashBytes != "" {
			pipe.ZAdd(ctx, canonicalKey, redis.Z{
				Score:  float64(currentBlock),
				Member: blockHashBytes,
			})
			count++

			// Also make sure the block hash entry has the canonical flag
			hashKey := fmt.Sprintf("blockHash:%s", blockHashBytes)
			pipe.HSet(ctx, hashKey, "canonical", true)
		}

		// Execute the pipeline
		_, err = pipe.Exec(ctx)
		if err != nil {
			p.logger.Warn("Failed to initialize canonical chain", "err", err)
			return
		}

		p.logger.Info("Initialized canonical chain", "blocksAdded", count)
	}
}

// GetRedisClient returns the underlying Redis client
func (p *RedisStateProvider) GetRedisClient() *redis.Client {
	return p.client
}

// CheckAndHandleReorganization checks if a block was part of a chain reorganization
// by looking for reorg markers in Redis. Returns true if a reorg was detected.
func (p *RedisStateProvider) CheckAndHandleReorganization(blockNum uint64) (bool, error) {
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	// Check if there's a reorg marker for this block
	reorgKey := fmt.Sprintf("reorg:%d", blockNum)
	exists, err := p.client.Exists(ctx, reorgKey).Result()
	if err != nil {
		return false, fmt.Errorf("error checking reorg marker: %w", err)
	}

	if exists == 1 {
		// This block was marked as part of a reorg
		p.logger.Info("Detected block from reorganized chain", "block", blockNum)

		// Get the reorg timestamp for logging
		timestamp, err := p.client.Get(ctx, reorgKey).Result()
		if err != nil {
			p.logger.Warn("Failed to get reorg timestamp", "block", blockNum, "err", err)
		} else {
			p.logger.Info("Block was reorged at", "timestamp", timestamp, "block", blockNum)
		}

		return true, nil
	}

	return false, nil
}

// GetChainConfig returns the chain configuration for the provider
func (p *RedisStateProvider) GetChainConfig() *chain.Config {
	// For simplicity, start with mainnet config
	return params.MainnetChainConfig
}

// GetBlockHashFn returns a function to retrieve block hashes by number
func (p *RedisStateProvider) GetBlockHashFn(currentNum uint64) func(n uint64) libcommon.Hash {
	return func(n uint64) libcommon.Hash {
		// Skip if we're requesting a block beyond current
		if n > currentNum {
			return libcommon.Hash{}
		}

		// Look up block hash
		key := fmt.Sprintf("block:%d", n)
		result, err := p.client.HGet(p.ctx, key, "hash").Result()
		if err != nil || result == "" {
			p.logger.Warn("Failed to get block hash", "number", n, "err", err)
			return libcommon.Hash{}
		}

		return libcommon.HexToHash(result)
	}
}

// Engine returns a dummy consensus engine
func (p *RedisStateProvider) Engine() consensus.Engine {
	// Return nil for now since we don't need it for read-only operations
	return nil
}

// GetLatestBlockNumber retrieves the latest block number from Redis
func (p *RedisStateProvider) GetLatestBlockNumber(ctx context.Context) (uint64, error) {
	result, err := p.client.Get(p.ctx, "currentBlock").Result()
	if err != nil {
		return 0, fmt.Errorf("failed to get latest block number: %w", err)
	}

	blockNum, err := strconv.ParseUint(result, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse block number: %w", err)
	}

	return blockNum, nil
}

// GetBlockByNumber retrieves a block by its number from Redis with transaction details
func (p *RedisStateProvider) GetBlockByNumber(ctx context.Context, blockNumber rpc.BlockNumber, fullTx bool) (map[string]interface{}, error) {
	// Convert BlockNumber to uint64
	var blockNum uint64
	var err error

	switch blockNumber {
	case rpc.LatestBlockNumber, rpc.PendingBlockNumber:
		// Get the latest block number from Redis
		blockNum, err = p.GetLatestBlockNumber(ctx)
		if err != nil {
			return nil, err
		}
	case rpc.EarliestBlockNumber:
		blockNum = 0
	default:
		blockNum = uint64(blockNumber)
	}

	// Try to get the block summary first (quicker)
	blockSummaryKey := fmt.Sprintf("block:%d:summary", blockNum)
	summaryData, err := p.client.Get(p.ctx, blockSummaryKey).Result()

	if err == nil {
		// We have a summary, use it as the base
		var blockSummary map[string]interface{}
		if err := json.Unmarshal([]byte(summaryData), &blockSummary); err != nil {
			p.logger.Warn("Failed to unmarshal block summary", "block", blockNum, "err", err)
			// Continue to fetch detailed data below
		} else {
			// Add transactions if requested
			if fullTx {
				blockSummary["transactions"], err = p.getBlockTransactions(ctx, blockNum)
				if err != nil {
					p.logger.Warn("Failed to get block transactions", "block", blockNum, "err", err)
					blockSummary["transactions"] = []interface{}{}
				}
			} else {
				// Just include transaction hashes
				blockSummary["transactions"], err = p.getBlockTransactionHashes(ctx, blockNum)
				if err != nil {
					p.logger.Warn("Failed to get block transaction hashes", "block", blockNum, "err", err)
					blockSummary["transactions"] = []interface{}{}
				}
			}
			return blockSummary, nil
		}
	}

	// If we don't have a summary or it failed to unmarshal, get the full block data
	block, err := p.BlockByNumber(ctx, blockNumber)
	if err != nil {
		return nil, err
	}

	// Convert block to map
	result := map[string]interface{}{
		"number":           fmt.Sprintf("0x%x", block.NumberU64()),
		"hash":             block.Hash().Hex(),
		"parentHash":       block.ParentHash().Hex(),
		"nonce":            fmt.Sprintf("0x%x", block.Nonce()),
		"sha3Uncles":       block.UncleHash().Hex(),
		"logsBloom":        fmt.Sprintf("0x%x", block.Bloom()),
		"transactionsRoot": block.TxHash().Hex(),
		"stateRoot":        block.Root().Hex(),
		"receiptsRoot":     block.ReceiptHash().Hex(),
		"miner":            block.Coinbase().Hex(),
		"difficulty":       block.Difficulty().String(),
		"totalDifficulty":  "0x0", // Not available in Redis
		"extraData":        fmt.Sprintf("0x%x", block.Extra()),
		"size":             fmt.Sprintf("0x%x", block.Size()),
		"gasLimit":         fmt.Sprintf("0x%x", block.GasLimit()),
		"gasUsed":          fmt.Sprintf("0x%x", block.GasUsed()),
		"timestamp":        fmt.Sprintf("0x%x", block.Time()),
	}

	// Add transactions
	if fullTx {
		result["transactions"], err = p.getBlockTransactions(ctx, blockNum)
		if err != nil {
			p.logger.Warn("Failed to get block transactions", "block", blockNum, "err", err)
			result["transactions"] = []interface{}{}
		}
	} else {
		result["transactions"], err = p.getBlockTransactionHashes(ctx, blockNum)
		if err != nil {
			p.logger.Warn("Failed to get block transaction hashes", "block", blockNum, "err", err)
			result["transactions"] = []interface{}{}
		}
	}

	return result, nil
}

// getBlockTransactionHashes retrieves all transaction hashes for a block
func (p *RedisStateProvider) getBlockTransactionHashes(ctx context.Context, blockNum uint64) ([]interface{}, error) {
	blockTxsKey := fmt.Sprintf("block:%d:txs", blockNum)

	// Get transaction hashes
	txHashes, err := p.client.SMembers(p.ctx, blockTxsKey).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get block transaction hashes: %w", err)
	}

	// Convert to interface slice
	result := make([]interface{}, len(txHashes))
	for i, hash := range txHashes {
		result[i] = hash
	}

	return result, nil
}

// getBlockTransactions retrieves all transactions for a block with full details
func (p *RedisStateProvider) getBlockTransactions(ctx context.Context, blockNum uint64) ([]interface{}, error) {
	blockTxsKey := fmt.Sprintf("block:%d:txs", blockNum)

	// Get transaction hashes
	txHashes, err := p.client.SMembers(p.ctx, blockTxsKey).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get block transaction hashes: %w", err)
	}

	// Create a pipeline to fetch all transactions in a batch
	pipe := p.client.Pipeline()
	commands := make([]redis.Cmder, len(txHashes))

	// Queue up all transaction fetch commands
	for i, hash := range txHashes {
		txKey := fmt.Sprintf("tx:%s", hash)
		commands[i] = pipe.ZRevRangeByScore(p.ctx, txKey, &redis.ZRangeBy{
			Min:    fmt.Sprintf("%d", blockNum),
			Max:    fmt.Sprintf("%d", blockNum),
			Offset: 0,
			Count:  1,
		})
	}

	// Execute pipeline
	_, err = pipe.Exec(p.ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to execute transaction fetch pipeline: %w", err)
	}

	// Process results
	result := make([]interface{}, 0, len(txHashes))
	for i, cmd := range commands {
		zRangeCmd, ok := cmd.(*redis.StringSliceCmd)
		if !ok {
			p.logger.Warn("Failed to convert Redis command", "hash", txHashes[i])
			continue
		}

		values, err := zRangeCmd.Result()
		if err != nil {
			p.logger.Warn("Failed to get transaction data", "hash", txHashes[i], "err", err)
			continue
		}

		if len(values) == 0 {
			p.logger.Warn("Transaction not found", "hash", txHashes[i], "block", blockNum)
			continue
		}

		// Parse transaction
		var tx types.Transaction
		if err := json.Unmarshal([]byte(values[0]), &tx); err != nil {
			p.logger.Warn("Failed to unmarshal transaction", "hash", txHashes[i], "err", err)
			continue
		}

		// Convert to map
		txMap := map[string]interface{}{
			"hash":             tx.Hash().Hex(),
			"nonce":            fmt.Sprintf("0x%x", tx.GetNonce()),
			"blockHash":        fmt.Sprintf("0x%x", blockNum),
			"blockNumber":      fmt.Sprintf("0x%x", blockNum),
			"transactionIndex": fmt.Sprintf("0x%x", i),
		}

		// Handle from address
		if sender, ok := tx.GetSender(); ok {
			txMap["from"] = sender.Hex()
		} else {
			txMap["from"] = "0x0000000000000000000000000000000000000000"
		}

		// Handle to address
		if to := tx.GetTo(); to != nil {
			txMap["to"] = to.Hex()
		} else {
			txMap["to"] = nil // null for contract creation
		}

		// Handle value, gas price, gas and input data
		txMap["value"] = fmt.Sprintf("0x%x", tx.GetValue())
		txMap["gas"] = fmt.Sprintf("0x%x", tx.GetGas())

		// Handle different transaction types for gas pricing
		if tx.Type() == types.DynamicFeeTxType {
			txMap["maxFeePerGas"] = fmt.Sprintf("0x%x", tx.GetFeeCap())
			txMap["maxPriorityFeePerGas"] = fmt.Sprintf("0x%x", tx.GetTip())
			txMap["gasPrice"] = fmt.Sprintf("0x%x", tx.GetFeeCap()) // For backward compatibility
		} else {
			txMap["gasPrice"] = fmt.Sprintf("0x%x", tx.GetPrice())
		}

		txMap["input"] = fmt.Sprintf("0x%x", tx.GetData())

		result = append(result, txMap)
	}

	return result, nil
}

// BlockByNumber returns a block by its number with chain reorganization awareness
func (p *RedisStateProvider) BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error) {
	// Convert BlockNumber to uint64
	var blockNum uint64
	switch number {
	case rpc.LatestBlockNumber, rpc.PendingBlockNumber:
		// Get the latest block number from Redis
		result, err := p.client.Get(p.ctx, "currentBlock").Result()
		if err != nil {
			return nil, fmt.Errorf("failed to get latest block number: %w", err)
		}

		blockNum, err = strconv.ParseUint(result, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse block number: %w", err)
		}
	case rpc.EarliestBlockNumber:
		blockNum = 0
	default:
		blockNum = uint64(number)
	}

	// Get block hash and header
	blockKey := fmt.Sprintf("block:%d", blockNum)

	// Get all block data with HGETALL to get hash and header
	blockData, err := p.client.HGetAll(p.ctx, blockKey).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get block data: %w", err)
	}

	if len(blockData) == 0 {
		return nil, fmt.Errorf("block not found: %d", blockNum)
	}

	// Check if this block is canonical (the most recent version after any reorgs)
	blockHash := blockData["hash"]
	if blockHash == "" {
		return nil, fmt.Errorf("block hash missing for block: %d", blockNum)
	}

	// Verify this is a canonical block by checking the canonical chain
	canonicalKey := "canonicalChain"
	isCanonical, err := p.client.ZScore(p.ctx, canonicalKey, blockHash).Result()
	if err != nil && err != redis.Nil {
		// Log the error but continue - we'll use the block we have
		p.logger.Warn("Failed to check if block is canonical", "block", blockNum, "hash", blockHash, "err", err)
	}

	// If block is not in the canonical chain but we found a hash in the block data
	// this means we had a reorg, but we can try to use this block anyway (with a warning)
	if err == redis.Nil && blockHash != "" {
		p.logger.Warn("Block is not in canonical chain, possible stale data from reorg",
			"block", blockNum,
			"hash", blockHash)
	}

	headerBytes := blockData["header"]
	if headerBytes == "" {
		return nil, fmt.Errorf("block header missing for block: %d", blockNum)
	}

	// Parse header
	var header types.Header
	if err := json.Unmarshal([]byte(headerBytes), &header); err != nil {
		return nil, fmt.Errorf("failed to unmarshal header: %w", err)
	}

	// For simplicity, we're returning a block with just the header
	// In a real implementation, you'd also fetch the transactions and receipts
	block := types.NewBlockWithHeader(&header)

	// Log if we're using a non-canonical block due to reorg
	if err == redis.Nil && blockHash != "" {
		p.logger.Debug("Using non-canonical block",
			"block", blockNum,
			"hash", blockHash,
			"header", header.Hash().Hex())
	} else if isCanonical > 0 {
		p.logger.Debug("Using canonical block",
			"block", blockNum,
			"hash", blockHash,
			"score", isCanonical)
	}

	return block, nil
}

// BlockByHash returns a block by its hash with chain reorganization awareness
func (p *RedisStateProvider) BlockByHash(ctx context.Context, hash libcommon.Hash) (*types.Block, error) {
	// Get block information including canonical status
	hashKey := fmt.Sprintf("blockHash:%s", hash.Hex())
	blockData, err := p.client.HGetAll(p.ctx, hashKey).Result()
	if err != nil || len(blockData) == 0 {
		return nil, fmt.Errorf("block hash not found: %s", hash.Hex())
	}

	// Extract the block number
	blockNumStr, ok := blockData["number"]
	if !ok || blockNumStr == "" {
		return nil, fmt.Errorf("block number missing for hash: %s", hash.Hex())
	}

	blockNum, err := strconv.ParseUint(blockNumStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse block number: %w", err)
	}

	// Check if this block is canonical
	isCanonical, ok := blockData["canonical"]
	if !ok || isCanonical != "true" {
		// Block exists but is not canonical (from a reorganized branch)
		p.logger.Warn("Request for non-canonical block by hash",
			"hash", hash.Hex(),
			"block", blockNum)
	}

	// Get block hash and header directly from block data
	blockKey := fmt.Sprintf("block:%d", blockNum)
	headerBytes, err := p.client.HGet(p.ctx, blockKey, "header").Result()
	if err == redis.Nil {
		return nil, fmt.Errorf("block header not found for block: %d", blockNum)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get block header: %w", err)
	}

	// Parse header
	var header types.Header
	if err := json.Unmarshal([]byte(headerBytes), &header); err != nil {
		return nil, fmt.Errorf("failed to unmarshal header: %w", err)
	}

	// For simplicity, we're returning a block with just the header
	// In a real implementation, you'd also fetch the transactions and receipts
	block := types.NewBlockWithHeader(&header)
	return block, nil
}

// StateAtBlock returns a state reader for a specific block
// This method is enhanced to handle chain reorganizations by checking block canonicity
func (p *RedisStateProvider) StateAtBlock(ctx context.Context, block *types.Block) (evmtypes.IntraBlockState, error) {
	if block == nil {
		return nil, errors.New("block is nil")
	}

	// Verify this block is canonical by checking both the block number and hash
	blockNum := block.NumberU64()
	blockHash := block.Hash()

	// Check if this is a canonical block
	canonicalKey := "canonicalChain"
	isCanonical, err := p.client.ZScore(p.ctx, canonicalKey, blockHash.Hex()).Result()

	// If the block is not in the canonical chain, log a warning
	// This is important for simulations to be aware they might be using non-canonical state
	if err == redis.Nil {
		// Block not found in canonical chain - this could be due to a reorg
		p.logger.Warn("Loading state for non-canonical block",
			"blockNum", blockNum,
			"blockHash", blockHash.Hex())

		// We'll still proceed with loading state for this block, even if it's not canonical
	} else if err != nil {
		// Some other error occurred - log but continue
		p.logger.Warn("Failed to check if block is canonical",
			"blockNum", blockNum,
			"blockHash", blockHash.Hex(),
			"err", err)
	} else {
		// Block is confirmed canonical
		p.logger.Debug("Loading state for canonical block",
			"blockNum", blockNum,
			"blockHash", blockHash.Hex(),
			"score", isCanonical)
	}

	// Create a reader for the block's state
	// We use a specific point-in-time reader that looks up the state as of this block
	reader := NewPointInTimeRedisStateReader(p.client, blockNum)

	// Create the intra-block state using this reader
	state := NewRedisIntraBlockState(reader, blockNum)

	return state, nil
}

// StateAtTransaction returns a state reader for a specific transaction
func (p *RedisStateProvider) StateAtTransaction(ctx context.Context, block *types.Block, txIndex int) (evmtypes.IntraBlockState, *types.Transaction, error) {
	if block == nil {
		return nil, nil, errors.New("block is nil")
	}

	transactions := block.Transactions()
	if txIndex < 0 || txIndex >= len(transactions) {
		return nil, nil, fmt.Errorf("transaction index out of range: %d", txIndex)
	}

	// Get the transaction from the block - it's already a pointer
	tx := transactions[txIndex]

	// Create a state reader for the block's state
	state, err := p.StateAtBlock(ctx, block)
	if err != nil {
		return nil, nil, err
	}

	return state, &tx, nil
}

// BalanceAt returns the balance of the given account at the given block
func (p *RedisStateProvider) BalanceAt(ctx context.Context, address libcommon.Address, blockNumber rpc.BlockNumber) (*big.Int, error) {
	// Convert BlockNumber to uint64
	var blockNum uint64
	switch blockNumber {
	case rpc.LatestBlockNumber, rpc.PendingBlockNumber:
		// Get the latest block number from Redis
		result, err := p.client.Get(p.ctx, "currentBlock").Result()
		if err != nil {
			return nil, fmt.Errorf("failed to get latest block number: %w", err)
		}

		blockNum, err = strconv.ParseUint(result, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse block number: %w", err)
		}
	case rpc.EarliestBlockNumber:
		blockNum = 0
	default:
		blockNum = uint64(blockNumber)
	}

	// Use the Redis state reader to get the account data
	reader := NewPointInTimeRedisStateReader(p.client, blockNum)
	account, err := reader.ReadAccountData(address)
	if err != nil {
		return nil, fmt.Errorf("failed to read account data: %w", err)
	}

	if account == nil {
		return big.NewInt(0), nil
	}

	return account.Balance.ToBig(), nil
}

// GetTransactionByHash retrieves a transaction by its hash
func (p *RedisStateProvider) GetTransactionByHash(ctx context.Context, hash libcommon.Hash) (*types.Transaction, libcommon.Hash, uint64, uint64, error) {
	txKey := fmt.Sprintf("tx:%s", hash.Hex())

	// Get the transaction data
	results, err := p.client.ZRevRange(p.ctx, txKey, 0, 0).Result()
	if err != nil {
		return nil, libcommon.Hash{}, 0, 0, fmt.Errorf("failed to retrieve transaction %s: %w", hash.Hex(), err)
	}

	if len(results) == 0 {
		return nil, libcommon.Hash{}, 0, 0, fmt.Errorf("transaction not found: %s", hash.Hex())
	}

	// Unmarshal the transaction data
	var tx types.Transaction
	if err := json.Unmarshal([]byte(results[0]), &tx); err != nil {
		return nil, libcommon.Hash{}, 0, 0, fmt.Errorf("failed to unmarshal transaction data: %w", err)
	}

	// Now find the block information for this transaction
	blockInfo, err := p.client.ZRevRangeWithScores(p.ctx, txKey, 0, 0).Result()
	if err != nil || len(blockInfo) == 0 {
		return nil, libcommon.Hash{}, 0, 0, fmt.Errorf("failed to get block info for transaction: %w", err)
	}

	blockNum := uint64(blockInfo[0].Score)

	// Get the block hash
	blockKey := fmt.Sprintf("block:%d", blockNum)
	blockHash, err := p.client.HGet(p.ctx, blockKey, "hash").Result()
	if err != nil {
		return nil, libcommon.Hash{}, 0, 0, fmt.Errorf("failed to get block hash: %w", err)
	}

	// Find the transaction index in the block
	blockTxsKey := fmt.Sprintf("block:%d:txs", blockNum)
	txHashes, err := p.client.SMembers(p.ctx, blockTxsKey).Result()
	if err != nil {
		return nil, libcommon.Hash{}, 0, 0, fmt.Errorf("failed to get block transactions: %w", err)
	}

	txIndex := uint64(0)
	found := false
	for i, txHash := range txHashes {
		if txHash == hash.Hex() {
			txIndex = uint64(i)
			found = true
			break
		}
	}

	if !found {
		p.logger.Warn("Transaction found but index in block not found", "tx", hash.Hex(), "block", blockNum)
	}

	return &tx, libcommon.HexToHash(blockHash), blockNum, txIndex, nil
}

// GetTransactionReceipt retrieves a transaction receipt by transaction hash
func (p *RedisStateProvider) GetTransactionReceipt(ctx context.Context, txHash libcommon.Hash) (*types.Receipt, error) {
	receiptKey := fmt.Sprintf("receipt:%s", txHash.Hex())

	// Get the receipt data
	results, err := p.client.ZRevRange(p.ctx, receiptKey, 0, 0).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve receipt for transaction %s: %w", txHash.Hex(), err)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("receipt not found for transaction: %s", txHash.Hex())
	}

	// Unmarshal the receipt data
	var receipt types.Receipt
	if err := json.Unmarshal([]byte(results[0]), &receipt); err != nil {
		return nil, fmt.Errorf("failed to unmarshal receipt data: %w", err)
	}

	return &receipt, nil
}

// GetLogs retrieves logs based on filter criteria
func (p *RedisStateProvider) GetLogs(ctx context.Context, filter *types.LogFilter) ([]*types.Log, error) {
	var logs []*types.Log

	// Determine block range
	fromBlock, toBlock, err := p.resolveBlockRange(filter.FromBlock, filter.ToBlock)
	if err != nil {
		return nil, err
	}

	// Check if we're filtering by address
	if len(filter.Addresses) > 0 {
		// Process each address
		for _, address := range filter.Addresses {
			addressLogs, err := p.getLogsByAddress(address, fromBlock, toBlock)
			if err != nil {
				p.logger.Warn("Error getting logs by address", "address", address.Hex(), "err", err)
				continue
			}
			logs = append(logs, addressLogs...)
		}
	} else if len(filter.Topics) > 0 {
		// Process by topics
		topicLogs, err := p.getLogsByTopics(filter.Topics, fromBlock, toBlock)
		if err != nil {
			return nil, err
		}
		logs = append(logs, topicLogs...)
	} else {
		// No specific filtering, get all logs in block range
		// This can be very inefficient for large ranges
		for blockNum := fromBlock; blockNum <= toBlock; blockNum++ {
			blockLogs, err := p.getLogsByBlock(blockNum)
			if err != nil {
				p.logger.Warn("Error getting logs for block", "block", blockNum, "err", err)
				continue
			}
			logs = append(logs, blockLogs...)
		}
	}

	// Apply additional filtering based on both address and topics
	if len(filter.Addresses) > 0 && len(filter.Topics) > 0 {
		var filteredLogs []*types.Log
		for _, log := range logs {
			// Check if log is from one of the addresses
			addressMatch := false
			for _, address := range filter.Addresses {
				if log.Address == address {
					addressMatch = true
					break
				}
			}

			if !addressMatch {
				continue
			}

			// Check if log matches topics
			if p.logMatchesTopics(log, filter.Topics) {
				filteredLogs = append(filteredLogs, log)
			}
		}
		logs = filteredLogs
	}

	return logs, nil
}

// resolveBlockRange resolves block range parameters
func (p *RedisStateProvider) resolveBlockRange(fromBlock, toBlock *rpc.BlockNumber) (uint64, uint64, error) {
	var from, to uint64
	var err error

	// Resolve fromBlock
	if fromBlock == nil {
		// Default to latest block
		from, err = p.GetLatestBlockNumber(p.ctx)
		if err != nil {
			return 0, 0, err
		}
	} else {
		switch *fromBlock {
		case rpc.LatestBlockNumber, rpc.PendingBlockNumber:
			from, err = p.GetLatestBlockNumber(p.ctx)
			if err != nil {
				return 0, 0, err
			}
		case rpc.EarliestBlockNumber:
			from = 0
		default:
			from = uint64(*fromBlock)
		}
	}

	// Resolve toBlock
	if toBlock == nil {
		// Default to latest block
		to, err = p.GetLatestBlockNumber(p.ctx)
		if err != nil {
			return 0, 0, err
		}
	} else {
		switch *toBlock {
		case rpc.LatestBlockNumber, rpc.PendingBlockNumber:
			to, err = p.GetLatestBlockNumber(p.ctx)
			if err != nil {
				return 0, 0, err
			}
		case rpc.EarliestBlockNumber:
			to = 0
		default:
			to = uint64(*toBlock)
		}
	}

	// Sanity check
	if to < from {
		return from, from, nil
	}

	return from, to, nil
}

// getLogsByAddress retrieves logs for a specific address
func (p *RedisStateProvider) getLogsByAddress(address libcommon.Address, fromBlock, toBlock uint64) ([]*types.Log, error) {
	var logs []*types.Log

	// Get log indices for this address within the block range
	addressKey := fmt.Sprintf("address:%s:logs", address.Hex())
	logIndices, err := p.client.ZRangeByScore(p.ctx, addressKey, &redis.ZRangeBy{
		Min: fmt.Sprintf("%d", fromBlock),
		Max: fmt.Sprintf("%d", toBlock),
	}).Result()

	if err != nil {
		return nil, fmt.Errorf("failed to get logs for address %s: %w", address.Hex(), err)
	}

	// Retrieve each log
	for _, indexStr := range logIndices {
		logIndex, err := strconv.ParseUint(indexStr, 10, 32)
		if err != nil {
			p.logger.Warn("Invalid log index", "index", indexStr, "err", err)
			continue
		}

		logKey := fmt.Sprintf("log:%d", logIndex)
		logData, err := p.client.Get(p.ctx, logKey).Result()
		if err != nil {
			p.logger.Warn("Failed to get log", "index", logIndex, "err", err)
			continue
		}

		var log types.Log
		if err := json.Unmarshal([]byte(logData), &log); err != nil {
			p.logger.Warn("Failed to unmarshal log", "index", logIndex, "err", err)
			continue
		}

		logs = append(logs, &log)
	}

	return logs, nil
}

// getLogsByTopics retrieves logs matching topic filters
func (p *RedisStateProvider) getLogsByTopics(topics [][]libcommon.Hash, fromBlock, toBlock uint64) ([]*types.Log, error) {
	if len(topics) == 0 {
		return nil, nil
	}

	// For simplicity, we'll use the first topic as the primary filter
	primaryTopic := topics[0][0]
	topicKey := fmt.Sprintf("topic:%s", primaryTopic.Hex())

	// Get log indices for this topic within the block range
	logIndices, err := p.client.ZRangeByScore(p.ctx, topicKey, &redis.ZRangeBy{
		Min: fmt.Sprintf("%d", fromBlock),
		Max: fmt.Sprintf("%d", toBlock),
	}).Result()

	if err != nil {
		return nil, fmt.Errorf("failed to get logs for topic %s: %w", primaryTopic.Hex(), err)
	}

	var logs []*types.Log

	// Retrieve each log
	for _, indexStr := range logIndices {
		logIndex, err := strconv.ParseUint(indexStr, 10, 32)
		if err != nil {
			p.logger.Warn("Invalid log index", "index", indexStr, "err", err)
			continue
		}

		logKey := fmt.Sprintf("log:%d", logIndex)
		logData, err := p.client.Get(p.ctx, logKey).Result()
		if err != nil {
			p.logger.Warn("Failed to get log", "index", logIndex, "err", err)
			continue
		}

		var log types.Log
		if err := json.Unmarshal([]byte(logData), &log); err != nil {
			p.logger.Warn("Failed to unmarshal log", "index", logIndex, "err", err)
			continue
		}

		// Check if this log matches all topic filters
		if p.logMatchesTopics(&log, topics) {
			logs = append(logs, &log)
		}
	}

	return logs, nil
}

// getLogsByBlock retrieves all logs for a specific block
func (p *RedisStateProvider) getLogsByBlock(blockNum uint64) ([]*types.Log, error) {
	var logs []*types.Log

	// Get transaction receipts for the block
	blockReceiptsKey := fmt.Sprintf("block:%d:receipts", blockNum)
	txHashes, err := p.client.ZRange(p.ctx, blockReceiptsKey, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get receipts for block %d: %w", blockNum, err)
	}

	// For each transaction, get the receipt and extract logs
	for _, txHash := range txHashes {
		receipt, err := p.GetTransactionReceipt(p.ctx, libcommon.HexToHash(txHash))
		if err != nil {
			p.logger.Warn("Failed to get receipt", "tx", txHash, "err", err)
			continue
		}

		logs = append(logs, receipt.Logs...)
	}

	return logs, nil
}

// logMatchesTopics checks if a log matches the given topic filters
func (p *RedisStateProvider) logMatchesTopics(log *types.Log, topicFilters [][]libcommon.Hash) bool {
	// If the log has fewer topics than we're filtering for, it can't match
	if len(log.Topics) < len(topicFilters) {
		return false
	}

	// Check each topic against its corresponding filter
	for i, topicFilter := range topicFilters {
		if len(topicFilter) == 0 {
			// Empty filter means match any topic at this position
			continue
		}

		matched := false
		for _, filterTopic := range topicFilter {
			if log.Topics[i] == filterTopic {
				matched = true
				break
			}
		}

		if !matched {
			return false
		}
	}

	return true
}

// PointInTimeRedisStateReader extends RedisStateReader to read state at a specific block
type PointInTimeRedisStateReader struct {
	RedisStateReader
	blockNum uint64
}

// NewPointInTimeRedisStateReader creates a new instance of PointInTimeRedisStateReader
func NewPointInTimeRedisStateReader(client *redis.Client, blockNum uint64) *PointInTimeRedisStateReader {
	return &PointInTimeRedisStateReader{
		RedisStateReader: RedisStateReader{
			client: client,
			ctx:    context.Background(),
		},
		blockNum: blockNum,
	}
}

// ReadAccountData reads account data from Redis at a specific block
func (r *PointInTimeRedisStateReader) ReadAccountData(address libcommon.Address) (*accounts.Account, error) {
	key := accountKey(address)

	// Get the most recent account data before or at the specified block
	result := r.client.ZRevRangeByScore(r.ctx, key, &redis.ZRangeBy{
		Min:    "0",
		Max:    fmt.Sprintf("%d", r.blockNum),
		Offset: 0,
		Count:  1,
	})

	if result.Err() != nil {
		return nil, result.Err()
	}

	values, err := result.Result()
	if err != nil {
		return nil, err
	}

	if len(values) == 0 {
		return nil, nil // Account doesn't exist at this block
	}

	var serialized SerializedAccount
	if err := json.Unmarshal([]byte(values[0]), &serialized); err != nil {
		return nil, err
	}

	balance, err := uint256.FromHex(serialized.Balance)
	if err != nil {
		return nil, err
	}

	return &accounts.Account{
		Nonce:       serialized.Nonce,
		Balance:     *balance,
		CodeHash:    serialized.CodeHash,
		Incarnation: serialized.Incarnation,
	}, nil
}

// ReadAccountStorage reads account storage from Redis at a specific block
func (r *PointInTimeRedisStateReader) ReadAccountStorage(address libcommon.Address, incarnation uint64, key *libcommon.Hash) ([]byte, error) {
	storageKeyStr := storageKey(address, key)

	// Get the most recent storage data before or at the specified block
	result := r.client.ZRevRangeByScore(r.ctx, storageKeyStr, &redis.ZRangeBy{
		Min:    "0",
		Max:    fmt.Sprintf("%d", r.blockNum),
		Offset: 0,
		Count:  1,
	})

	if result.Err() != nil {
		return nil, result.Err()
	}

	values, err := result.Result()
	if err != nil {
		return nil, err
	}

	if len(values) == 0 {
		return nil, nil // Storage doesn't exist at this block
	}

	return []byte(values[0]), nil
}

// accessList is a simple implementation of an access list for our RedisIntraBlockState
type accessList struct {
	addresses map[libcommon.Address]bool
	slots     map[libcommon.Address]map[libcommon.Hash]bool
}

func newAccessList() *accessList {
	return &accessList{
		addresses: make(map[libcommon.Address]bool),
		slots:     make(map[libcommon.Address]map[libcommon.Hash]bool),
	}
}

func (al *accessList) ContainsAddress(addr libcommon.Address) bool {
	return al.addresses[addr]
}

func (al *accessList) AddAddress(addr libcommon.Address) bool {
	if al.addresses[addr] {
		return false
	}
	al.addresses[addr] = true
	return true
}

func (al *accessList) AddSlot(addr libcommon.Address, slot libcommon.Hash) (bool, bool) {
	addrChange := al.AddAddress(addr)

	if al.slots[addr] == nil {
		al.slots[addr] = make(map[libcommon.Hash]bool)
	}

	if al.slots[addr][slot] {
		return addrChange, false
	}

	al.slots[addr][slot] = true
	return addrChange, true
}

// RedisIntraBlockState implements the evmtypes.IntraBlockState interface using Redis
type RedisIntraBlockState struct {
	stateReader state.StateReader
	blockNum    uint64
	hooks       *tracing.Hooks
	accessList  *accessList
	refund      uint64
}

// NewRedisIntraBlockState creates a new instance of RedisIntraBlockState
func NewRedisIntraBlockState(stateReader state.StateReader, blockNum uint64) *RedisIntraBlockState {
	return &RedisIntraBlockState{
		stateReader: stateReader,
		blockNum:    blockNum,
		accessList:  newAccessList(),
	}
}

// Implementation of required evmtypes.IntraBlockState methods

// CreateAccount creates a new account
func (s *RedisIntraBlockState) CreateAccount(addr libcommon.Address, contractCreation bool) error {
	return errors.New("not implemented: read-only state")
}

// SubBalance subtracts amount from account
func (s *RedisIntraBlockState) SubBalance(addr libcommon.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) error {
	return errors.New("not implemented: read-only state")
}

// AddBalance adds amount to account
func (s *RedisIntraBlockState) AddBalance(addr libcommon.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) error {
	return errors.New("not implemented: read-only state")
}

// GetBalance returns the balance of the given account
func (s *RedisIntraBlockState) GetBalance(addr libcommon.Address) (*uint256.Int, error) {
	account, err := s.stateReader.ReadAccountData(addr)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return uint256.NewInt(0), nil
	}

	result := new(uint256.Int).Set(&account.Balance)
	return result, nil
}

// GetNonce returns the nonce of the given account
func (s *RedisIntraBlockState) GetNonce(addr libcommon.Address) (uint64, error) {
	account, err := s.stateReader.ReadAccountData(addr)
	if err != nil {
		return 0, err
	}
	if account == nil {
		return 0, nil
	}
	return account.Nonce, nil
}

// SetNonce sets the nonce of the account
func (s *RedisIntraBlockState) SetNonce(addr libcommon.Address, nonce uint64) error {
	return errors.New("not implemented: read-only state")
}

// GetCodeHash returns the code hash of the given account
func (s *RedisIntraBlockState) GetCodeHash(addr libcommon.Address) (libcommon.Hash, error) {
	account, err := s.stateReader.ReadAccountData(addr)
	if err != nil {
		return libcommon.Hash{}, err
	}
	if account == nil {
		return libcommon.Hash{}, nil
	}
	return account.CodeHash, nil
}

// GetCode returns the code of the given account
func (s *RedisIntraBlockState) GetCode(addr libcommon.Address) ([]byte, error) {
	account, err := s.stateReader.ReadAccountData(addr)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, nil
	}

	code, err := s.stateReader.ReadAccountCode(addr, account.Incarnation)
	if err != nil {
		return nil, err
	}
	return code, nil
}

// SetCode sets the code of the account
func (s *RedisIntraBlockState) SetCode(addr libcommon.Address, code []byte) error {
	return errors.New("not implemented: read-only state")
}

// GetCodeSize returns the code size of the given account
func (s *RedisIntraBlockState) GetCodeSize(addr libcommon.Address) (int, error) {
	code, err := s.GetCode(addr)
	if err != nil {
		return 0, err
	}
	return len(code), nil
}

// ResolveCodeHash returns code hash, potentially delegated
func (s *RedisIntraBlockState) ResolveCodeHash(addr libcommon.Address) (libcommon.Hash, error) {
	return s.GetCodeHash(addr)
}

// ResolveCode returns code, potentially delegated
func (s *RedisIntraBlockState) ResolveCode(addr libcommon.Address) ([]byte, error) {
	return s.GetCode(addr)
}

// GetDelegatedDesignation returns designated address
func (s *RedisIntraBlockState) GetDelegatedDesignation(addr libcommon.Address) (libcommon.Address, bool, error) {
	return libcommon.Address{}, false, nil
}

// AddRefund adds to the refund counter
func (s *RedisIntraBlockState) AddRefund(gas uint64) {
	s.refund += gas
}

// SubRefund subtracts from the refund counter
func (s *RedisIntraBlockState) SubRefund(gas uint64) {
	if gas > s.refund {
		s.refund = 0
	} else {
		s.refund -= gas
	}
}

// GetRefund returns the refund counter
func (s *RedisIntraBlockState) GetRefund() uint64 {
	return s.refund
}

// GetCommittedState gets the committed state
func (s *RedisIntraBlockState) GetCommittedState(addr libcommon.Address, key *libcommon.Hash, outValue *uint256.Int) error {
	return s.GetState(addr, key, outValue)
}

// GetState gets the state value
func (s *RedisIntraBlockState) GetState(addr libcommon.Address, slot *libcommon.Hash, outValue *uint256.Int) error {
	account, err := s.stateReader.ReadAccountData(addr)
	if err != nil {
		return err
	}
	if account == nil {
		outValue.Clear()
		return nil
	}

	value, err := s.stateReader.ReadAccountStorage(addr, account.Incarnation, slot)
	if err != nil {
		return err
	}

	if len(value) == 0 {
		outValue.Clear()
		return nil
	}

	outValue.SetBytes(value)
	return nil
}

// SetState sets the state value
func (s *RedisIntraBlockState) SetState(addr libcommon.Address, key *libcommon.Hash, value uint256.Int) error {
	return errors.New("not implemented: read-only state")
}

// GetTransientState gets the transient state
func (s *RedisIntraBlockState) GetTransientState(addr libcommon.Address, key libcommon.Hash) uint256.Int {
	var value uint256.Int
	return value
}

// SetTransientState sets the transient state
func (s *RedisIntraBlockState) SetTransientState(addr libcommon.Address, key libcommon.Hash, value uint256.Int) {
	// No-op in read-only mode
}

// Selfdestruct marks the contract for self-destruction
func (s *RedisIntraBlockState) Selfdestruct(addr libcommon.Address) (bool, error) {
	return false, errors.New("not implemented: read-only state")
}

// HasSelfdestructed reports whether the contract was selfdestructed
func (s *RedisIntraBlockState) HasSelfdestructed(addr libcommon.Address) (bool, error) {
	return false, nil
}

// Selfdestruct6780 marks the contract for self-destruction with 6780 rules
func (s *RedisIntraBlockState) Selfdestruct6780(addr libcommon.Address) error {
	return errors.New("not implemented: read-only state")
}

// Exist reports whether the given account exists
func (s *RedisIntraBlockState) Exist(addr libcommon.Address) (bool, error) {
	account, err := s.stateReader.ReadAccountData(addr)
	if err != nil {
		return false, err
	}
	return account != nil, nil
}

// Empty returns whether the given account is empty
func (s *RedisIntraBlockState) Empty(addr libcommon.Address) (bool, error) {
	account, err := s.stateReader.ReadAccountData(addr)
	if err != nil {
		return true, err
	}
	if account == nil {
		return true, nil
	}
	return account.Nonce == 0 && account.Balance.IsZero() && account.CodeHash == (libcommon.Hash{}), nil
}

// Prepare prepares the access list from rules and transactions
func (s *RedisIntraBlockState) Prepare(rules *chain.Rules, sender, coinbase libcommon.Address, dest *libcommon.Address,
	precompiles []libcommon.Address, txAccesses types.AccessList, authorities []libcommon.Address) error {

	// Convert the transaction access list to the internal one
	s.accessList = newAccessList()
	for _, access := range txAccesses {
		s.accessList.AddAddress(access.Address)
		for _, key := range access.StorageKeys {
			s.accessList.AddSlot(access.Address, key)
		}
	}

	// Add the sender, coinbase and precompiled addresses
	s.accessList.AddAddress(sender)
	s.accessList.AddAddress(coinbase)
	for _, addr := range precompiles {
		s.accessList.AddAddress(addr)
	}

	// Add destination if there is one
	if dest != nil {
		s.accessList.AddAddress(*dest)
	}

	// Add authorities if provided
	for _, authority := range authorities {
		s.accessList.AddAddress(authority)
	}

	return nil
}

// AddressInAccessList returns whether an address is in the access list
func (s *RedisIntraBlockState) AddressInAccessList(addr libcommon.Address) bool {
	if s.accessList == nil {
		return false
	}
	return s.accessList.ContainsAddress(addr)
}

// AddAddressToAccessList adds an address to the access list
func (s *RedisIntraBlockState) AddAddressToAccessList(addr libcommon.Address) bool {
	if s.accessList == nil {
		s.accessList = newAccessList()
	}
	return s.accessList.AddAddress(addr)
}

// AddSlotToAccessList adds a slot to the access list
func (s *RedisIntraBlockState) AddSlotToAccessList(addr libcommon.Address, slot libcommon.Hash) (bool, bool) {
	if s.accessList == nil {
		s.accessList = newAccessList()
	}
	return s.accessList.AddSlot(addr, slot)
}

// RevertToSnapshot reverts to a given snapshot
func (s *RedisIntraBlockState) RevertToSnapshot(id int) {
	// No-op in read-only mode
}

// Snapshot creates a new snapshot
func (s *RedisIntraBlockState) Snapshot() int {
	return 0 // Always return 0 in read-only mode
}

// AddLog adds a log entry
func (s *RedisIntraBlockState) AddLog(log *types.Log) {
	// No-op in read-only mode
}

// AddPreimage adds a preimage
func (s *RedisIntraBlockState) AddPreimage(hash libcommon.Hash, preimage []byte) {
	// No-op in read-only mode
}

// SetHooks sets the tracing hooks
func (s *RedisIntraBlockState) SetHooks(hooks *tracing.Hooks) {
	s.hooks = hooks
}
