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
	"math"
	"math/big"
	"reflect"
	"time"

	"github.com/holiman/uint256"
	"github.com/redis/go-redis/v9"

	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon-lib/types/accounts"
	"github.com/erigontech/erigon/core/types"
)

// RedisStateReader implements the StateReader interface using Redis as the backing store
type RedisStateReader struct {
	client   *redis.Client
	ctx      context.Context
	logger   log.Logger
	blockNum uint64 // For point-in-time queries, 0 means latest
}

// RedisStateWriter implements the StateWriter interface using Redis as the backing store
type RedisStateWriter struct {
	client   *redis.Client
	ctx      context.Context
	logger   log.Logger
	blockNum uint64
	txNum    uint64
}

// RedisHistoricalWriter extends RedisStateWriter with WriteChangeSets and WriteHistory methods
type RedisHistoricalWriter struct {
	RedisStateWriter
}

// Each interface method is implemented directly in RedisHistoricalWriter struct
// This avoids an import cycle with state package

// SerializedAccount is a serializable version of accounts.Account
type SerializedAccount struct {
	Nonce       uint64         `json:"nonce"`
	Balance     string         `json:"balance"` // Using string for uint256
	CodeHash    libcommon.Hash `json:"codeHash"`
	Incarnation uint64         `json:"incarnation"`
}

// NewRedisStateReader creates a new instance of RedisStateReader
func NewRedisStateReader(client *redis.Client) *RedisStateReader {
	ctx, _ := context.WithCancel(context.Background())
	return &RedisStateReader{
		client:   client,
		ctx:      ctx,
		logger:   log.Root(),
		blockNum: 0, // 0 means latest
	}
}

// NewRedisStateReaderWithLogger creates a new instance of RedisStateReader with a custom logger
func NewRedisStateReaderWithLogger(client *redis.Client, logger log.Logger) *RedisStateReader {
	ctx, _ := context.WithCancel(context.Background())
	return &RedisStateReader{
		client:   client,
		ctx:      ctx,
		logger:   logger,
		blockNum: 0,
	}
}

// NewRedisStateReaderAtBlock creates a new instance of RedisStateReader at a specific block height
func NewRedisStateReaderAtBlock(client *redis.Client, blockNum uint64) *RedisStateReader {
	ctx, _ := context.WithCancel(context.Background())
	return &RedisStateReader{
		client:   client,
		ctx:      ctx,
		logger:   log.Root(),
		blockNum: blockNum,
	}
}

// NewRedisStateWriter creates a new instance of RedisStateWriter
func NewRedisStateWriter(client *redis.Client, blockNum uint64) *RedisStateWriter {
	ctx, _ := context.WithCancel(context.Background())
	return &RedisStateWriter{
		client:   client,
		ctx:      ctx,
		logger:   log.Root(),
		blockNum: blockNum,
	}
}

// NewRedisStateWriterWithLogger creates a new instance of RedisStateWriter with a custom logger
func NewRedisStateWriterWithLogger(client *redis.Client, blockNum uint64, logger log.Logger) *RedisStateWriter {
	ctx, _ := context.WithCancel(context.Background())
	return &RedisStateWriter{
		client:   client,
		ctx:      ctx,
		logger:   logger,
		blockNum: blockNum,
	}
}

// NewRedisHistoricalWriter creates a new instance of RedisHistoricalWriter
func NewRedisHistoricalWriter(client *redis.Client, blockNum uint64) *RedisHistoricalWriter {
	return &RedisHistoricalWriter{
		RedisStateWriter: *NewRedisStateWriter(client, blockNum),
	}
}

// GetBlockNum returns the block number for this writer
func (w *RedisHistoricalWriter) GetBlockNum() uint64 {
	return w.blockNum
}

// WriteBlockStart writes the block start marker to Redis, initializing the block context
func (w *RedisHistoricalWriter) WriteBlockStart(blockNum uint64) error {
	// Add direct console logging for debugging
	fmt.Printf("REDIS_WRITE: Starting new block %d\n", blockNum)

	// Update current block number
	w.blockNum = blockNum
	w.txNum = 0 // Reset transaction counter at block start

	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()

	// Update the current block pointer
	err := w.client.Set(ctx, "currentBlock", blockNum, 0).Err()
	if err != nil {
		return fmt.Errorf("failed to update current block: %w", err)
	}

	// Create a block entry if it doesn't exist
	blockKey := fmt.Sprintf("block:%d", blockNum)
	exists, err := w.client.Exists(ctx, blockKey).Result()
	if err != nil {
		return fmt.Errorf("failed to check block existence: %w", err)
	}

	if exists == 0 {
		// Initialize block data structure
		blockData := map[string]interface{}{
			"number":    blockNum,
			"timestamp": time.Now().Unix(),
		}

		err = w.client.HSet(ctx, blockKey, blockData).Err()
		if err != nil {
			return fmt.Errorf("failed to initialize block data: %w", err)
		}
	}

	return nil
}

// SetTxNum sets the current transaction number being processed
func (w *RedisStateWriter) SetTxNum(txNum uint64) {
	w.txNum = txNum
}

// GetTxNum gets the current transaction number being processed
func (w *RedisStateWriter) GetTxNum() uint64 {
	return w.txNum
}

// DirectTestWrite is a simple test method that writes directly to Redis
func (w *RedisStateWriter) DirectTestWrite(key string, value string) error {
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()

	return w.client.Set(ctx, key, value, 24*time.Hour).Err()
}

// WriteChangeSets writes change sets to Redis
func (w *RedisHistoricalWriter) WriteChangeSets() error {
	// Add direct console logging for debugging
	fmt.Printf("REDIS_WRITE: Writing change sets for block %d\n", w.blockNum)

	// For Redis implementation, most changes are written immediately
	// However, we need to update canonical chain information to handle reorgs properly

	// If this is a normal block (not a reorg marker)
	if w.txNum != 0 {
		ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
		defer cancel()

		// Update the canonical chain information
		canonicalKey := "canonicalChain"
		blockHashKey := fmt.Sprintf("block:%d", w.blockNum)

		// Get the block hash
		blockHash, err := w.client.HGet(ctx, blockHashKey, "hash").Result()
		if err != nil && err != redis.Nil {
			return fmt.Errorf("failed to get block hash for block %d: %w", w.blockNum, err)
		}

		if blockHash != "" {
			// Add this block/hash pair to the canonical chain
			err = w.client.ZAdd(ctx, canonicalKey, redis.Z{
				Score:  float64(w.blockNum),
				Member: blockHash,
			}).Err()

			if err != nil {
				return fmt.Errorf("failed to update canonical chain: %w", err)
			}

			// Update the hash entry to mark it as canonical
			hashKey := fmt.Sprintf("blockHash:%s", blockHash)
			err = w.client.HSet(ctx, hashKey, "canonical", true).Err()
			if err != nil {
				return fmt.Errorf("failed to mark block as canonical: %w", err)
			}

			// Update the current block pointer
			err = w.client.Set(ctx, "currentBlock", w.blockNum, 0).Err()
			if err != nil {
				return fmt.Errorf("failed to update current block: %w", err)
			}
		}
	}

	return nil
}

// WriteHistory writes history to Redis
func (w *RedisHistoricalWriter) WriteHistory() error {
	// Add direct console logging for debugging
	fmt.Printf("REDIS_WRITE: Writing history for block %d\n", w.blockNum)

	// For Redis implementation, we want to handle specifically marked blocks
	// txNum == 0 is a special marker for blocks that have been reorged out
	// or need special handling

	if w.txNum == 0 {
		// This is a block that has been reorged out
		// We could add extra handling here, e.g. marking keys as invalid
		// or implementing a pruning policy
		ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
		defer cancel()

		// We could add a record to a special key to track reorgs
		reorgKey := fmt.Sprintf("reorg:%d", w.blockNum)
		err := w.client.Set(ctx, reorgKey, time.Now().Format(time.RFC3339), 30*24*time.Hour).Err()
		if err != nil {
			return fmt.Errorf("error recording reorg marker: %w", err)
		}
	}

	return nil
}

// StoreBlockInfo stores block header information in Redis
func (w *RedisHistoricalWriter) StoreBlockInfo(headerInterface interface{}, root libcommon.Hash) error {
	// Add direct console logging for debugging
	fmt.Printf("REDIS_WRITE: Storing block info for block %d, root %s\n", w.blockNum, root.Hex())

	// Extract relevant header information
	headerJSON, err := json.Marshal(headerInterface)
	if err != nil {
		return fmt.Errorf("failed to marshal block header: %w", err)
	}

	// Attempt to extract common block header fields
	var blockTime uint64
	var difficulty *big.Int
	var gasLimit uint64
	var gasUsed uint64
	var parentHash libcommon.Hash
	var coinbase libcommon.Address
	var extraData []byte
	var baseFee *big.Int

	// Extract header hash if available
	var headerHash string
	if header, ok := headerInterface.(interface{ Hash() libcommon.Hash }); ok {
		headerHash = header.Hash().Hex()
	}

	// Try to extract common header fields using reflection to avoid type dependencies
	headerVal := reflect.ValueOf(headerInterface)
	if headerVal.Kind() == reflect.Ptr {
		headerVal = headerVal.Elem()
	}

	// Only proceed if we have a struct
	if headerVal.Kind() == reflect.Struct {
		// Extract Time field
		if timeField := headerVal.FieldByName("Time"); timeField.IsValid() {
			blockTime = timeField.Uint()
		}

		// Extract Difficulty field
		if diffField := headerVal.FieldByName("Difficulty"); diffField.IsValid() {
			if diff, ok := diffField.Interface().(*big.Int); ok && diff != nil {
				difficulty = diff
			}
		}

		// Extract GasLimit field
		if gasLimitField := headerVal.FieldByName("GasLimit"); gasLimitField.IsValid() {
			gasLimit = gasLimitField.Uint()
		}

		// Extract GasUsed field
		if gasUsedField := headerVal.FieldByName("GasUsed"); gasUsedField.IsValid() {
			gasUsed = gasUsedField.Uint()
		}

		// Extract ParentHash field
		if parentHashField := headerVal.FieldByName("ParentHash"); parentHashField.IsValid() {
			if hash, ok := parentHashField.Interface().(libcommon.Hash); ok {
				parentHash = hash
			}
		}

		// Extract Coinbase field
		if coinbaseField := headerVal.FieldByName("Coinbase"); coinbaseField.IsValid() {
			if addr, ok := coinbaseField.Interface().(libcommon.Address); ok {
				coinbase = addr
			}
		}

		// Extract ExtraData field
		if extraDataField := headerVal.FieldByName("Extra"); extraDataField.IsValid() {
			if data, ok := extraDataField.Interface().([]byte); ok {
				extraData = data
			}
		}

		// Extract BaseFee field (EIP-1559)
		if baseFeeField := headerVal.FieldByName("BaseFee"); baseFeeField.IsValid() && !baseFeeField.IsNil() {
			if fee, ok := baseFeeField.Interface().(*big.Int); ok && fee != nil {
				baseFee = fee
			}
		}
	}

	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()

	// Store block information
	blockKey := fmt.Sprintf("block:%d", w.blockNum)

	// Create a block data structure with current information
	blockData := map[string]interface{}{
		"number":      w.blockNum,
		"timestamp":   blockTime,
		"header":      string(headerJSON),
		"stateRoot":   root.Hex(),
		"headerHash":  headerHash,
		"parentHash":  parentHash.Hex(),
		"gasLimit":    gasLimit,
		"gasUsed":     gasUsed,
		"indexedTime": time.Now().Unix(), // When we indexed this block
	}

	if difficulty != nil {
		blockData["difficulty"] = difficulty.String()
	}

	if baseFee != nil {
		blockData["baseFee"] = baseFee.String()
	}

	if coinbase != (libcommon.Address{}) {
		blockData["coinbase"] = coinbase.Hex()
	}

	if len(extraData) > 0 {
		blockData["extraData"] = libcommon.Bytes2Hex(extraData)
	}

	// Store detailed block info
	blockInfoKey := fmt.Sprintf("blockinfo:%d", w.blockNum)
	if err := w.client.HSet(ctx, blockInfoKey, blockData).Err(); err != nil {
		w.logger.Warn("Failed to store detailed block info", "block", w.blockNum, "err", err)
		// Non-fatal error, continue processing
	}

	if headerHash != "" {
		// Store hash to block number mapping
		blockHashKey := fmt.Sprintf("blockHash:%s", headerHash)
		err = w.client.HSet(ctx, blockHashKey, map[string]interface{}{
			"number":    w.blockNum,
			"timestamp": blockTime,
			"canonical": true,
		}).Err()

		if err != nil {
			return fmt.Errorf("failed to store block hash mapping: %w", err)
		}

		// Also set hash field in the block entry
		blockData["hash"] = headerHash

		// Add to latest blocks index (with score as block number for ordering)
		latestBlocksKey := "blocks:latest"
		err = w.client.ZAdd(ctx, latestBlocksKey, redis.Z{
			Score:  float64(w.blockNum),
			Member: headerHash,
		}).Err()
		if err != nil {
			w.logger.Warn("Failed to add block to latest blocks index", "block", w.blockNum, "err", err)
		}

		// Index block by miner/coinbase
		if coinbase != (libcommon.Address{}) {
			minerBlocksKey := fmt.Sprintf("miner:%s:blocks", coinbase.Hex())
			err = w.client.ZAdd(ctx, minerBlocksKey, redis.Z{
				Score:  float64(w.blockNum),
				Member: headerHash,
			}).Err()
			if err != nil {
				w.logger.Warn("Failed to index block by miner", "miner", coinbase.Hex(), "err", err)
			}
		}
	}

	// Store actual block data
	err = w.client.HSet(ctx, blockKey, blockData).Err()
	if err != nil {
		return fmt.Errorf("failed to store block data: %w", err)
	}

	return nil
}

// accountKey creates the Redis key for an account
func accountKey(address libcommon.Address) string {
	return fmt.Sprintf("account:%s", address.Hex())
}

// storageKey creates the Redis key for a storage slot
func storageKey(address libcommon.Address, key *libcommon.Hash) string {
	return fmt.Sprintf("storage:%s:%s", address.Hex(), key.Hex())
}

// codeKey creates the Redis key for contract code
func codeKey(codeHash libcommon.Hash) string {
	return fmt.Sprintf("code:%s", codeHash.Hex())
}

// ReadAccountDataAtBlock reads account data from Redis at a specific block height
func (r *RedisStateReader) ReadAccountDataAtBlock(address libcommon.Address, blockNum uint64) (*accounts.Account, error) {
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()

	key := accountKey(address)

	// Get the most recent account data before or at the specified block
	result := r.client.ZRevRangeByScore(ctx, key, &redis.ZRangeBy{
		Min:    "0",
		Max:    fmt.Sprintf("%d", blockNum),
		Offset: 0,
		Count:  1,
	})

	if result.Err() != nil {
		if result.Err() == redis.Nil {
			return nil, nil // Account doesn't exist
		}
		return nil, fmt.Errorf("redis error reading account %s at block %d: %w", address.Hex(), blockNum, result.Err())
	}

	values, err := result.Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get account result: %w", err)
	}

	if len(values) == 0 {
		return nil, nil // Account doesn't exist
	}

	var serialized SerializedAccount
	if err := json.Unmarshal([]byte(values[0]), &serialized); err != nil {
		return nil, fmt.Errorf("invalid account data for %s: %w", address.Hex(), err)
	}

	balance, err := uint256.FromHex(serialized.Balance)
	if err != nil {
		return nil, fmt.Errorf("invalid balance value for %s: %w", address.Hex(), err)
	}

	return &accounts.Account{
		Nonce:       serialized.Nonce,
		Balance:     *balance,
		CodeHash:    serialized.CodeHash,
		Incarnation: serialized.Incarnation,
	}, nil
}

// ReadAccountData reads account data from Redis at latest block
func (r *RedisStateReader) ReadAccountData(address libcommon.Address) (*accounts.Account, error) {
	// For compatibility, we'll use a very large block number to get the latest state
	return r.ReadAccountDataAtBlock(address, math.MaxUint64)
}

// ReadAccountDataForDebug reads account data from Redis for debugging
func (r *RedisStateReader) ReadAccountDataForDebug(address libcommon.Address) (*accounts.Account, error) {
	return r.ReadAccountData(address)
}

// ReadAccountStorageAtBlock reads account storage from Redis at a specific block height
func (r *RedisStateReader) ReadAccountStorageAtBlock(address libcommon.Address, incarnation uint64, key *libcommon.Hash, blockNum uint64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()

	storageKeyStr := storageKey(address, key)

	// Get the most recent storage data before or at the specified block
	result := r.client.ZRevRangeByScore(ctx, storageKeyStr, &redis.ZRangeBy{
		Min:    "0",
		Max:    fmt.Sprintf("%d", blockNum),
		Offset: 0,
		Count:  1,
	})

	if result.Err() != nil {
		if result.Err() == redis.Nil {
			return nil, nil // Storage doesn't exist
		}
		return nil, fmt.Errorf("redis error reading storage %s at block %d: %w",
			storageKeyStr, blockNum, result.Err())
	}

	values, err := result.Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get storage result: %w", err)
	}

	if len(values) == 0 {
		return nil, nil // Storage doesn't exist
	}

	// Empty string means zero value - return empty bytes
	if len(values[0]) == 0 {
		return []byte{}, nil
	}

	return []byte(values[0]), nil
}

// ReadAccountStorage reads account storage from Redis at latest block
func (r *RedisStateReader) ReadAccountStorage(address libcommon.Address, incarnation uint64, key *libcommon.Hash) ([]byte, error) {
	blockNum := r.blockNum
	if blockNum == 0 {
		blockNum = math.MaxUint64 // Latest state
	}
	return r.ReadAccountStorageAtBlock(address, incarnation, key, blockNum)
}

// ReadAccountCode reads account code from Redis
func (r *RedisStateReader) ReadAccountCode(address libcommon.Address, incarnation uint64) ([]byte, error) {
	// First get the account to find the code hash
	account, err := r.ReadAccountData(address)
	if err != nil {
		return nil, fmt.Errorf("failed to read account data for code lookup: %w", err)
	}
	if account == nil {
		return nil, nil
	}

	if account.Incarnation != incarnation {
		return nil, nil
	}

	// Check if it's empty code
	if account.CodeHash == (libcommon.Hash{}) {
		return nil, nil
	}

	// Get the code using the code hash
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()

	key := codeKey(account.CodeHash)
	result := r.client.Get(ctx, key)
	if result.Err() == redis.Nil {
		return nil, nil
	}
	if result.Err() != nil {
		return nil, fmt.Errorf("failed to get code from redis for hash %s: %w",
			account.CodeHash.Hex(), result.Err())
	}

	code := result.Val()
	if len(code) == 0 {
		return nil, nil // Empty code
	}

	return []byte(code), nil
}

// ReadAccountCodeSize reads account code size from Redis
func (r *RedisStateReader) ReadAccountCodeSize(address libcommon.Address, incarnation uint64) (int, error) {
	code, err := r.ReadAccountCode(address, incarnation)
	if err != nil {
		return 0, err
	}
	return len(code), nil
}

// ReadAccountIncarnation reads account incarnation from Redis
func (r *RedisStateReader) ReadAccountIncarnation(address libcommon.Address) (uint64, error) {
	account, err := r.ReadAccountData(address)
	if err != nil {
		return 0, err
	}
	if account == nil {
		return 0, nil
	}
	return account.Incarnation, nil
}

// UpdateAccountData updates account data in Redis
func (w *RedisStateWriter) UpdateAccountData(address libcommon.Address, original, account *accounts.Account) error {
	if account == nil {
		return errors.New("account cannot be nil")
	}

	// Add direct console logging for debugging
	fmt.Printf("REDIS_WRITE: Updating account data for %s at block %d, nonce: %d, balance: %s\n",
		address.Hex(), w.blockNum, account.Nonce, account.Balance.Hex())

	// Add detailed debug logging
	w.logger.Info("Redis: Updating account data",
		"block", w.blockNum,
		"address", address.Hex(),
		"nonce", account.Nonce,
		"balance", account.Balance.Hex(),
		"codeHash", account.CodeHash.Hex())

	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()

	key := accountKey(address)
	serialized := SerializedAccount{
		Nonce:       account.Nonce,
		Balance:     account.Balance.Hex(),
		CodeHash:    account.CodeHash,
		Incarnation: account.Incarnation,
	}

	data, err := json.Marshal(serialized)
	if err != nil {
		return fmt.Errorf("failed to marshal account data: %w", err)
	}

	// Store the account with the current block number as score
	cmd := w.client.ZAdd(ctx, key, redis.Z{
		Score:  float64(w.blockNum),
		Member: string(data),
	})

	if cmd.Err() != nil {
		w.logger.Error("Redis: Failed to update account data",
			"address", address.Hex(),
			"block", w.blockNum,
			"err", cmd.Err())
		return fmt.Errorf("redis error updating account data for %s: %w", address.Hex(), cmd.Err())
	}

	w.logger.Debug("Redis: Successfully updated account data",
		"address", address.Hex(),
		"block", w.blockNum)
	return nil
}

// UpdateAccountCode updates account code in Redis
func (w *RedisStateWriter) UpdateAccountCode(address libcommon.Address, incarnation uint64, codeHash libcommon.Hash, code []byte) error {
	// Skip if code is empty
	if len(code) == 0 {
		return nil
	}

	// Add direct console logging for debugging
	fmt.Printf("REDIS_WRITE: Updating code for address %s, codeHash %s, code length %d at block %d\n",
		address.Hex(), codeHash.Hex(), len(code), w.blockNum)

	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()

	// Store code by hash (immutable)
	key := codeKey(codeHash)

	// Check if code already exists - don't need to rewrite if it does
	existsCmd := w.client.Exists(ctx, key)
	if existsCmd.Err() != nil {
		return fmt.Errorf("redis error checking code existence for hash %s: %w",
			codeHash.Hex(), existsCmd.Err())
	}

	// If code already exists, skip writing it again
	if existsCmd.Val() > 0 {
		return nil
	}

	// Store code with no expiration (immutable data)
	setCmd := w.client.Set(ctx, key, code, 0)
	if setCmd.Err() != nil {
		return fmt.Errorf("redis error storing code for hash %s: %w",
			codeHash.Hex(), setCmd.Err())
	}

	return nil
}

// HandleTransaction stores transaction data in Redis
func (w *RedisStateWriter) HandleTransaction(tx types.Transaction, receipt *types.Receipt, blockNum uint64, txIndex uint64) error {
	// Add direct console logging for debugging
	fmt.Printf("REDIS_WRITE: Handling transaction %s at block %d, txIndex %d\n",
		tx.Hash().Hex(), blockNum, txIndex)

	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()

	// Serialize the transaction to JSON
	txHash := tx.Hash()
	txData, err := json.Marshal(tx)
	if err != nil {
		return fmt.Errorf("failed to marshal transaction %s: %w", txHash.Hex(), err)
	}

	// Store the transaction with block number as score
	txKey := fmt.Sprintf("tx:%s", txHash.Hex())
	err = w.client.ZAdd(ctx, txKey, redis.Z{
		Score:  float64(blockNum),
		Member: string(txData),
	}).Err()
	if err != nil {
		return fmt.Errorf("redis error storing transaction %s: %w", txHash.Hex(), err)
	}

	// Add transaction to block's transaction list with ordering by index
	blockTxsKey := fmt.Sprintf("block:%d:txs", blockNum)
	err = w.client.ZAdd(ctx, blockTxsKey, redis.Z{
		Score:  float64(txIndex), // Use txIndex as score for proper ordering
		Member: txHash.Hex(),
	}).Err()
	if err != nil {
		return fmt.Errorf("redis error adding transaction to block: %w", err)
	}

	// Set transaction sender
	sender, ok := tx.GetSender()
	if ok {
		// Add transaction to sender's transaction list
		senderTxsKey := fmt.Sprintf("sender:%s:txs", sender.Hex())
		err = w.client.ZAdd(ctx, senderTxsKey, redis.Z{
			Score:  float64(blockNum),
			Member: txHash.Hex(),
		}).Err()
		if err != nil {
			return fmt.Errorf("redis error adding transaction to sender: %w", err)
		}
	}

	// Also store recipient for contract creation or normal transactions
	var to *libcommon.Address
	// Try to get recipient safely, using reflection to avoid interface incompatibilities
	txVal := reflect.ValueOf(tx)
	if txVal.Kind() == reflect.Ptr {
		txVal = txVal.Elem()
	}
	
	// Check if the tx has a To method
	toMethod := reflect.ValueOf(tx).MethodByName("To")
	if toMethod.IsValid() {
		// Call the To method
		result := toMethod.Call(nil)
		if len(result) > 0 && !result[0].IsNil() {
			if toAddr, ok := result[0].Interface().(*libcommon.Address); ok && toAddr != nil {
				to = toAddr
				// Add transaction to recipient's received txs list
				recipientTxsKey := fmt.Sprintf("recipient:%s:txs", to.Hex())
				err = w.client.ZAdd(ctx, recipientTxsKey, redis.Z{
					Score:  float64(blockNum),
					Member: txHash.Hex(),
				}).Err()
				if err != nil {
					w.logger.Warn("Failed to add transaction to recipient list", "recipient", to.Hex(), "err", err)
					// Non-fatal error, continue processing
				}
			}
		}
	}
	
	// Store transaction data directly in a hash for easier retrieval
	txInfoKey := fmt.Sprintf("txinfo:%s", txHash.Hex())
	txInfo := map[string]interface{}{
		"hash":      txHash.Hex(),
		"blockNum":  blockNum,
		"txIndex":   txIndex,
		"timestamp": time.Now().Unix(), // For approximate timing
	}
	
	// Get gas using reflection if available
	gasMethod := reflect.ValueOf(tx).MethodByName("Gas")
	if gasMethod.IsValid() {
		result := gasMethod.Call(nil)
		if len(result) > 0 {
			txInfo["gas"] = result[0].Uint()
		}
	}
	
	// Get gas price using reflection if available
	gasPriceMethod := reflect.ValueOf(tx).MethodByName("GasPrice")
	if gasPriceMethod.IsValid() {
		result := gasPriceMethod.Call(nil)
		if len(result) > 0 && !result[0].IsNil() {
			if bigInt, ok := result[0].Interface().(*big.Int); ok && bigInt != nil {
				txInfo["gasPrice"] = bigInt.String()
			}
		}
	}
	
	// Get value using reflection if available
	valueMethod := reflect.ValueOf(tx).MethodByName("Value")
	if valueMethod.IsValid() {
		result := valueMethod.Call(nil)
		if len(result) > 0 && !result[0].IsNil() {
			if bigInt, ok := result[0].Interface().(*big.Int); ok && bigInt != nil {
				txInfo["value"] = bigInt.String()
			}
		}
	}
	
	if sender, ok := tx.GetSender(); ok {
		txInfo["from"] = sender.Hex()
	}
	if to != nil {
		txInfo["to"] = to.Hex()
	}
	
	if err := w.client.HSet(ctx, txInfoKey, txInfo).Err(); err != nil {
		w.logger.Warn("Failed to store transaction info", "tx", txHash.Hex(), "err", err)
		// Non-fatal error, continue processing
	}

	// If we have a receipt, store it as well
	if receipt != nil {
		err = w.StoreReceipt(txHash, receipt, blockNum, txIndex)
		if err != nil {
			return fmt.Errorf("failed to store receipt for transaction %s: %w", txHash.Hex(), err)
		}
	}

	return nil
}

// StoreReceipt stores a transaction receipt in Redis
func (w *RedisStateWriter) StoreReceipt(txHash libcommon.Hash, receipt *types.Receipt, blockNum uint64, txIndex uint64) error {
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()

	// Add direct console logging for debugging
	fmt.Printf("REDIS_WRITE: Storing receipt for tx %s at block %d, txIndex %d, status: %d, gasUsed: %d, logs: %d\n",
		txHash.Hex(), blockNum, txIndex, receipt.Status, receipt.GasUsed, len(receipt.Logs))

	// Serialize the receipt to JSON
	receiptData, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("failed to marshal receipt for transaction %s: %w", txHash.Hex(), err)
	}

	// Store the receipt with block number as score
	receiptKey := fmt.Sprintf("receipt:%s", txHash.Hex())
	err = w.client.ZAdd(ctx, receiptKey, redis.Z{
		Score:  float64(blockNum),
		Member: string(receiptData),
	}).Err()
	if err != nil {
		return fmt.Errorf("redis error storing receipt for transaction %s: %w", txHash.Hex(), err)
	}

	// Add receipt to block's receipts list
	blockReceiptsKey := fmt.Sprintf("block:%d:receipts", blockNum)
	err = w.client.ZAdd(ctx, blockReceiptsKey, redis.Z{
		Score:  float64(txIndex),
		Member: txHash.Hex(),
	}).Err()
	if err != nil {
		return fmt.Errorf("redis error adding receipt to block: %w", err)
	}
	
	// Store receipt data directly in a hash for easier retrieval
	receiptInfoKey := fmt.Sprintf("receiptinfo:%s", txHash.Hex())
	receiptInfo := map[string]interface{}{
		"txHash":    txHash.Hex(),
		"blockNum":  blockNum,
		"txIndex":   txIndex,
		"status":    receipt.Status,
		"gasUsed":   receipt.GasUsed,
		"logsCount": len(receipt.Logs),
		"timestamp": time.Now().Unix(), // For approximate timing
	}
	
	if receipt.ContractAddress != (libcommon.Address{}) {
		receiptInfo["contractAddress"] = receipt.ContractAddress.Hex()
		
		// Add special index for contract creation events
		contractCreationKey := fmt.Sprintf("contracts:created")
		err = w.client.ZAdd(ctx, contractCreationKey, redis.Z{
			Score:  float64(blockNum),
			Member: receipt.ContractAddress.Hex(),
		}).Err()
		if err != nil {
			w.logger.Warn("Failed to index contract creation", "contract", receipt.ContractAddress.Hex(), "err", err)
			// Non-fatal error, continue processing
		}
	}
	
	if err := w.client.HSet(ctx, receiptInfoKey, receiptInfo).Err(); err != nil {
		w.logger.Warn("Failed to store receipt info", "tx", txHash.Hex(), "err", err)
		// Non-fatal error, continue processing
	}

	// For each log, store it in the logs by topic index
	for _, log := range receipt.Logs {
		// Store log by its index with transaction context
		logKey := fmt.Sprintf("log:%d", log.Index)
		
		// Enhance log with transaction information
		// This makes logs self-contained with all reference information
		log.TxHash = txHash
		log.BlockNumber = blockNum
		log.TxIndex = uint(txIndex)
		
		logData, err := json.Marshal(log)
		if err != nil {
			w.logger.Warn("Failed to marshal log", "log", log.Index, "err", err)
			continue
		}

		err = w.client.Set(ctx, logKey, logData, 0).Err()
		if err != nil {
			w.logger.Warn("Failed to store log", "log", log.Index, "err", err)
			continue
		}
		
		// Create a log hash key for the full log info to avoid duplication
		logDataKey := fmt.Sprintf("loginfo:%d", log.Index)
		logInfo := map[string]interface{}{
			"txHash":      txHash.Hex(),
			"blockNum":    blockNum,
			"txIndex":     txIndex,
			"logIndex":    log.Index,
			"address":     log.Address.Hex(),
			"data":        libcommon.Bytes2Hex(log.Data),
			"timestamp":   time.Now().Unix(),
		}
		
		// Add topics individually
		for i, topic := range log.Topics {
			topicKey := fmt.Sprintf("topic%d", i)
			logInfo[topicKey] = topic.Hex()
		}
		
		if err := w.client.HSet(ctx, logDataKey, logInfo).Err(); err != nil {
			w.logger.Warn("Failed to store log info", "log", log.Index, "err", err)
			// Non-fatal error, continue processing
		}

		// Index logs by each topic (allows searching by ANY topic)
		for i, topic := range log.Topics {
			// Index by specific topic position (topic0, topic1, ...)
			topicPosKey := fmt.Sprintf("topic%d:%s", i, topic.Hex())
			err = w.client.ZAdd(ctx, topicPosKey, redis.Z{
				Score:  float64(blockNum),
				Member: fmt.Sprintf("%d", log.Index),
			}).Err()
			if err != nil {
				w.logger.Warn("Failed to index log by topic position", 
					"position", i, "topic", topic.Hex(), "err", err)
			}
			
			// Index by any topic position (for general topic searches)
			topicKey := fmt.Sprintf("topic:%s", topic.Hex())
			err = w.client.ZAdd(ctx, topicKey, redis.Z{
				Score:  float64(blockNum),
				Member: fmt.Sprintf("%d", log.Index),
			}).Err()
			if err != nil {
				w.logger.Warn("Failed to index log by topic", "topic", topic.Hex(), "err", err)
			}
		}

		// Index logs by address
		addressKey := fmt.Sprintf("address:%s:logs", log.Address.Hex())
		err = w.client.ZAdd(ctx, addressKey, redis.Z{
			Score:  float64(blockNum),
			Member: fmt.Sprintf("%d", log.Index),
		}).Err()
		if err != nil {
			w.logger.Warn("Failed to index log by address", "address", log.Address.Hex(), "err", err)
		}
		
		// Index transactions that emitted logs
		txWithLogsKey := fmt.Sprintf("txs:with:logs")
		err = w.client.ZAdd(ctx, txWithLogsKey, redis.Z{
			Score:  float64(blockNum),
			Member: txHash.Hex(),
		}).Err()
		if err != nil {
			w.logger.Warn("Failed to index tx with logs", "tx", txHash.Hex(), "err", err)
		}
	}

	return nil
}

// DeleteAccount deletes an account in Redis
func (w *RedisStateWriter) DeleteAccount(address libcommon.Address, original *accounts.Account) error {
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()

	// For deletion, we store an empty account with current block number
	key := accountKey(address)
	serialized := SerializedAccount{
		Nonce:       0,
		Balance:     "0x0",
		CodeHash:    libcommon.Hash{},
		Incarnation: 0,
	}

	data, err := json.Marshal(serialized)
	if err != nil {
		return fmt.Errorf("failed to marshal empty account data: %w", err)
	}

	// Store the deleted account with the current block number as score
	cmd := w.client.ZAdd(ctx, key, redis.Z{
		Score:  float64(w.blockNum),
		Member: string(data),
	})

	if cmd.Err() != nil {
		return fmt.Errorf("redis error deleting account %s: %w", address.Hex(), cmd.Err())
	}

	return nil
}

// WriteAccountStorage writes account storage to Redis
func (w *RedisStateWriter) WriteAccountStorage(address libcommon.Address, incarnation uint64, key *libcommon.Hash, original, value *uint256.Int) error {
	// Add direct console logging for debugging
	fmt.Printf("REDIS_WRITE: Writing storage for address %s, key %s at block %d\n",
		address.Hex(), key.Hex(), w.blockNum)

	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()

	storageKeyStr := storageKey(address, key)

	// Convert value to bytes
	var valueBytes []byte
	if value != nil && !value.IsZero() {
		valueBytes = value.Bytes()
	} else {
		valueBytes = []byte{} // Empty value for zero or nil
	}

	// Store storage with the current block number as score
	cmd := w.client.ZAdd(ctx, storageKeyStr, redis.Z{
		Score:  float64(w.blockNum),
		Member: string(valueBytes),
	})

	if cmd.Err() != nil {
		return fmt.Errorf("redis error writing storage for %s at key %s: %w",
			address.Hex(), key.Hex(), cmd.Err())
	}

	return nil
}

// CreateContract creates a new contract in Redis
func (w *RedisStateWriter) CreateContract(address libcommon.Address) error {
	// Creating a contract just means setting the incarnation
	// We get the current account first
	reader := NewRedisStateReader(w.client)
	account, err := reader.ReadAccountData(address)
	if err != nil {
		return fmt.Errorf("failed to read account data for contract creation: %w", err)
	}

	if account == nil {
		// New account
		account = &accounts.Account{
			Nonce:       0,
			Balance:     *uint256.NewInt(0),
			CodeHash:    libcommon.Hash{},
			Incarnation: 1, // FirstContractIncarnation
		}
	} else {
		// Existing account, set incarnation
		// If this is the first time it's becoming a contract, use FirstContractIncarnation
		// Otherwise increment the existing incarnation
		if account.Incarnation == 0 {
			account.Incarnation = 1 // FirstContractIncarnation
		} else {
			account.Incarnation++
		}
	}

	return w.UpdateAccountData(address, nil, account)
}
