package state

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon-lib/rlp"
	"github.com/erigontech/erigon-lib/types/accounts"
	"github.com/erigontech/erigon/core/types"
	"github.com/holiman/uint256"
	"github.com/redis/go-redis/v9"
)

// RedisState handles persisting blockchain state to Redis
type RedisState struct {
	client        *redis.Client
	enabled       bool
	url           string
	logger        log.Logger
	currentBlock  uint64
	blockMutex    sync.RWMutex
	pipeline      redis.Pipeliner
	pipelineMutex sync.Mutex
	ctx           context.Context
}

// Global instance
var redisState *RedisState
var redisInitOnce sync.Once

// AccountJSON represents an account as stored in Redis
type AccountJSON struct {
	Balance     string `json:"balance"`
	Nonce       uint64 `json:"nonce"`
	CodeHash    string `json:"codeHash"`
	Incarnation uint64 `json:"incarnation"`
}

// Initialize creates or returns the global RedisState instance
func InitRedisState(redisURL string, redisPassword string, logger log.Logger) *RedisState {
	redisInitOnce.Do(func() {
		if redisURL == "" {
			redisState = &RedisState{
				enabled: false,
				logger:  logger,
				ctx:     context.Background(),
			}
			return
		}

		// Parse Redis URL to extract address and DB
		opts, err := redis.ParseURL(redisURL)
		if err != nil {
			logger.Error("Invalid Redis URL format", "url", redisURL, "error", err)
			redisState = &RedisState{
				enabled: false,
				logger:  logger,
				ctx:     context.Background(),
			}
			return
		}

		// Override password if provided
		if redisPassword != "" {
			opts.Password = redisPassword
		}

		client := redis.NewClient(opts)

		// Test connection
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if _, err := client.Ping(ctx).Result(); err != nil {
			logger.Error("Failed to connect to Redis", "url", redisURL, "error", err)
			redisState = &RedisState{
				enabled: false,
				logger:  logger,
				ctx:     context.Background(),
			}
			return
		}

		redisState = &RedisState{
			client:   client,
			enabled:  true,
			url:      redisURL,
			logger:   logger,
			ctx:      context.Background(),
			pipeline: client.Pipeline(),
		}

		// Load current block if available
		val, err := client.Get(ctx, "currentBlock").Result()
		if err == nil {
			currentBlock, err := strconv.ParseUint(val, 10, 64)
			if err == nil {
				redisState.currentBlock = currentBlock
			}
		}

		logger.Info("Redis state integration initialized", "url", redisURL, "enabled", true)
	})

	return redisState
}

// GetRedisState returns the global RedisState instance
func GetRedisState() *RedisState {
	if redisState == nil {
		// Return a disabled instance if not initialized
		return &RedisState{
			enabled: false,
			ctx:     context.Background(),
		}
	}
	return redisState
}

// Enabled returns whether Redis integration is enabled
func (rs *RedisState) Enabled() bool {
	return rs.enabled
}

// Close closes the Redis connection
func (rs *RedisState) Close() error {
	if !rs.enabled {
		return nil
	}

	rs.FlushPipeline() // Ensure all commands are sent

	if rs.client != nil {
		return rs.client.Close()
	}
	return nil
}

// beginBlockProcessing starts block processing in Redis
func (rs *RedisState) beginBlockProcessing(blockNum uint64) {
	if !rs.enabled {
		return
	}

	rs.blockMutex.Lock()
	defer rs.blockMutex.Unlock()

	// Only execute if this is an advancement
	if blockNum > rs.currentBlock {
		rs.currentBlock = blockNum
		rs.pipelineMutex.Lock()
		rs.pipeline.Set(rs.ctx, "currentBlock", blockNum, 0)
		rs.pipelineMutex.Unlock()
	}
}

// FlushPipeline sends all queued commands to Redis
// This is exported to allow explicit flushing at commit points
func (rs *RedisState) FlushPipeline() {
	if !rs.enabled {
		return
	}

	rs.pipelineMutex.Lock()
	defer rs.pipelineMutex.Unlock()

	_, err := rs.pipeline.Exec(rs.ctx)
	if err != nil {
		rs.logger.Error("Failed to execute Redis pipeline", "error", err)
	}

	// Create a new pipeline
	rs.pipeline = rs.client.Pipeline()
}

// writeAccount writes account data to Redis
func (rs *RedisState) writeAccount(blockNum uint64, blockHash libcommon.Hash, address libcommon.Address, account *accounts.Account) {
	if !rs.enabled || account == nil {
		return
	}

	rs.logger.Debug("Writing account to Redis", "address", address.Hex(), "nonce", account.Nonce, "balance", account.Balance.ToBig().String(), "block", blockNum)

	accountJSON := AccountJSON{
		Balance:     account.Balance.ToBig().String(),
		Nonce:       account.Nonce,
		CodeHash:    account.CodeHash.Hex(),
		Incarnation: account.Incarnation,
	}

	jsonData, err := json.Marshal(accountJSON)
	if err != nil {
		rs.logger.Error("Failed to marshal account data", "address", address, "error", err)
		return
	}

	rs.pipelineMutex.Lock()
	defer rs.pipelineMutex.Unlock()

	// Store in Redis as a sorted set with block number as score
	key := fmt.Sprintf("account:%s", address.Hex())
	rs.pipeline.ZAdd(rs.ctx, key, redis.Z{
		Score:  float64(blockNum),
		Member: string(jsonData),
	})

	// Also store in block-specific account index
	rs.pipeline.HSet(rs.ctx, fmt.Sprintf("block:%s:accounts", blockHash.Hex()), address.Hex(), string(jsonData))
}

// deleteAccount marks an account as deleted in Redis
func (rs *RedisState) deleteAccount(blockNum uint64, blockHash libcommon.Hash, address libcommon.Address) {
	if !rs.enabled {
		return
	}

	rs.pipelineMutex.Lock()
	defer rs.pipelineMutex.Unlock()

	// Store a deletion marker in the sorted set
	key := fmt.Sprintf("account:%s", address.Hex())
	deletionMarker := "{\"deleted\":true}"

	rs.pipeline.ZAdd(rs.ctx, key, redis.Z{
		Score:  float64(blockNum),
		Member: deletionMarker,
	})

	// Also record in block-specific account index
	rs.pipeline.HSet(rs.ctx, fmt.Sprintf("block:%s:accounts", blockHash.Hex()), address.Hex(), deletionMarker)
}

// writeCode writes contract code to Redis
func (rs *RedisState) writeCode(codeHash libcommon.Hash, code []byte) {
	if !rs.enabled || len(code) == 0 {
		return
	}

	rs.pipelineMutex.Lock()
	defer rs.pipelineMutex.Unlock()

	// Store code by hash (only once, as code is immutable)
	key := fmt.Sprintf("code:%s", codeHash.Hex())
	rs.pipeline.SetNX(rs.ctx, key, code, 0) // Never expires
}

// writeStorage writes contract storage to Redis
func (rs *RedisState) writeStorage(blockNum uint64, blockHash libcommon.Hash, address libcommon.Address, key *libcommon.Hash, value *uint256.Int) {
	if !rs.enabled {
		return
	}

	rs.pipelineMutex.Lock()
	defer rs.pipelineMutex.Unlock()

	// Store storage slot in Redis
	redisKey := fmt.Sprintf("storage:%s:%s", address.Hex(), key.Hex())

	// Create storage entry
	var storageValue string
	if value.IsZero() {
		// For zero values, store a deletion marker
		storageValue = "null"
	} else {
		storageValue = "0x" + value.Hex()
	}

	// Store as a sorted set with block number as score
	rs.pipeline.ZAdd(rs.ctx, redisKey, redis.Z{
		Score:  float64(blockNum),
		Member: storageValue,
	})

	// Also store in block-specific storage index
	storageIndexKey := fmt.Sprintf("block:%s:storage:%s", blockHash.Hex(), address.Hex())
	rs.pipeline.HSet(rs.ctx, storageIndexKey, key.Hex(), storageValue)
}

// writeBlock writes block data to Redis
func (rs *RedisState) writeBlock(block *types.Header, blockHash libcommon.Hash) {
	if !rs.enabled || block == nil {
		return
	}

	blockNum := block.Number.Uint64()

	rs.pipelineMutex.Lock()
	defer rs.pipelineMutex.Unlock()

	// Set block data - indexed by block number
	blockKey := fmt.Sprintf("block:%d", blockNum)
	rs.pipeline.HSet(rs.ctx, blockKey, map[string]interface{}{
		"hash":       blockHash.Hex(),
		"parentHash": block.ParentHash.Hex(),
		"stateRoot":  block.Root.Hex(),
		"timestamp":  block.Time,
		"number":     blockNum,
	})

	// Map hash to block number and metadata
	hashKey := fmt.Sprintf("blockHash:%s", blockHash.Hex())
	rs.pipeline.HSet(rs.ctx, hashKey, map[string]interface{}{
		"number":    blockNum,
		"timestamp": block.Time,
	})

	// Add to canonical chain
	rs.pipeline.ZAdd(rs.ctx, "canonicalChain", redis.Z{
		Score:  float64(blockNum),
		Member: blockHash.Hex(),
	})
}

// writeTx writes transaction data to Redis
func (rs *RedisState) writeTx(blockNum uint64, blockHash libcommon.Hash, tx types.Transaction, txIndex int) {
	if !rs.enabled {
		return
	}

	// Get tx hash and sender
	txHash := tx.Hash()
	sender, ok := tx.GetSender()

	rs.pipelineMutex.Lock()
	defer rs.pipelineMutex.Unlock()

	// Store transaction data in Redis
	txData, err := rlp.EncodeToBytes(tx)
	if err != nil {
		rs.logger.Error("Failed to encode transaction", "hash", txHash, "error", err)
		return
	}

	// Store transaction directly in the block key
	blockTxsKey := fmt.Sprintf("txs:%d", blockNum)
	rs.pipeline.HSet(rs.ctx, blockTxsKey, fmt.Sprintf("%d", txIndex), string(txData))

	// Add transaction hash to block's transaction list for reference
	rs.pipeline.SAdd(rs.ctx, fmt.Sprintf("block:%d:txs", blockNum), txHash.Hex())

	// Map transaction hash to block number for lookups
	txBlockKey := fmt.Sprintf("tx:%s:block", txHash.Hex())
	rs.pipeline.Set(rs.ctx, txBlockKey, blockNum, 0)

	// Store sender index if available
	if ok {
		rs.pipeline.ZAdd(rs.ctx, fmt.Sprintf("sender:%s:txs", sender.Hex()), redis.Z{
			Score:  float64(blockNum),
			Member: txHash.Hex(),
		})
	}
}

// writeReceipt writes receipt data to Redis
func (rs *RedisState) writeReceipt(blockNum uint64, blockHash libcommon.Hash, receipt *types.Receipt) {
	if !rs.enabled || receipt == nil {
		return
	}

	rs.pipelineMutex.Lock()
	defer rs.pipelineMutex.Unlock()

	// Encode receipt to binary
	receiptData, err := rlp.EncodeToBytes(receipt)
	if err != nil {
		rs.logger.Error("Failed to encode receipt", "hash", receipt.TxHash, "error", err)
		return
	}

	// Store receipt directly in block-indexed hash
	blockReceiptsKey := fmt.Sprintf("receipts:%d", blockNum)
	rs.pipeline.HSet(rs.ctx, blockReceiptsKey, fmt.Sprintf("%d", receipt.TransactionIndex), string(receiptData))

	// Map receipt hash to block number for lookups
	receiptBlockKey := fmt.Sprintf("receipt:%s:block", receipt.TxHash.Hex())
	rs.pipeline.Set(rs.ctx, receiptBlockKey, blockNum, 0)

	// Add to block's receipts list (maintain reference by tx hash)
	rs.pipeline.ZAdd(rs.ctx, fmt.Sprintf("block:%d:receipts", blockNum), redis.Z{
		Score:  float64(receipt.TransactionIndex),
		Member: receipt.TxHash.Hex(),
	})
}

// logs have been removed from the Redis implementation
func (rs *RedisState) writeLog(blockNum uint64, blockHash libcommon.Hash, log *types.Log) {
	// No longer implemented - logs are not stored in Redis
	return
}

// handleReorg handles chain reorganization in Redis by completely deleting non-canonical data
func (rs *RedisState) handleReorg(reorgFromBlock uint64, newCanonicalHash libcommon.Hash) {
	if !rs.enabled {
		return
	}

	rs.logger.Info("Handling chain reorganization", "fromBlock", reorgFromBlock, "newCanonicalHash", newCanonicalHash.Hex())

	// Get old block hash
	oldBlockKey := fmt.Sprintf("block:%d", reorgFromBlock)
	oldHash, err := rs.client.HGet(rs.ctx, oldBlockKey, "hash").Result()
	if err != nil {
		rs.logger.Error("Failed to get block hash during reorg", "block", reorgFromBlock, "error", err)
		return
	}

	// Record reorg information for debugging
	reorgKey := fmt.Sprintf("reorg:%d", reorgFromBlock)
	rs.pipelineMutex.Lock()
	defer rs.pipelineMutex.Unlock()

	// Track reorg metadata
	rs.pipeline.HSet(rs.ctx, reorgKey, map[string]interface{}{
		"oldHash":   oldHash,
		"newHash":   newCanonicalHash.Hex(),
		"timestamp": time.Now().Unix(),
	})

	// Delete all block data from reorgFromBlock and higher
	for i := reorgFromBlock; i <= rs.currentBlock; i++ {
		rs.pipeline.Del(rs.ctx, fmt.Sprintf("block:%d", i))
		rs.pipeline.Del(rs.ctx, fmt.Sprintf("txs:%d", i))
		rs.pipeline.Del(rs.ctx, fmt.Sprintf("receipts:%d", i))
		rs.pipeline.Del(rs.ctx, fmt.Sprintf("block:%d:txs", i))
		rs.pipeline.Del(rs.ctx, fmt.Sprintf("block:%d:receipts", i))
	}

	// Delete the block hash entry
	rs.pipeline.Del(rs.ctx, fmt.Sprintf("blockHash:%s", oldHash))

	// Remove from canonical chain
	rs.pipeline.ZRemRangeByScore(rs.ctx, "canonicalChain",
		fmt.Sprintf("%f", float64(reorgFromBlock)), "+inf")

	// 1. Find and delete accounts at this block height and above
	rs.pipeline.Eval(rs.ctx, `
		local keys = redis.call('keys', 'account:*')
		for i, key in ipairs(keys) do
			redis.call('zremrangebyscore', key, ARGV[1], '+inf')
		end
		return #keys
	`, []string{}, fmt.Sprintf("%f", float64(reorgFromBlock)))

	// 2. Find and delete storage at this block height and above
	rs.pipeline.Eval(rs.ctx, `
		local keys = redis.call('keys', 'storage:*')
		for i, key in ipairs(keys) do
			redis.call('zremrangebyscore', key, ARGV[1], '+inf')
		end
		return #keys
	`, []string{}, fmt.Sprintf("%f", float64(reorgFromBlock)))

	// 3. Delete all tx and receipt mappings
	rs.pipeline.Eval(rs.ctx, `
		-- Find all tx mappings for blocks >= reorgFromBlock
		local txMappingKeys = redis.call('keys', 'tx:*:block')
		for i, key in ipairs(txMappingKeys) do
			local blockNum = tonumber(redis.call('get', key))
			if blockNum >= tonumber(ARGV[1]) then
				redis.call('del', key)
			end
		end

		-- Find all receipt mappings for blocks >= reorgFromBlock
		local receiptMappingKeys = redis.call('keys', 'receipt:*:block')
		for i, key in ipairs(receiptMappingKeys) do
			local blockNum = tonumber(redis.call('get', key))
			if blockNum >= tonumber(ARGV[1]) then
				redis.call('del', key)
			end
		end

		return #txMappingKeys + #receiptMappingKeys
	`, []string{}, fmt.Sprintf("%d", reorgFromBlock))

	// Update current block if needed
	rs.blockMutex.Lock()
	if reorgFromBlock <= rs.currentBlock {
		rs.currentBlock = reorgFromBlock - 1
		rs.pipeline.Set(rs.ctx, "currentBlock", rs.currentBlock, 0)
	}
	rs.blockMutex.Unlock()

	// Execute all deletion commands
	rs.FlushPipeline()

	rs.logger.Info("Chain reorganization completed", "fromBlock", reorgFromBlock)
}

// GetAccountAtBlock gets account state at a specific block number - O(1) since we delete non-canonical data
func (rs *RedisState) GetAccountAtBlock(address libcommon.Address, blockNum uint64) (*AccountJSON, error) {
	if !rs.enabled {
		return nil, fmt.Errorf("redis state not enabled")
	}

	// Get account data from the sorted set - always canonical since we delete non-canonical data
	key := fmt.Sprintf("account:%s", address.Hex())
	results, err := rs.client.ZRevRangeByScoreWithScores(rs.ctx, key, &redis.ZRangeBy{
		Min:    "-inf",
		Max:    fmt.Sprintf("%f", float64(blockNum)),
		Offset: 0,
		Count:  1,
	}).Result()

	if err != nil || len(results) == 0 {
		return nil, fmt.Errorf("account not found at block %d", blockNum)
	}

	// Parse the account data
	var accountJSON AccountJSON
	if err := json.Unmarshal([]byte(results[0].Member.(string)), &accountJSON); err != nil {
		return nil, fmt.Errorf("failed to unmarshal account data: %v", err)
	}

	return &accountJSON, nil
}

// GetStorageAtBlock gets storage state at a specific block number - O(1) since we delete non-canonical data
func (rs *RedisState) GetStorageAtBlock(address libcommon.Address, key libcommon.Hash, blockNum uint64) (string, error) {
	if !rs.enabled {
		return "", fmt.Errorf("redis state not enabled")
	}

	// Get storage data from the sorted set - always canonical since we delete non-canonical data
	storageKey := fmt.Sprintf("storage:%s:%s", address.Hex(), key.Hex())
	results, err := rs.client.ZRevRangeByScoreWithScores(rs.ctx, storageKey, &redis.ZRangeBy{
		Min:    "-inf",
		Max:    fmt.Sprintf("%f", float64(blockNum)),
		Offset: 0,
		Count:  1,
	}).Result()

	if err != nil || len(results) == 0 {
		return "", fmt.Errorf("storage not found at block %d", blockNum)
	}

	// Get the storage value
	storageValue := results[0].Member.(string)

	return storageValue, nil
}
