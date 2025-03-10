package state

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
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

const (
	// Key prefixes for Redis storage
	accountPrefix   = "account:"
	storagePrefix   = "storage:"
	codePrefix      = "code:"
	blockPrefix     = "block:"
	blockHashPrefix = "blockHash:"
	txsPrefix       = "txs:"
	receiptsPrefix  = "receipts:"
	reorgPrefix     = "reorg:"

	// Timeouts
	redisConnectTimeout = 5 * time.Second
	redisReorgTimeout   = 5 * time.Minute
	redisBatchTimeout   = 30 * time.Second

	// Batch sizes
	redisReorgBatchSize = 100

	// Maximum number of commands to queue before flushing
	maxPipelineSize = 5000
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
	pipelineCount int
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
				enabled:       false,
				logger:        logger,
				ctx:           context.Background(),
				pipelineCount: 0,
			}
			return
		}

		// Parse Redis URL to extract address and DB
		opts, err := redis.ParseURL(redisURL)
		if err != nil {
			logger.Error("Invalid Redis URL format", "url", redisURL, "error", err)
			redisState = &RedisState{
				enabled:       false,
				logger:        logger,
				ctx:           context.Background(),
				pipelineCount: 0,
			}
			return
		}

		// Override password if provided
		if redisPassword != "" {
			opts.Password = redisPassword
		}

		// Configure reasonable defaults for Redis client
		opts.PoolSize = 20 // Increase connection pool size for better concurrency
		opts.MinIdleConns = 5
		opts.MaxRetries = 3

		client := redis.NewClient(opts)

		// Test connection
		ctx, cancel := context.WithTimeout(context.Background(), redisConnectTimeout)
		defer cancel()

		if _, err := client.Ping(ctx).Result(); err != nil {
			logger.Error("Failed to connect to Redis", "url", redisURL, "error", err)
			redisState = &RedisState{
				enabled:       false,
				logger:        logger,
				ctx:           context.Background(),
				pipelineCount: 0,
			}
			return
		}

		// Check if BSSL module is loaded
		bsslAvailable := false
		_, err = client.Do(ctx, "BSSL.PING").Result()
		if err != nil {
			// If error contains "unknown command", the module isn't loaded
			if strings.Contains(err.Error(), "unknown command") {
				bsslAvailable = false
			} else {
				// Other errors (like wrong number of arguments) mean the command exists
				bsslAvailable = true
			}
		} else {
			bsslAvailable = true
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
			pipelineCount: 0,
			bsslAvailable: bsslAvailable,
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

	// Ensure all commands are sent
	if err := rs.FlushPipeline(); err != nil {
		rs.logger.Error("Error flushing Redis pipeline during close", "error", err)
		// Continue closing anyway
	}

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
		rs.pipelineCount++
		rs.pipelineMutex.Unlock()

		// Auto-flush if needed
		if err := rs.AutoFlushPipeline(); err != nil {
			rs.logger.Error("Error auto-flushing pipeline", "error", err)
		}
	}
}

// FlushPipeline sends all queued commands to Redis
// This is exported to allow explicit flushing at commit points
func (rs *RedisState) FlushPipeline() error {
	if !rs.enabled {
		return nil
	}

	rs.pipelineMutex.Lock()
	defer rs.pipelineMutex.Unlock()

	// Skip if pipeline is empty
	if rs.pipelineCount == 0 {
		return nil
	}

	_, err := rs.pipeline.Exec(rs.ctx)
	if err != nil {
		rs.logger.Error("Failed to execute Redis pipeline", "error", err, "commands", rs.pipelineCount)
		// Create a new pipeline
		rs.pipeline = rs.client.Pipeline()
		rs.pipelineCount = 0
		return err
	}

	// Create a new pipeline
	rs.pipeline = rs.client.Pipeline()
	rs.pipelineCount = 0
	return nil
}

func (rs *RedisState) AutoFlushPipeline() error {
	rs.pipelineMutex.Lock()
	shouldFlush := rs.pipelineCount >= maxPipelineSize
	rs.pipelineMutex.Unlock()

	if shouldFlush {
		return rs.FlushPipeline()
	}
	return nil
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
	// Use BSSL module for O(1) historical state
	rs.pipeline.Do(rs.ctx, "BSSL.SET",
		fmt.Sprintf("%s%s", accountPrefix, address.Hex()),
		blockNum,
		string(jsonData))
	rs.pipelineCount++
	rs.pipelineMutex.Unlock()

	// Auto-flush if needed
	if err := rs.AutoFlushPipeline(); err != nil {
		rs.logger.Error("Error auto-flushing pipeline", "error", err)
	}
}

// deleteAccount marks an account as deleted in Redis
func (rs *RedisState) deleteAccount(blockNum uint64, blockHash libcommon.Hash, address libcommon.Address) {
	if !rs.enabled {
		return
	}

	rs.pipelineMutex.Lock()
	// Store a deletion marker
	deletionMarker := "{\"deleted\":true}"

	// Use BSSL module for O(1) historical state
	rs.pipeline.Do(rs.ctx, "BSSL.SET",
		fmt.Sprintf("%s%s", accountPrefix, address.Hex()),
		blockNum,
		deletionMarker)
	rs.pipelineCount++
	rs.pipelineMutex.Unlock()

	// Auto-flush if needed
	if err := rs.AutoFlushPipeline(); err != nil {
		rs.logger.Error("Error auto-flushing pipeline", "error", err)
	}
}

// writeCode writes contract code to Redis
func (rs *RedisState) writeCode(codeHash libcommon.Hash, code []byte) {
	if !rs.enabled || len(code) == 0 {
		return
	}

	rs.pipelineMutex.Lock()
	// Store code by hash (only once, as code is immutable)
	key := fmt.Sprintf("%s%s", codePrefix, codeHash.Hex())
	rs.pipeline.SetNX(rs.ctx, key, code, 0) // Never expires
	rs.pipelineCount++
	rs.pipelineMutex.Unlock()

	// Auto-flush if needed
	if err := rs.AutoFlushPipeline(); err != nil {
		rs.logger.Error("Error auto-flushing pipeline", "error", err)
	}
}

// writeStorage writes contract storage to Redis
// writeStorage writes contract storage to Redis
func (rs *RedisState) writeStorage(blockNum uint64, blockHash libcommon.Hash, address libcommon.Address, key *libcommon.Hash, value *uint256.Int) {
	if !rs.enabled {
		return
	}

	rs.pipelineMutex.Lock()
	// Create storage key
	redisKey := fmt.Sprintf("%s%s:%s", storagePrefix, address.Hex(), key.Hex())

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

	// Use BSSL module for O(1) historical state
	rs.pipeline.Do(rs.ctx, "BSSL.SET", redisKey, blockNum, storageJSON)
	rs.pipelineCount++
	rs.pipelineMutex.Unlock()

	// Auto-flush if needed
	if err := rs.AutoFlushPipeline(); err != nil {
		rs.logger.Error("Error auto-flushing pipeline", "error", err)
	}
}

// writeBlock writes block data to Redis
func (rs *RedisState) writeBlock(block *types.Header, blockHash libcommon.Hash) {
	if !rs.enabled || block == nil {
		return
	}

	blockNum := block.Number.Uint64()

	rs.pipelineMutex.Lock()
	// Set block data - indexed by block number
	blockKey := fmt.Sprintf("%s%d", blockPrefix, blockNum)
	rs.pipeline.HSet(rs.ctx, blockKey, map[string]interface{}{
		"hash":       blockHash.Hex(),
		"parentHash": block.ParentHash.Hex(),
		"stateRoot":  block.Root.Hex(),
		"timestamp":  block.Time,
		"number":     blockNum,
	})
	rs.pipelineCount++

	// Map hash to block number and metadata
	hashKey := fmt.Sprintf("%s%s", blockHashPrefix, blockHash.Hex())
	rs.pipeline.HSet(rs.ctx, hashKey, map[string]interface{}{
		"number":    blockNum,
		"timestamp": block.Time,
	})
	rs.pipelineCount++
	rs.pipelineMutex.Unlock()

	// Auto-flush if needed
	if err := rs.AutoFlushPipeline(); err != nil {
		rs.logger.Error("Error auto-flushing pipeline", "error", err)
	}
}

// writeTx writes transaction data to Redis
func (rs *RedisState) writeTx(blockNum uint64, blockHash libcommon.Hash, tx types.Transaction, txIndex int) {
	if !rs.enabled {
		return
	}

	// Get tx hash
	txHash := tx.Hash()

	rs.pipelineMutex.Lock()
	// Store transaction data in Redis
	txData, err := rlp.EncodeToBytes(tx)
	if err != nil {
		rs.logger.Error("Failed to encode transaction", "hash", txHash, "error", err)
		rs.pipelineMutex.Unlock()
		return
	}

	// Store transaction directly in the block key
	blockTxsKey := fmt.Sprintf("%s%d", txsPrefix, blockNum)
	rs.pipeline.HSet(rs.ctx, blockTxsKey, fmt.Sprintf("%d", txIndex), string(txData))
	rs.pipelineCount++
	rs.pipelineMutex.Unlock()

	// Auto-flush if needed
	if err := rs.AutoFlushPipeline(); err != nil {
		rs.logger.Error("Error auto-flushing pipeline", "error", err)
	}
}

// writeReceipt writes receipt data to Redis
func (rs *RedisState) writeReceipt(blockNum uint64, blockHash libcommon.Hash, receipt *types.Receipt) {
	if !rs.enabled || receipt == nil {
		return
	}

	rs.pipelineMutex.Lock()
	// Encode receipt to binary
	receiptData, err := rlp.EncodeToBytes(receipt)
	if err != nil {
		rs.logger.Error("Failed to encode receipt", "hash", receipt.TxHash, "error", err)
		rs.pipelineMutex.Unlock()
		return
	}

	// Store receipt directly in block-indexed hash
	blockReceiptsKey := fmt.Sprintf("%s%d", receiptsPrefix, blockNum)
	rs.pipeline.HSet(rs.ctx, blockReceiptsKey, fmt.Sprintf("%d", receipt.TransactionIndex), string(receiptData))
	rs.pipelineCount++
	rs.pipelineMutex.Unlock()

	// Auto-flush if needed
	if err := rs.AutoFlushPipeline(); err != nil {
		rs.logger.Error("Error auto-flushing pipeline", "error", err)
	}
}

// handleReorg handles chain reorganization in Redis with a focus on reliability
func (rs *RedisState) handleReorg(reorgFromBlock uint64, newCanonicalHash libcommon.Hash) {
	if !rs.enabled {
		return
	}

	rs.logger.Info("Handling chain reorganization", "fromBlock", reorgFromBlock, "newCanonicalHash", newCanonicalHash.Hex())

	// Get old block hash
	oldBlockKey := fmt.Sprintf("%s%d", blockPrefix, reorgFromBlock)
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
	ctx, cancel := context.WithTimeout(rs.ctx, redisReorgTimeout)
	defer cancel()

	// Record reorg information for debugging
	reorgKey := fmt.Sprintf("%s%d", reorgPrefix, reorgFromBlock)
	rs.pipelineMutex.Lock()

	// Track reorg metadata
	rs.pipeline.HSet(ctx, reorgKey, map[string]interface{}{
		"oldHash":   oldHash,
		"newHash":   newCanonicalHash.Hex(),
		"timestamp": time.Now().Unix(),
	})

	// Delete all block data from reorgFromBlock and higher
	for i := reorgFromBlock; i <= rs.currentBlock; i++ {
		rs.pipeline.Del(ctx, fmt.Sprintf("%s%d", blockPrefix, i))
		rs.pipeline.Del(ctx, fmt.Sprintf("%s%d", txsPrefix, i))
		rs.pipeline.Del(ctx, fmt.Sprintf("%s%d", receiptsPrefix, i))
		rs.pipeline.Del(ctx, fmt.Sprintf("%s%d:txs", blockPrefix, i))
		rs.pipeline.Del(ctx, fmt.Sprintf("%s%d:receipts", blockPrefix, i))
	}

	// Delete the block hash entry
	if oldHash != "" {
		rs.pipeline.Del(ctx, fmt.Sprintf("%s%s", blockHashPrefix, oldHash))
	}

	// Execute the block/transaction deletion commands first
	_, execErr := rs.pipeline.Exec(ctx)
	rs.pipeline = rs.client.Pipeline() // Create a fresh pipeline
	if execErr != nil {
		rs.logger.Error("Failed to execute block deletion pipeline", "error", execErr)
		// Continue anyway to try to clean up state
	}

	rs.handleReorgWithBSSL(ctx, reorgFromBlock)

	// Update current block if needed
	rs.blockMutex.Lock()
	if reorgFromBlock <= rs.currentBlock {
		rs.currentBlock = reorgFromBlock - 1
		rs.pipeline.Set(ctx, "currentBlock", rs.currentBlock, 0)
	}
	rs.blockMutex.Unlock()

	// Execute all remaining commands
	_, execErr = rs.pipeline.Exec(ctx)
	if execErr != nil {
		rs.logger.Error("Failed to execute state cleanup pipeline", "error", execErr)
	}

	// Create a fresh pipeline
	rs.pipeline = rs.client.Pipeline()
	rs.pipelineMutex.Unlock()

	rs.logger.Info("Chain reorganization completed", "fromBlock", reorgFromBlock)
}

// handleReorgWithBSSL handles chain reorganization for BSSL data
func (rs *RedisState) handleReorgWithBSSL(ctx context.Context, reorgFromBlock uint64) {
	rs.logger.Info("Handling reorg with BSSL module", "fromBlock", reorgFromBlock)

	// Helper function to process keys in batches using SCAN
	processKeysWithPrefix := func(prefix string) {
		processed := 0
		totalErrors := 0
		var cursor uint64 = 0

		for {
			// Create context with timeout for each batch
			batchCtx, cancel := context.WithTimeout(ctx, redisBatchTimeout)
			defer cancel()

			// Use SCAN instead of KEYS for better performance with large datasets
			keys, nextCursor, err := rs.client.Scan(batchCtx, cursor, prefix+"*", int64(redisReorgBatchSize)).Result()
			if err != nil {
				rs.logger.Error("Error scanning keys", "prefix", prefix, "error", err)
				break
			}

			// Create a pipeline for this batch
			pipe := rs.client.Pipeline()
			keysToProcess := make([]string, 0, len(keys))

			// First get info for all keys in batch
			for _, key := range keys {
				pipe.Do(batchCtx, "BSSL.INFO", key)
				keysToProcess = append(keysToProcess, key)
			}

			// Execute the info commands
			infoResults, err := pipe.Exec(batchCtx)
			if err != nil {
				rs.logger.Error("Error getting info for keys", "error", err)
				totalErrors++
				// Continue to next batch
				cursor = nextCursor
				if cursor == 0 {
					break
				}
				continue
			}

			// Process each key based on its info
			statePipe := rs.client.Pipeline()
			for i, result := range infoResults {
				if result.Err() != nil {
					rs.logger.Warn("Error getting info for key", "key", keysToProcess[i], "error", result.Err())
					totalErrors++
					continue
				}

				// Extract info data
				infoValue := extractRedisResult(result)
				if infoValue == nil {
					rs.logger.Warn("Could not extract value from Redis result", "key", keysToProcess[i])
					totalErrors++
					continue
				}

				info, ok := infoValue.([]interface{})
				if !ok || len(info) < 3 {
					rs.logger.Warn("Invalid info format", "key", keysToProcess[i], "info", infoValue)
					totalErrors++
					continue
				}

				// Parse last block
				lastBlockStr := fmt.Sprintf("%v", info[1])
				lastBlock, parseErr := strconv.ParseUint(lastBlockStr, 10, 64)
				if parseErr != nil {
					rs.logger.Warn("Failed to parse last block", "key", keysToProcess[i], "value", lastBlockStr, "error", parseErr)
					totalErrors++
					continue
				}

				// Only process keys that have data after the reorg point
				if lastBlock >= reorgFromBlock {
					// Get state at block before reorg
					statePipe.Do(batchCtx, "BSSL.GETSTATEATBLOCK", keysToProcess[i], reorgFromBlock-1)
				}
			}

			// Execute the state retrieval commands
			stateResults, err := statePipe.Exec(batchCtx)
			if err != nil {
				rs.logger.Error("Error getting states before reorg", "error", err)
				totalErrors++
				// Continue to next batch
				cursor = nextCursor
				if cursor == 0 {
					break
				}
				continue
			}

			// Now set the pre-reorg state at the reorg block
			updatePipe := rs.client.Pipeline()
			for i, result := range stateResults {
				if result.Err() != nil {
					if result.Err() != redis.Nil {
						rs.logger.Warn("Error getting state before reorg", "key", keysToProcess[i], "error", result.Err())
						totalErrors++
					}
					continue
				}

				// Extract state data
				state := extractRedisResult(result)
				if state == nil {
					continue
				}

				// Set the state at reorg block
				updatePipe.Do(batchCtx, "BSSL.SET", keysToProcess[i], reorgFromBlock, state)
			}

			// Execute the state update commands
			_, err = updatePipe.Exec(batchCtx)
			if err != nil {
				rs.logger.Error("Error updating states at reorg point", "error", err)
				totalErrors++
			}

			processed += len(keys)
			rs.logger.Debug("Processed keys in reorg", "prefix", prefix, "batch", len(keys), "total", processed)

			// Move to next batch
			cursor = nextCursor
			if cursor == 0 {
				break
			}
		}

		rs.logger.Info("Completed processing keys for reorg", "prefix", prefix, "processed", processed, "errors", totalErrors)
	}

	// Process account and storage keys
	processKeysWithPrefix(accountPrefix)
	processKeysWithPrefix(storagePrefix)
}

// Helper function to extract value from Redis result using reflection
func extractRedisResult(result redis.Cmder) interface{} {
	var resultValue interface{}

	// First try to get the value using reflection
	v := reflect.ValueOf(result)
	if v.Kind() == reflect.Ptr && !v.IsNil() {
		// Try to find a Val method
		valMethod := v.MethodByName("Val")
		if valMethod.IsValid() {
			vals := valMethod.Call(nil)
			if len(vals) > 0 {
				resultValue = vals[0].Interface()
				return resultValue
			}
		}
	}

	// If reflection didn't work, try direct type assertion
	cmd, ok := result.(*redis.Cmd)
	if ok {
		resultValue = cmd.Val()
		return resultValue
	}

	// Try other common Redis command types
	if stringCmd, ok := result.(*redis.StringCmd); ok {
		return stringCmd.Val()
	}
	if intCmd, ok := result.(*redis.IntCmd); ok {
		return intCmd.Val()
	}
	if zSliceCmd, ok := result.(*redis.ZSliceCmd); ok {
		return zSliceCmd.Val()
	}

	return nil
}

// GetAccountAtBlock gets account state at a specific block number
func (rs *RedisState) GetAccountAtBlock(address libcommon.Address, blockNum uint64) (*AccountJSON, error) {
	if !rs.enabled {
		return nil, fmt.Errorf("redis state not enabled")
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(rs.ctx, 5*time.Second)
	defer cancel()

	var accountData string

	// Use BSSL module for O(1) historical state
	result, err := rs.client.Do(ctx, "BSSL.GETSTATEATBLOCK",
		fmt.Sprintf("%s%s", accountPrefix, address.Hex()),
		blockNum).Result()

	if err != nil {
		if err == redis.Nil {
			return nil, fmt.Errorf("account not found at block %d", blockNum)
		}
		return nil, fmt.Errorf("account lookup failed: %v", err)
	}

	if result == nil {
		return nil, fmt.Errorf("account not found at block %d", blockNum)
	}

	accountData = result.(string)

	// Check for deletion marker
	if accountData == "{\"deleted\":true}" {
		return nil, fmt.Errorf("account deleted at block %d", blockNum)
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

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(rs.ctx, 5*time.Second)
	defer cancel()

	storageKey := fmt.Sprintf("%s%s:%s", storagePrefix, address.Hex(), key.Hex())

	// Use BSSL module for O(1) historical state
	result, err := rs.client.Do(ctx, "BSSL.GETSTATEATBLOCK", storageKey, blockNum).Result()

	if err != nil {
		if err == redis.Nil {
			return "", fmt.Errorf("storage not found at block %d", blockNum)
		}
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

}

// GetBlockByNumber gets block data by block number
func (rs *RedisState) GetBlockByNumber(blockNum uint64) (map[string]string, error) {
	if !rs.enabled {
		return nil, fmt.Errorf("redis state not enabled")
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(rs.ctx, 5*time.Second)
	defer cancel()

	// Get block data
	blockKey := fmt.Sprintf("%s%d", blockPrefix, blockNum)
	blockData, err := rs.client.HGetAll(ctx, blockKey).Result()

	if err != nil {
		return nil, fmt.Errorf("block lookup failed: %v", err)
	}

	if len(blockData) == 0 {
		return nil, fmt.Errorf("block %d not found", blockNum)
	}

	return blockData, nil
}

// GetTransactionsByBlockNumber gets all transactions in a block
func (rs *RedisState) GetTransactionsByBlockNumber(blockNum uint64) (map[string]string, error) {
	if !rs.enabled {
		return nil, fmt.Errorf("redis state not enabled")
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(rs.ctx, 5*time.Second)
	defer cancel()

	// Get transaction data
	txKey := fmt.Sprintf("%s%d", txsPrefix, blockNum)
	txData, err := rs.client.HGetAll(ctx, txKey).Result()

	if err != nil {
		return nil, fmt.Errorf("transaction lookup failed: %v", err)
	}

	return txData, nil
}

// GetTransactionHashesByBlockNumber gets all transaction hashes in a block
func (rs *RedisState) GetTransactionHashesByBlockNumber(blockNum uint64) ([]string, error) {
	if !rs.enabled {
		return nil, fmt.Errorf("redis state not enabled")
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(rs.ctx, 5*time.Second)
	defer cancel()

	// Get transaction hashes
	txHashesKey := fmt.Sprintf("%s%d:txs", blockPrefix, blockNum)
	txHashes, err := rs.client.SMembers(ctx, txHashesKey).Result()

	if err != nil {
		return nil, fmt.Errorf("transaction hashes lookup failed: %v", err)
	}

	return txHashes, nil
}

// GetReceiptsByBlockNumber gets all receipts in a block
func (rs *RedisState) GetReceiptsByBlockNumber(blockNum uint64) (map[string]string, error) {
	if !rs.enabled {
		return nil, fmt.Errorf("redis state not enabled")
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(rs.ctx, 5*time.Second)
	defer cancel()

	// Get receipt data
	receiptKey := fmt.Sprintf("%s%d", receiptsPrefix, blockNum)
	receiptData, err := rs.client.HGetAll(ctx, receiptKey).Result()

	if err != nil {
		return nil, fmt.Errorf("receipt lookup failed: %v", err)
	}

	return receiptData, nil
}

// GetCode gets contract code by code hash
func (rs *RedisState) GetCode(codeHash libcommon.Hash) ([]byte, error) {
	if !rs.enabled {
		return nil, fmt.Errorf("redis state not enabled")
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(rs.ctx, 5*time.Second)
	defer cancel()

	// Get code data
	codeKey := fmt.Sprintf("%s%s", codePrefix, codeHash.Hex())
	code, err := rs.client.Get(ctx, codeKey).Bytes()

	if err != nil {
		if err == redis.Nil {
			return nil, fmt.Errorf("code not found for hash %s", codeHash.Hex())
		}
		return nil, fmt.Errorf("code lookup failed: %v", err)
	}

	return code, nil
}

// GetLatestBlockNumber gets the latest processed block number
func (rs *RedisState) GetLatestBlockNumber() (uint64, error) {
	if !rs.enabled {
		return 0, fmt.Errorf("redis state not enabled")
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(rs.ctx, 5*time.Second)
	defer cancel()

	// Get current block
	currentBlockStr, err := rs.client.Get(ctx, "currentBlock").Result()

	if err != nil {
		if err == redis.Nil {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to get latest block number: %v", err)
	}

	currentBlock, err := strconv.ParseUint(currentBlockStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse latest block number: %v", err)
	}

	return currentBlock, nil
}
