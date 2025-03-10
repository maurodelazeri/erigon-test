package state

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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
	// Track if BSSL module is available
	bsslAvailable bool
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

		// Check if BSSL module is loaded
		bsslAvailable := false
		_, err = client.Do(ctx, "BSSL.INFO", "test").Result()
		if err != nil {
			// If error contains "unknown command", the module isn't loaded
			if strings.Contains(err.Error(), "unknown command") {
				bsslAvailable = false
			} else {
				// Other errors (like wrong number of arguments) mean the command exists
				bsslAvailable = true
			}
		}

		if !bsslAvailable {
			logger.Warn("BSSL module not detected - using legacy sorted set approach")
		} else {
			logger.Info("BSSL module detected - using optimized skip list for blockchain state")
		}

		redisState = &RedisState{
			client:        client,
			enabled:       true,
			url:           redisURL,
			logger:        logger,
			ctx:           context.Background(),
			pipeline:      client.Pipeline(),
			bsslAvailable: bsslAvailable,
		}

		if !bsslAvailable {
			logger.Warn("BSSL module not detected in Redis - using legacy sorted set approach. Consider installing the BSSL module for better performance.")
		} else {
			logger.Info("BSSL module detected - using optimized skip list for blockchain state")
		}

		// Load current block if available
		val, err := client.Get(ctx, "currentBlock").Result()
		if err == nil {
			currentBlock, err := strconv.ParseUint(val, 10, 64)
			if err == nil {
				redisState.currentBlock = currentBlock
			}
		}

		logger.Info("Redis state integration initialized", "url", redisURL, "enabled", true, "bsslEnabled", bsslAvailable)
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

	if rs.bsslAvailable {
		// Use BSSL module for O(1) historical state
		rs.pipeline.Do(rs.ctx, "BSSL.SET",
			fmt.Sprintf("account:%s", address.Hex()),
			blockNum,
			string(jsonData))
	} else {
		// Legacy approach using sorted sets
		key := fmt.Sprintf("account:%s", address.Hex())
		rs.pipeline.ZAdd(rs.ctx, key, redis.Z{
			Score:  float64(blockNum),
			Member: string(jsonData),
		})
	}
}

// deleteAccount marks an account as deleted in Redis
func (rs *RedisState) deleteAccount(blockNum uint64, blockHash libcommon.Hash, address libcommon.Address) {
	if !rs.enabled {
		return
	}

	rs.pipelineMutex.Lock()
	defer rs.pipelineMutex.Unlock()

	// Store a deletion marker
	deletionMarker := "{\"deleted\":true}"

	if rs.bsslAvailable {
		// Use BSSL module for O(1) historical state
		rs.pipeline.Do(rs.ctx, "BSSL.SET",
			fmt.Sprintf("account:%s", address.Hex()),
			blockNum,
			deletionMarker)
	} else {
		// Legacy approach using sorted sets
		key := fmt.Sprintf("account:%s", address.Hex())
		rs.pipeline.ZAdd(rs.ctx, key, redis.Z{
			Score:  float64(blockNum),
			Member: deletionMarker,
		})
	}
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

	// Create storage key
	redisKey := fmt.Sprintf("storage:%s:%s", address.Hex(), key.Hex())

	// Create storage entry
	var storageValue string
	if value.IsZero() {
		// For zero values, store a deletion marker
		storageValue = "null"
	} else {
		storageValue = "0x" + value.Hex()
	}

	// Storage state json for BSSL
	storageJSON := fmt.Sprintf("{\"value\":\"%s\"}", storageValue)

	if rs.bsslAvailable {
		// Use BSSL module for O(1) historical state
		rs.pipeline.Do(rs.ctx, "BSSL.SET", redisKey, blockNum, storageJSON)
	} else {
		// Legacy approach using sorted sets
		rs.pipeline.ZAdd(rs.ctx, redisKey, redis.Z{
			Score:  float64(blockNum),
			Member: storageValue,
		})
	}
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

	// Get tx hash
	txHash := tx.Hash()

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

	// Add to block's receipts list (maintain reference by tx hash)
	rs.pipeline.ZAdd(rs.ctx, fmt.Sprintf("block:%d:receipts", blockNum), redis.Z{
		Score:  float64(receipt.TransactionIndex),
		Member: receipt.TxHash.Hex(),
	})
}

// handleReorg handles chain reorganization in Redis by completely deleting non-canonical data
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
		if err == redis.Nil {
			rs.logger.Warn("Block not found during reorg, proceeding anyway", "block", reorgFromBlock)
			oldHash = ""
		} else {
			rs.logger.Error("Failed to get block hash during reorg", "block", reorgFromBlock, "error", err)
			return
		}
	}

	// Create a context with timeout for the entire reorg operation
	ctx, cancel := context.WithTimeout(rs.ctx, 5*time.Minute)
	defer cancel()

	// Record reorg information for debugging
	reorgKey := fmt.Sprintf("reorg:%d", reorgFromBlock)
	rs.pipelineMutex.Lock()
	defer rs.pipelineMutex.Unlock()

	// Track reorg metadata
	rs.pipeline.HSet(ctx, reorgKey, map[string]interface{}{
		"oldHash":   oldHash,
		"newHash":   newCanonicalHash.Hex(),
		"timestamp": time.Now().Unix(),
	})

	// Delete all block data from reorgFromBlock and higher
	for i := reorgFromBlock; i <= rs.currentBlock; i++ {
		rs.pipeline.Del(ctx, fmt.Sprintf("block:%d", i))
		rs.pipeline.Del(ctx, fmt.Sprintf("txs:%d", i))
		rs.pipeline.Del(ctx, fmt.Sprintf("receipts:%d", i))
		rs.pipeline.Del(ctx, fmt.Sprintf("block:%d:txs", i))
		rs.pipeline.Del(ctx, fmt.Sprintf("block:%d:receipts", i))
	}

	// Delete the block hash entry if we found it
	if oldHash != "" {
		rs.pipeline.Del(ctx, fmt.Sprintf("blockHash:%s", oldHash))
	}

	// Remove from canonical chain
	rs.pipeline.ZRemRangeByScore(ctx, "canonicalChain",
		fmt.Sprintf("%f", float64(reorgFromBlock)), "+inf")

	// Execute the block deletion commands first
	_, pipeErr := rs.pipeline.Exec(ctx)
	if pipeErr != nil {
		rs.logger.Error("Failed to execute block deletion pipeline", "error", pipeErr)
		// Continue anyway to try to clean up state data
	}

	// Reset pipeline for state operations
	rs.pipeline = rs.client.Pipeline()

	if rs.bsslAvailable {
		// Handle state with BSSL module
		processKeysBatch := func(keyPrefix string, batchSize int64) {
			var cursor uint64 = 0
			for {
				// Use SCAN instead of KEYS for better reliability with large datasets
				keys, nextCursor, err := rs.client.Scan(ctx, cursor, keyPrefix+"*", batchSize).Result()
				if err != nil {
					rs.logger.Error("Error scanning keys", "prefix", keyPrefix, "error", err)
					break
				}

				// Process each key in this batch
				for _, key := range keys {
					// Get key info
					info, err := rs.client.Do(ctx, "BSSL.INFO", key).Result()
					if err != nil {
						rs.logger.Warn("Error getting info for key", "key", key, "error", err)
						continue
					}

					infoArr, ok := info.([]interface{})
					if !ok || len(infoArr) < 2 {
						rs.logger.Warn("Invalid info format", "key", key)
						continue
					}

					// Parse last block with full error handling
					lastBlockStr := fmt.Sprintf("%v", infoArr[1])
					lastBlock, parseErr := strconv.ParseUint(lastBlockStr, 10, 64)
					if parseErr != nil {
						rs.logger.Warn("Failed to parse last block", "key", key, "value", lastBlockStr, "error", parseErr)
						continue
					}

					// Check if this key is affected by the reorg
					if lastBlock >= reorgFromBlock {
						// Get state at block before reorg
						prevState, err := rs.client.Do(ctx, "BSSL.GETSTATEATBLOCK", key, reorgFromBlock-1).Result()
						if err != nil {
							if err != redis.Nil {
								rs.logger.Warn("Error getting state before reorg", "key", key, "error", err)
							}
							continue
						}

						if prevState != nil {
							// Set the state at reorg block
							rs.pipeline.Do(ctx, "BSSL.SET", key, reorgFromBlock, prevState)
						}
					}
				}

				// Execute any accumulated commands in pipeline periodically
				if rs.pipeline.Len() > 1000 {
					_, err := rs.pipeline.Exec(ctx)
					if err != nil {
						rs.logger.Error("Error executing pipeline during reorg", "error", err)
					}
					rs.pipeline = rs.client.Pipeline()
				}

				// Move to next batch
				cursor = nextCursor
				if cursor == 0 {
					break
				}
			}
		}

		// Process account and storage keys in batches of 100
		processKeysBatch("account:", 100)
		processKeysBatch("storage:", 100)
	} else {
		// Legacy approach with sorted sets - safer implementation
		processSortedSets := func(keyPrefix string, batchSize int64) {
			var cursor uint64 = 0
			for {
				// Use SCAN instead of KEYS
				keys, nextCursor, err := rs.client.Scan(ctx, cursor, keyPrefix+"*", batchSize).Result()
				if err != nil {
					rs.logger.Error("Error scanning keys", "prefix", keyPrefix, "error", err)
					break
				}

				// Remove data for each key
				for _, key := range keys {
					rs.pipeline.ZRemRangeByScore(ctx, key, fmt.Sprintf("%f", float64(reorgFromBlock)), "+inf")
				}

				// Execute pipeline periodically
				if rs.pipeline.Len() > 1000 {
					_, err := rs.pipeline.Exec(ctx)
					if err != nil {
						rs.logger.Error("Error executing pipeline during reorg", "error", err)
					}
					rs.pipeline = rs.client.Pipeline()
				}

				// Move to next batch
				cursor = nextCursor
				if cursor == 0 {
					break
				}
			}
		}

		// Process account and storage sorted sets in batches of 100
		processSortedSets("account:", 100)
		processSortedSets("storage:", 100)
	}

	// Update current block if needed
	rs.blockMutex.Lock()
	if reorgFromBlock <= rs.currentBlock {
		rs.currentBlock = reorgFromBlock - 1
		rs.pipeline.Set(ctx, "currentBlock", rs.currentBlock, 0)
	}
	rs.blockMutex.Unlock()

	// Execute any remaining commands
	_, pipeErr = rs.pipeline.Exec(ctx)
	if pipeErr != nil {
		rs.logger.Error("Failed to execute state cleanup pipeline", "error", pipeErr)
	}

	// Create a new pipeline for future operations
	rs.pipeline = rs.client.Pipeline()

	rs.logger.Info("Chain reorganization completed", "fromBlock", reorgFromBlock, "oldHash", oldHash, "newHash", newCanonicalHash.Hex())
}

// GetAccountAtBlock gets account state at a specific block number
func (rs *RedisState) GetAccountAtBlock(address libcommon.Address, blockNum uint64) (*AccountJSON, error) {
	if !rs.enabled {
		return nil, fmt.Errorf("redis state not enabled")
	}

	var accountData string

	if rs.bsslAvailable {
		// Use BSSL module for O(1) historical state
		result, err := rs.client.Do(rs.ctx, "BSSL.GETSTATEATBLOCK",
			fmt.Sprintf("account:%s", address.Hex()),
			blockNum).Result()

		if err != nil {
			return nil, fmt.Errorf("account lookup failed: %v", err)
		}

		if result == nil {
			return nil, fmt.Errorf("account not found at block %d", blockNum)
		}

		accountData = result.(string)
	} else {
		// Legacy approach using sorted sets
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

		accountData = results[0].Member.(string)
	}

	// Parse the account data
	var accountJSON AccountJSON
	if err := json.Unmarshal([]byte(accountData), &accountJSON); err != nil {
		return nil, fmt.Errorf("failed to unmarshal account data: %v", err)
	}

	return &accountJSON, nil
}

// GetStorageAtBlock gets storage state at a specific block number
func (rs *RedisState) GetStorageAtBlock(address libcommon.Address, key libcommon.Hash, blockNum uint64) (string, error) {
	if !rs.enabled {
		return "", fmt.Errorf("redis state not enabled")
	}

	storageKey := fmt.Sprintf("storage:%s:%s", address.Hex(), key.Hex())

	if rs.bsslAvailable {
		// Use BSSL module for O(1) historical state
		result, err := rs.client.Do(rs.ctx, "BSSL.GETSTATEATBLOCK", storageKey, blockNum).Result()

		if err != nil {
			return "", fmt.Errorf("storage lookup failed: %v", err)
		}

		if result == nil {
			return "", fmt.Errorf("storage not found at block %d", blockNum)
		}

		// Parse storage JSON
		var storageState struct {
			Value string `json:"value"`
		}

		if err := json.Unmarshal([]byte(result.(string)), &storageState); err != nil {
			return "", fmt.Errorf("failed to unmarshal storage data: %v", err)
		}

		return storageState.Value, nil
	} else {
		// Legacy approach using sorted sets
		results, err := rs.client.ZRevRangeByScoreWithScores(rs.ctx, storageKey, &redis.ZRangeBy{
			Min:    "-inf",
			Max:    fmt.Sprintf("%f", float64(blockNum)),
			Offset: 0,
			Count:  1,
		}).Result()

		if err != nil || len(results) == 0 {
			return "", fmt.Errorf("storage not found at block %d", blockNum)
		}

		return results[0].Member.(string), nil
	}
}
