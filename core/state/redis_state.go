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
	redisConnectTimeout   = 5 * time.Second
	redisOperationTimeout = 15 * time.Second // Increased timeout
	redisReorgTimeout     = 5 * time.Minute
	redisBatchTimeout     = 30 * time.Second

	// Batch sizes
	redisReorgBatchSize = 100

	// Maximum number of commands to queue before flushing
	maxPipelineSize = 1000 // Reduced from 5000
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
	bsslAvailable bool

	// Block transaction and receipt collections
	blockTxs       map[uint64][][]byte // Map of blockNum -> list of encoded transactions
	blockReceipts  map[uint64][][]byte // Map of blockNum -> list of encoded receipts
	blockDataMutex sync.Mutex          // Mutex for accessing block data maps
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

// BlockJSON represents block metadata stored in Redis
type BlockJSON struct {
	Hash       string `json:"hash"`
	ParentHash string `json:"parentHash"`
	StateRoot  string `json:"stateRoot"`
	Timestamp  uint64 `json:"timestamp"`
	Number     uint64 `json:"number"`
}

// BlockHashJSON represents the block hash to block number mapping
type BlockHashJSON struct {
	Number    uint64 `json:"number"`
	Timestamp uint64 `json:"timestamp"`
}

// ReorgJSON represents reorg metadata
type ReorgJSON struct {
	OldHash   string `json:"oldHash"`
	NewHash   string `json:"newHash"`
	Timestamp int64  `json:"timestamp"`
}

// Initialize creates or returns the global RedisState instance
func InitRedisState(redisURL string, redisPassword string, logger log.Logger) (*RedisState, error) {
	var initErr error
	redisInitOnce.Do(func() {
		if redisURL == "" {
			initErr = fmt.Errorf("Redis URL is required, cannot initialize Redis state with empty URL")
			return
		}

		// Parse Redis URL to extract address and DB
		opts, err := redis.ParseURL(redisURL)
		if err != nil {
			initErr = fmt.Errorf("invalid Redis URL format: %w", err)
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
		opts.ReadTimeout = redisOperationTimeout
		opts.WriteTimeout = redisOperationTimeout
		opts.DialTimeout = redisConnectTimeout
		opts.PoolTimeout = redisOperationTimeout * 2

		client := redis.NewClient(opts)

		// Test connection
		ctx, cancel := context.WithTimeout(context.Background(), redisConnectTimeout)
		defer cancel()

		if _, err := client.Ping(ctx).Result(); err != nil {
			initErr = fmt.Errorf("failed to connect to Redis: %w", err)
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
			blockTxs:      make(map[uint64][][]byte),
			blockReceipts: make(map[uint64][][]byte),
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

	if initErr != nil {
		return nil, initErr
	}
	return redisState, nil
}

// GetRedisState returns the global RedisState instance
func GetRedisState() *RedisState {
	if redisState == nil {
		// If Redis is not initialized, this is a critical error
		// This will ensure that operations cannot proceed without Redis
		panic("Redis state not initialized. Redis is required for operation.")
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

// BeginBlockProcessing starts block processing in Redis
func (rs *RedisState) BeginBlockProcessing(blockNum uint64) error {
	// We know rs.enabled is always true now with our changes

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
			rs.logger.Error("Failed to update block processing state in Redis", "block", blockNum, "error", err)
			return fmt.Errorf("redis error in BeginBlockProcessing: %w", err)
		}
	}

	return nil
}

// beginBlockProcessing starts block processing in Redis - legacy name for backwards compatibility
func (rs *RedisState) beginBlockProcessing(blockNum uint64) error {
	return rs.BeginBlockProcessing(blockNum)
}

// FinishBlockProcessing writes all collected block data to Redis
func (rs *RedisState) FinishBlockProcessing(blockNum uint64) error {
	// First check if the block header exists
	// If we're calling FinishBlockProcessing but the block header doesn't exist in Redis,
	// this is now a critical error since we require Redis to be consistent
	ctx, cancel := context.WithTimeout(rs.ctx, 5*time.Second)
	defer cancel()

	// Use MULTI/EXEC for atomicity
	pipe := rs.client.TxPipeline()

	blockExists, err := rs.client.Exists(ctx, fmt.Sprintf("%s%d", blockPrefix, blockNum)).Result()
	if err != nil {
		return fmt.Errorf("error checking if block exists in Redis: %w", err)
	} else if blockExists == 0 {
		return fmt.Errorf("critical consistency error: block %d header doesn't exist in Redis but trying to finalize. Block headers must be written before finalization", blockNum)
	}

	// Check if we should write empty arrays for blocks without transactions
	var writeTxs, writeReceipts bool

	rs.blockDataMutex.Lock()
	// Check if we have any transaction or receipt data for this block
	txs, hasTxs := rs.blockTxs[blockNum]
	receipts, hasReceipts := rs.blockReceipts[blockNum]

	// Important consistency check: If we have transactions but no receipts, or vice versa,
	// we need to make sure that both collections have entries for consistency
	if hasTxs && !hasReceipts {
		rs.logger.Debug("Block has transactions but no receipts in memory", "block", blockNum, "txCount", len(txs))
		// Create an empty receipts array of the same length as transactions
		rs.blockReceipts[blockNum] = make([][]byte, len(txs))
		receipts = rs.blockReceipts[blockNum]
		hasReceipts = true
	} else if !hasTxs && hasReceipts {
		rs.logger.Debug("Block has receipts but no transactions in memory", "block", blockNum, "receiptCount", len(receipts))
		// Create an empty transactions array of the same length as receipts
		rs.blockTxs[blockNum] = make([][]byte, len(receipts))
		txs = rs.blockTxs[blockNum]
		hasTxs = true
	}

	// We should write transactions if we have them OR if this is the first time finalizing this block
	// This ensures that blocks without transactions still get an empty array entry
	writeTxs = hasTxs
	writeReceipts = hasReceipts

	// If we don't have any data but it's the first time processing this block,
	// we'll still write empty arrays to ensure we have entries for all blocks
	if !hasTxs && !hasReceipts {
		// Check if we already have these keys by seeing if we've processed this block before

		// Check if transactions key exists
		txsExists, err := rs.client.Exists(ctx, fmt.Sprintf("%s%d", txsPrefix, blockNum)).Result()
		if err != nil {
			rs.blockDataMutex.Unlock()
			return fmt.Errorf("error checking if transactions exist in Redis: %w", err)
		}

		receiptsExists, err := rs.client.Exists(ctx, fmt.Sprintf("%s%d", receiptsPrefix, blockNum)).Result()
		if err != nil {
			rs.blockDataMutex.Unlock()
			return fmt.Errorf("error checking if receipts exist in Redis: %w", err)
		}

		// Always write both or neither for consistency
		shouldWrite := (txsExists == 0) || (receiptsExists == 0)

		// If either key doesn't exist, write both
		writeTxs = writeTxs || shouldWrite
		writeReceipts = writeReceipts || shouldWrite
	}

	// If there's nothing to do, return early
	if !writeTxs && !writeReceipts {
		rs.blockDataMutex.Unlock()
		return nil
	}

	// Start Redis transaction to ensure atomicity
	_, err = pipe.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		// Write transactions for the block
		if writeTxs {
			// Default to empty array if we don't have any transactions
			var txsData []byte
			var err error

			if hasTxs {
				// Encode the transactions array
				txsData, err = json.Marshal(txs)
			} else {
				// Create an empty array
				txsData, err = json.Marshal([][]byte{})
			}

			if err != nil {
				return fmt.Errorf("failed to encode block transactions array: %w", err)
			}

			// Write the block's transaction array
			blockTxsKey := fmt.Sprintf("%s%d", txsPrefix, blockNum)
			pipe.Set(ctx, blockTxsKey, string(txsData), 0) // Never expires
		}

		// Write receipts for the block
		if writeReceipts {
			// Default to empty array if we don't have any receipts
			var receiptsData []byte
			var err error

			if hasReceipts {
				// Encode the receipts array
				receiptsData, err = json.Marshal(receipts)
			} else {
				// Create an empty array
				receiptsData, err = json.Marshal([][]byte{})
			}

			if err != nil {
				return fmt.Errorf("failed to encode block receipts array: %w", err)
			}

			// Write the block's receipt array
			blockReceiptsKey := fmt.Sprintf("%s%d", receiptsPrefix, blockNum)
			pipe.Set(ctx, blockReceiptsKey, string(receiptsData), 0) // Never expires
		}

		return nil
	})

	// Clean up memory after we've encoded the data
	delete(rs.blockTxs, blockNum)
	delete(rs.blockReceipts, blockNum)
	rs.blockDataMutex.Unlock()

	// Execute the transaction
	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to atomically write block data to Redis: %w", err)
	}

	return nil
}

// FlushPipeline sends all queued commands to Redis
// This is exported to allow explicit flushing at commit points
func (rs *RedisState) FlushPipeline() error {
	rs.pipelineMutex.Lock()
	defer rs.pipelineMutex.Unlock()

	// Skip if pipeline is empty
	if rs.pipelineCount == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(rs.ctx, redisOperationTimeout)
	defer cancel()

	rs.logger.Debug("Flushing Redis pipeline", "commands", rs.pipelineCount)
	_, err := rs.pipeline.Exec(ctx)
	if err != nil {
		rs.logger.Error("Failed to execute Redis pipeline", "error", err, "commands", rs.pipelineCount)
		// Create a new pipeline even on error
		rs.pipeline = rs.client.Pipeline()
		rs.pipelineCount = 0
		return fmt.Errorf("redis pipeline execution error: %w", err)
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
func (rs *RedisState) writeAccount(blockNum uint64, blockHash libcommon.Hash, address libcommon.Address, account *accounts.Account) error {
	if account == nil || !rs.enabled {
		return nil
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
		return fmt.Errorf("failed to marshal account data for Redis: %w", err)
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
		return fmt.Errorf("failed to flush account data to Redis: %w", err)
	}

	return nil
}

// deleteAccount marks an account as deleted in Redis
func (rs *RedisState) deleteAccount(blockNum uint64, blockHash libcommon.Hash, address libcommon.Address) error {
	if !rs.enabled {
		return nil
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
		return fmt.Errorf("failed to flush account deletion to Redis: %w", err)
	}

	return nil
}

// writeCode writes contract code to Redis
func (rs *RedisState) writeCode(codeHash libcommon.Hash, code []byte) error {
	if len(code) == 0 || !rs.enabled {
		return nil
	}

	rs.pipelineMutex.Lock()
	// Store code by hash (only once, as code is immutable)
	key := fmt.Sprintf("%s%s", codePrefix, codeHash.Hex())
	rs.pipeline.SetNX(rs.ctx, key, code, 0) // Never expires
	rs.pipelineCount++
	rs.pipelineMutex.Unlock()

	// Auto-flush if needed
	if err := rs.AutoFlushPipeline(); err != nil {
		return fmt.Errorf("failed to flush code to Redis: %w", err)
	}

	return nil
}

// writeStorage writes contract storage to Redis
func (rs *RedisState) writeStorage(blockNum uint64, blockHash libcommon.Hash, address libcommon.Address, key *libcommon.Hash, value *uint256.Int) error {
	if !rs.enabled {
		return nil
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
		return fmt.Errorf("failed to flush storage to Redis: %w", err)
	}

	return nil
}

// WriteBlock writes block data to Redis
func (rs *RedisState) WriteBlock(block *types.Header, blockHash libcommon.Hash) error {
	if block == nil || !rs.enabled {
		return nil
	}

	blockNum := block.Number.Uint64()

	// During snapshot sync, we want to ensure block headers are immediately written
	// and confirmed in Redis, especially since FinishBlockProcessing checks for them
	// First encode the block header
	blockData, err := rlp.EncodeToBytes(block)
	if err != nil {
		rs.logger.Error("Failed to encode block", "blockHash", blockHash, "error", err)
		return fmt.Errorf("failed to encode block for Redis: %w", err)
	}

	// For snapshot sync and critical operations, we use a direct Redis write
	// instead of the pipeline to ensure immediate persistence
	ctx, cancel := context.WithTimeout(rs.ctx, redisOperationTimeout)
	defer cancel()

	// Directly write the block header to Redis with immediate effect
	blockKey := fmt.Sprintf("%s%d", blockPrefix, blockNum)
	err = rs.client.Set(ctx, blockKey, string(blockData), 0).Err()
	if err != nil {
		rs.logger.Error("Failed to write block header to Redis", "block", blockNum, "hash", blockHash.Hex(), "error", err)
		return fmt.Errorf("failed to write block header to Redis: %w", err)
	}

	// Also store block hash mapping for reverse lookups
	blockHashJSON := BlockHashJSON{
		Number:    blockNum,
		Timestamp: block.Time,
	}
	hashData, err := json.Marshal(blockHashJSON)
	if err != nil {
		rs.logger.Error("Failed to marshal block hash data", "blockHash", blockHash, "error", err)
		// Continue even if block hash mapping fails - it's less critical than the header itself
	} else {
		hashKey := fmt.Sprintf("%s%s", blockHashPrefix, blockHash.Hex())
		if err = rs.client.Set(ctx, hashKey, string(hashData), 0).Err(); err != nil {
			rs.logger.Warn("Failed to write block hash mapping to Redis", "block", blockNum, "hash", blockHash.Hex(), "error", err)
			// Continue even if this fails - it's not critical
		}
	}

	rs.logger.Debug("Successfully wrote block header to Redis", "block", blockNum, "hash", blockHash.Hex())
	return nil
}

// Legacy method for backwards compatibility
func (rs *RedisState) writeBlock(block *types.Header, blockHash libcommon.Hash) error {
	return rs.WriteBlock(block, blockHash)
}

// WriteTx collects transaction data for batch writing per block
func (rs *RedisState) WriteTx(blockNum uint64, blockHash libcommon.Hash, tx types.Transaction, txIndex int) error {
	if !rs.enabled {
		return nil
	}

	// Safety check for null transaction
	if tx == nil {
		rs.logger.Warn("Received nil transaction to write to Redis", "block", blockNum, "index", txIndex)
		return nil // Skip this transaction but don't return an error
	}

	// Get tx hash for logging
	txHash := tx.Hash()

	// Encode transaction
	txData, err := rlp.EncodeToBytes(tx)
	if err != nil {
		rs.logger.Error("Failed to encode transaction", "hash", txHash, "error", err)
		return fmt.Errorf("failed to encode transaction for Redis: %w", err)
	}

	// Add transaction to the block's collection
	rs.blockDataMutex.Lock()
	defer rs.blockDataMutex.Unlock()

	// Ensure the slice is large enough
	if txs, exists := rs.blockTxs[blockNum]; !exists || len(txs) <= txIndex {
		// Create or resize the slice to accommodate the transaction index
		newSize := txIndex + 1
		if exists {
			// If the slice exists but is too small, make it bigger
			if len(txs) < newSize {
				newTxs := make([][]byte, newSize)
				copy(newTxs, txs)
				rs.blockTxs[blockNum] = newTxs
			}
		} else {
			// Create a new slice if none exists
			rs.blockTxs[blockNum] = make([][]byte, newSize)
		}
	}

	// Store the transaction at the correct index
	rs.blockTxs[blockNum][txIndex] = txData

	return nil
}

// Legacy method for backwards compatibility
func (rs *RedisState) writeTx(blockNum uint64, blockHash libcommon.Hash, tx types.Transaction, txIndex int) error {
	return rs.WriteTx(blockNum, blockHash, tx, txIndex)
}

// WriteReceipt collects receipt data for batch writing per block
func (rs *RedisState) WriteReceipt(blockNum uint64, blockHash libcommon.Hash, receipt *types.Receipt) error {
	if !rs.enabled {
		return nil
	}

	// Safety check for null receipt
	if receipt == nil {
		rs.logger.Warn("Received nil receipt to write to Redis", "block", blockNum)
		return nil // Skip this receipt but don't return an error
	}

	// Encode receipt to binary
	receiptData, err := rlp.EncodeToBytes(receipt)
	if err != nil {
		rs.logger.Error("Failed to encode receipt", "hash", receipt.TxHash, "error", err)
		return fmt.Errorf("failed to encode receipt for Redis: %w", err)
	}

	// Add receipt to the block's collection
	rs.blockDataMutex.Lock()
	defer rs.blockDataMutex.Unlock()

	txIndex := receipt.TransactionIndex
	// Ensure the slice is large enough
	if receipts, exists := rs.blockReceipts[blockNum]; !exists || len(receipts) <= int(txIndex) {
		// Create or resize the slice to accommodate the receipt index
		newSize := int(txIndex) + 1
		if exists {
			// If the slice exists but is too small, make it bigger
			if len(receipts) < newSize {
				newReceipts := make([][]byte, newSize)
				copy(newReceipts, receipts)
				rs.blockReceipts[blockNum] = newReceipts
			}
		} else {
			// Create a new slice if none exists
			rs.blockReceipts[blockNum] = make([][]byte, newSize)
		}
	}

	// Store the receipt at the correct index
	rs.blockReceipts[blockNum][txIndex] = receiptData

	return nil
}

// Legacy method for backwards compatibility
func (rs *RedisState) writeReceipt(blockNum uint64, blockHash libcommon.Hash, receipt *types.Receipt) error {
	return rs.WriteReceipt(blockNum, blockHash, receipt)
}

// handleReorg handles chain reorganization in Redis with a focus on reliability and atomicity
func (rs *RedisState) handleReorg(reorgFromBlock uint64, newCanonicalHash libcommon.Hash) error {
	rs.logger.Info("Handling chain reorganization", "fromBlock", reorgFromBlock, "newCanonicalHash", newCanonicalHash.Hex())

	// Create a context with timeout for the operation to fetch old block hash
	ctx, cancel := context.WithTimeout(rs.ctx, redisOperationTimeout)
	defer cancel()

	// Get old block hash
	oldBlockKey := fmt.Sprintf("%s%d", blockPrefix, reorgFromBlock)
	oldBlockData, err := rs.client.Get(ctx, oldBlockKey).Result()
	var oldHash string

	if err != nil {
		if err == redis.Nil {
			rs.logger.Warn("Block not found during reorg, proceeding anyway", "block", reorgFromBlock)
			oldHash = ""
		} else {
			rs.logger.Error("Failed to get block data during reorg", "block", reorgFromBlock, "error", err)
			return fmt.Errorf("failed to get block data during reorg: %w", err)
		}
	} else {
		// Parse the RLP encoded block to get its hash
		var block types.Header
		if err = rlp.DecodeBytes([]byte(oldBlockData), &block); err != nil {
			rs.logger.Error("Failed to decode block during reorg", "block", reorgFromBlock, "error", err)
			oldHash = ""
		} else {
			oldHash = block.Hash().Hex()
		}
	}

	// Check if the current block is far ahead of the reorg point, which could indicate
	// we're doing a reorg during or after a snapshot sync. In that case, we should be
	// more careful about deleting data.
	rs.blockMutex.RLock()
	currentBlock := rs.currentBlock
	rs.blockMutex.RUnlock()

	if currentBlock > reorgFromBlock+1000 {
		rs.logger.Warn("Large gap between reorg point and current block",
			"reorgBlock", reorgFromBlock,
			"currentBlock", currentBlock,
			"gap", currentBlock-reorgFromBlock)
	}

	// Create a context with timeout for the entire reorg operation
	ctx, cancel = context.WithTimeout(rs.ctx, redisReorgTimeout)
	defer cancel()

	// Use MULTI/EXEC for atomicity
	pipe := rs.client.TxPipeline()

	// Record reorg information for debugging
	reorgKey := fmt.Sprintf("%s%d", reorgPrefix, reorgFromBlock)

	// Track reorg metadata
	reorgJSON := ReorgJSON{
		OldHash:   oldHash,
		NewHash:   newCanonicalHash.Hex(),
		Timestamp: time.Now().Unix(),
	}

	reorgData, err := json.Marshal(reorgJSON)
	if err != nil {
		rs.logger.Error("Failed to marshal reorg data", "error", err)
		return fmt.Errorf("failed to marshal reorg data: %w", err)
	}

	// Start a transaction for metadata and block deletion
	_, err = pipe.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		// Store reorg metadata
		pipe.Set(ctx, reorgKey, string(reorgData), 0)

		// Delete all block data from reorgFromBlock and higher in a single atomic transaction
		var keysToDelete []string

		// Find all keys that need to be deleted
		for i := reorgFromBlock; i <= currentBlock; i++ {
			blockKey := fmt.Sprintf("%s%d", blockPrefix, i)
			blockTxsKey := fmt.Sprintf("%s%d", txsPrefix, i)
			blockReceiptsKey := fmt.Sprintf("%s%d", receiptsPrefix, i)

			// Get existence information for all keys at once
			exists, err := rs.client.Exists(ctx, blockKey, blockTxsKey, blockReceiptsKey).Result()
			if err != nil {
				return fmt.Errorf("failed to check key existence during reorg: %w", err)
			}

			if exists > 0 {
				// Check individual keys
				if exists, _ := rs.client.Exists(ctx, blockKey).Result(); exists > 0 {
					keysToDelete = append(keysToDelete, blockKey)
				}

				if exists, _ := rs.client.Exists(ctx, blockTxsKey).Result(); exists > 0 {
					keysToDelete = append(keysToDelete, blockTxsKey)
				}

				if exists, _ := rs.client.Exists(ctx, blockReceiptsKey).Result(); exists > 0 {
					keysToDelete = append(keysToDelete, blockReceiptsKey)
				}
			}
		}

		// Delete all block hash entries
		if oldHash != "" {
			keysToDelete = append(keysToDelete, fmt.Sprintf("%s%s", blockHashPrefix, oldHash))
		}

		// Delete all keys in a single operation if any exist
		if len(keysToDelete) > 0 {
			pipe.Del(ctx, keysToDelete...)
		}

		// Update current block
		rs.blockMutex.Lock()
		if reorgFromBlock <= rs.currentBlock {
			rs.currentBlock = reorgFromBlock - 1
			pipe.Set(ctx, "currentBlock", rs.currentBlock, 0)
		}
		rs.blockMutex.Unlock()

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to execute atomic block deletion during reorg: %w", err)
	}

	// Handle BSSL data separately (since it has its own batch processing)
	err = rs.handleReorgWithBSSL(ctx, reorgFromBlock)
	if err != nil {
		return fmt.Errorf("failed to handle BSSL data during reorg: %w", err)
	}

	rs.logger.Info("Chain reorganization completed", "fromBlock", reorgFromBlock)
	return nil
}

// handleReorgWithBSSL handles chain reorganization for BSSL data
func (rs *RedisState) handleReorgWithBSSL(ctx context.Context, reorgFromBlock uint64) error {
	rs.logger.Info("Handling reorg with BSSL module", "fromBlock", reorgFromBlock)

	// Helper function to process keys in batches using SCAN
	processKeysWithPrefix := func(prefix string) error {
		processed := 0
		var cursor uint64 = 0

		for {
			// Create context with timeout for each batch
			batchCtx, cancel := context.WithTimeout(ctx, redisBatchTimeout)
			defer cancel()

			// Use SCAN instead of KEYS for better performance with large datasets
			keys, nextCursor, err := rs.client.Scan(batchCtx, cursor, prefix+"*", int64(redisReorgBatchSize)).Result()
			if err != nil {
				rs.logger.Error("Error scanning keys", "prefix", prefix, "error", err)
				return fmt.Errorf("failed to scan keys with prefix %s: %w", prefix, err)
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
				return fmt.Errorf("failed to get info for keys: %w", err)
			}

			// Process each key based on its info
			statePipe := rs.client.Pipeline()
			for i, result := range infoResults {
				if result.Err() != nil {
					rs.logger.Warn("Error getting info for key", "key", keysToProcess[i], "error", result.Err())
					continue
				}

				// Extract info data
				infoValue := extractRedisResult(result)
				if infoValue == nil {
					rs.logger.Warn("Could not extract value from Redis result", "key", keysToProcess[i])
					continue
				}

				info, ok := infoValue.([]interface{})
				if !ok || len(info) < 3 {
					rs.logger.Warn("Invalid info format", "key", keysToProcess[i], "info", infoValue)
					continue
				}

				// Parse last block
				lastBlockStr := fmt.Sprintf("%v", info[1])
				lastBlock, parseErr := strconv.ParseUint(lastBlockStr, 10, 64)
				if parseErr != nil {
					rs.logger.Warn("Failed to parse last block", "key", keysToProcess[i], "value", lastBlockStr, "error", parseErr)
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
				return fmt.Errorf("failed to get states before reorg: %w", err)
			}

			// Now set the pre-reorg state at the reorg block
			updatePipe := rs.client.Pipeline()
			for i, result := range stateResults {
				if result.Err() != nil {
					if result.Err() != redis.Nil {
						rs.logger.Warn("Error getting state before reorg", "key", keysToProcess[i], "error", result.Err())
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
				return fmt.Errorf("failed to update states at reorg point: %w", err)
			}

			processed += len(keys)
			rs.logger.Debug("Processed keys in reorg", "prefix", prefix, "batch", len(keys), "total", processed)

			// Move to next batch
			cursor = nextCursor
			if cursor == 0 {
				break
			}
		}

		rs.logger.Info("Completed processing keys for reorg", "prefix", prefix, "processed", processed)
		return nil
	}

	// Process account and storage keys
	if err := processKeysWithPrefix(accountPrefix); err != nil {
		return err
	}

	if err := processKeysWithPrefix(storagePrefix); err != nil {
		return err
	}

	return nil
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
	ctx, cancel := context.WithTimeout(rs.ctx, redisOperationTimeout)
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
func (rs *RedisState) GetBlockByNumber(blockNum uint64) (*types.Header, error) {
	if !rs.enabled {
		return nil, fmt.Errorf("redis state not enabled")
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(rs.ctx, 5*time.Second)
	defer cancel()

	// Get block data
	blockKey := fmt.Sprintf("%s%d", blockPrefix, blockNum)
	blockData, err := rs.client.Get(ctx, blockKey).Result()

	if err != nil {
		if err == redis.Nil {
			return nil, fmt.Errorf("block %d not found", blockNum)
		}
		return nil, fmt.Errorf("block lookup failed: %v", err)
	}

	// Decode the block header
	var header types.Header
	if err := rlp.DecodeBytes([]byte(blockData), &header); err != nil {
		return nil, fmt.Errorf("failed to decode block header: %v", err)
	}

	return &header, nil
}

// GetTransactionsByBlockNumber gets all transactions in a block
func (rs *RedisState) GetTransactionsByBlockNumber(blockNum uint64) ([]types.Transaction, error) {
	if !rs.enabled {
		return nil, fmt.Errorf("redis state not enabled")
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(rs.ctx, 5*time.Second)
	defer cancel()

	// Get the block transactions collection
	blockTxsKey := fmt.Sprintf("%s%d", txsPrefix, blockNum)
	blockTxsData, err := rs.client.Get(ctx, blockTxsKey).Bytes()

	if err != nil {
		if err == redis.Nil {
			return []types.Transaction{}, nil // No transactions for this block
		}
		return nil, fmt.Errorf("failed to get block transactions: %v", err)
	}

	// Decode the array of encoded transactions
	var encodedTxs [][]byte
	if err := json.Unmarshal(blockTxsData, &encodedTxs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal block transactions array: %v", err)
	}

	// Decode each transaction
	txs := make([]types.Transaction, len(encodedTxs))
	for i, encodedTx := range encodedTxs {
		if encodedTx == nil {
			continue // Skip nil entries (shouldn't happen, but just in case)
		}

		var tx types.Transaction
		if err := rlp.DecodeBytes(encodedTx, &tx); err != nil {
			return nil, fmt.Errorf("failed to decode transaction at index %d: %v", i, err)
		}
		txs[i] = tx
	}

	return txs, nil
}

// GetTransactionHashesByBlockNumber gets all transaction hashes in a block
func (rs *RedisState) GetTransactionHashesByBlockNumber(blockNum uint64) ([]string, error) {
	if !rs.enabled {
		return nil, fmt.Errorf("redis state not enabled")
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(rs.ctx, 5*time.Second)
	defer cancel()

	// Get the block transactions collection
	blockTxsKey := fmt.Sprintf("%s%d", txsPrefix, blockNum)
	blockTxsData, err := rs.client.Get(ctx, blockTxsKey).Bytes()

	if err != nil {
		if err == redis.Nil {
			return []string{}, nil // No transactions for this block
		}
		return nil, fmt.Errorf("failed to get block transactions: %v", err)
	}

	// Decode the array of encoded transactions
	var encodedTxs [][]byte
	if err := json.Unmarshal(blockTxsData, &encodedTxs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal block transactions array: %v", err)
	}

	// Get the hash for each transaction
	txHashes := make([]string, 0, len(encodedTxs))
	for _, encodedTx := range encodedTxs {
		if encodedTx == nil {
			continue // Skip nil entries (shouldn't happen, but just in case)
		}

		var tx types.Transaction
		if err := rlp.DecodeBytes(encodedTx, &tx); err != nil {
			return nil, fmt.Errorf("failed to decode transaction: %v", err)
		}
		txHashes = append(txHashes, tx.Hash().Hex())
	}

	return txHashes, nil
}

// GetReceiptsByBlockNumber gets all receipts in a block
func (rs *RedisState) GetReceiptsByBlockNumber(blockNum uint64) ([]*types.Receipt, error) {
	if !rs.enabled {
		return nil, fmt.Errorf("redis state not enabled")
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(rs.ctx, 5*time.Second)
	defer cancel()

	// Get the block receipts collection
	blockReceiptsKey := fmt.Sprintf("%s%d", receiptsPrefix, blockNum)
	blockReceiptsData, err := rs.client.Get(ctx, blockReceiptsKey).Bytes()

	if err != nil {
		if err == redis.Nil {
			return []*types.Receipt{}, nil // No receipts for this block
		}
		return nil, fmt.Errorf("failed to get block receipts: %v", err)
	}

	// Decode the array of encoded receipts
	var encodedReceipts [][]byte
	if err := json.Unmarshal(blockReceiptsData, &encodedReceipts); err != nil {
		return nil, fmt.Errorf("failed to unmarshal block receipts array: %v", err)
	}

	// Decode each receipt
	receipts := make([]*types.Receipt, len(encodedReceipts))
	for i, encodedReceipt := range encodedReceipts {
		if encodedReceipt == nil {
			continue // Skip nil entries (shouldn't happen, but just in case)
		}

		var receipt types.Receipt
		if err := rlp.DecodeBytes(encodedReceipt, &receipt); err != nil {
			return nil, fmt.Errorf("failed to decode receipt at index %d: %v", i, err)
		}
		receipts[i] = &receipt
	}

	return receipts, nil
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
