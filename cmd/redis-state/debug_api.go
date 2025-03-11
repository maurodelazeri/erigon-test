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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/holiman/uint256"
	jsoniter "github.com/json-iterator/go"

	"github.com/erigontech/erigon-lib/chain"
	"github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/common/hexutil"
	"github.com/erigontech/erigon-lib/log/v3"

	"github.com/erigontech/erigon/consensus"
	"github.com/erigontech/erigon/consensus/ethash"
	"github.com/erigontech/erigon/consensus/ethash/ethashcfg"
	"github.com/erigontech/erigon/core"
	"github.com/erigontech/erigon/core/state"
	"github.com/erigontech/erigon/core/types"
	"github.com/erigontech/erigon/core/vm"
	"github.com/erigontech/erigon/core/vm/evmtypes"
	"github.com/erigontech/erigon/eth/tracers"
	tracersConfig "github.com/erigontech/erigon/eth/tracers/config"
	"github.com/erigontech/erigon/eth/tracers/logger"
	"github.com/erigontech/erigon/rpc"
)

type DebugAPI struct {
	stateProvider *RedisStateProvider
	logger        log.Logger
}

// TraceBlockByNumber implements debug_traceBlockByNumber. Returns Geth style block traces.
func (api *DebugAPI) TraceBlockByNumber(ctx context.Context, blockNumber rpc.BlockNumber, config *tracersConfig.TraceConfig, stream *jsoniter.Stream) error {
	api.logger.Info("Tracing block", "number", blockNumber)
	return api.traceBlock(ctx, rpc.BlockNumberOrHashWithNumber(blockNumber), config, stream)
}

// TraceBlockByHash implements debug_traceBlockByHash. Returns Geth style block traces.
func (api *DebugAPI) TraceBlockByHash(ctx context.Context, hash common.Hash, config *tracersConfig.TraceConfig, stream *jsoniter.Stream) error {
	api.logger.Info("Tracing block", "hash", hash.Hex())
	return api.traceBlock(ctx, rpc.BlockNumberOrHashWithHash(hash, true), config, stream)
}

// traceBlock implements the shared logic between TraceBlockByNumber and TraceBlockByHash
func (api *DebugAPI) traceBlock(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash, config *tracersConfig.TraceConfig, stream *jsoniter.Stream) error {
	// Convert block number or hash to a specific block number
	var blockNum uint64

	if blockNumber, ok := blockNrOrHash.Number(); ok {
		switch blockNumber {
		case rpc.LatestBlockNumber, rpc.PendingBlockNumber:
			latest, err := api.stateProvider.GetLatestBlockNumber(ctx)
			if err != nil {
				stream.WriteNil()
				return err
			}
			blockNum = latest
		case rpc.EarliestBlockNumber:
			blockNum = 0
		default:
			blockNum = uint64(blockNumber)
		}
	} else if hash, ok := blockNrOrHash.Hash(); ok {
		// Get block number from hash using Redis
		hashKey := fmt.Sprintf("blockHash:%s", hash.Hex())
		blockNumStr, err := api.stateProvider.GetRedisClient().Get(ctx, hashKey).Result()
		if err != nil {
			stream.WriteNil()
			return fmt.Errorf("block hash not found: %s", hash.Hex())
		}
		blockNum, err = strconv.ParseUint(blockNumStr, 10, 64)
		if err != nil {
			stream.WriteNil()
			return fmt.Errorf("invalid block number for hash: %s", hash.Hex())
		}
	} else {
		stream.WriteNil()
		return errors.New("invalid block number or hash")
	}

	// Get block details
	blockInfo, err := api.stateProvider.GetBlockByNumber(ctx, rpc.BlockNumber(blockNum), true)
	if err != nil {
		stream.WriteNil()
		return err
	}

	// Create header from block info
	header := &types.Header{
		ParentHash: common.HexToHash(blockInfo["parentHash"].(string)),
		Root:       common.HexToHash(blockInfo["stateRoot"].(string)),
		Number:     new(big.Int).SetUint64(blockNum),
	}

	// Parse timestamp if available
	if timestamp, ok := blockInfo["timestamp"]; ok {
		if timestampHex, ok := timestamp.(string); ok && strings.HasPrefix(timestampHex, "0x") {
			timestampVal, err := hexutil.DecodeUint64(timestampHex)
			if err == nil {
				header.Time = timestampVal
			}
		}
	}

	// Set up default config if none provided
	if config == nil {
		config = &tracersConfig.TraceConfig{}
	}

	// Get chain config
	chainConfig := api.stateProvider.GetChainConfig()

	// Create a consensus engine (using consensus.EngineReader interface)
	var consensusEngine consensus.EngineReader = ethash.New(ethashcfg.Config{
		PowMode:       ethashcfg.ModeNormal,
		CachesInMem:   3,
		DatasetsInMem: 1,
	}, nil, false)

	// Construct block hash getter function
	getHashFn := func(n uint64) common.Hash {
		if n > blockNum {
			return common.Hash{}
		}

		// Try to get block hash from Redis
		blockKey := fmt.Sprintf("block:%d", n)
		blockData, err := api.stateProvider.GetRedisClient().HGet(ctx, blockKey, "hash").Result()
		if err != nil || blockData == "" {
			return common.Hash{}
		}

		return common.HexToHash(blockData)
	}

	// Create state reader for this block
	stateReader := NewPointInTimeRedisStateReader(api.stateProvider.GetRedisClient(), blockNum)
	defer stateReader.Close()

	// Create intra-block state
	ibs := state.New(stateReader)

	// Create block context
	blockCtx := core.NewEVMBlockContext(header, getHashFn, consensusEngine, nil, chainConfig)

	blockHash := common.HexToHash(blockInfo["hash"].(string))

	// Start writing the response
	stream.WriteArrayStart()

	// Get transactions for this block
	txs, err := api.stateProvider.GetTransactionsByBlockNumber(ctx, blockNum)
	if err != nil {
		api.logger.Warn("Failed to retrieve transactions", "block", blockNum, "err", err)
		// Continue with empty transaction list
		txs = make([]types.Transaction, 0)
	}

	// Define call timeout
	callTimeout := 5 * time.Second

	// Apply each transaction in order
	var usedGas uint64

	for txIndex, tx := range txs {
		// Create basic transaction data map
		txData := map[string]interface{}{
			"hash": tx.Hash().Hex(),
			"from": "0x0000000000000000000000000000000000000000", // Would need to recover sender
		}

		// Add other transaction fields
		if to := tx.GetTo(); to != nil {
			txData["to"] = to.Hex()
		}

		txData["value"] = tx.GetValue().Hex()
		txData["gas"] = hexutil.EncodeUint64(tx.GetGas())
		txData["gasPrice"] = tx.GetPrice().Hex()
		txData["input"] = "0x" + common.Bytes2Hex(tx.GetData())
		txData["nonce"] = hexutil.EncodeUint64(tx.GetNonce())

		stream.WriteObjectStart()

		// Add transaction hash to output
		txHash := common.HexToHash(txData["hash"].(string))
		stream.WriteObjectField("txHash")
		stream.WriteString(txHash.Hex())
		stream.WriteMore()
		stream.WriteObjectField("result")

		// Construct transaction message
		var msg types.Message
		var txCtx evmtypes.TxContext

		// Set transaction-specific state
		ibs.SetTxContext(txIndex)

		// Retrieve "from" address
		from := common.HexToAddress(txData["from"].(string))

		// Retrieve "to" address (might be nil for contract creation)
		var to *common.Address
		if toStr, ok := txData["to"].(string); ok && toStr != "" && toStr != "null" {
			toAddr := common.HexToAddress(toStr)
			to = &toAddr
		}

		// Parse transaction value
		value := new(uint256.Int)
		if valueStr, ok := txData["value"].(string); ok && valueStr != "" {
			// Remove 0x prefix if present
			if strings.HasPrefix(valueStr, "0x") {
				valueStr = valueStr[2:]
			}
			value.SetFromHex(valueStr)
		}

		// Parse gas limit
		var gas uint64 = 0
		if gasStr, ok := txData["gas"].(string); ok && gasStr != "" {
			if strings.HasPrefix(gasStr, "0x") {
				gas, err = hexutil.DecodeUint64(gasStr)
				if err != nil {
					gas = 0
				}
			} else {
				gas, err = strconv.ParseUint(gasStr, 10, 64)
				if err != nil {
					gas = 0
				}
			}
		}

		// Parse gas price
		gasPrice := new(uint256.Int)
		if gasPriceStr, ok := txData["gasPrice"].(string); ok && gasPriceStr != "" {
			// Remove 0x prefix if present
			if strings.HasPrefix(gasPriceStr, "0x") {
				gasPriceStr = gasPriceStr[2:]
			}
			gasPrice.SetFromHex(gasPriceStr)
		}

		// Parse input data
		var data []byte
		if inputStr, ok := txData["input"].(string); ok && inputStr != "" {
			data = common.FromHex(inputStr)
		}

		// Create the message
		msg = types.NewMessage(
			from,     // from
			to,       // to
			0,        // nonce (not used for tracing)
			value,    // value
			gas,      // gas limit
			gasPrice, // gas price
			gasPrice, // fee cap (same as gas price for legacy txs)
			gasPrice, // tip cap (same as gas price for legacy txs)
			data,     // data
			nil,      // access list
			false,    // check nonce
			false,    // is free
			nil,      // blob fee
		)

		// Set up transaction context
		txCtx = core.NewEVMTxContext(msg)

		// Copy rules from chain config
		rules := chainConfig.Rules(blockNum, blockCtx.Time)

		// Trace the transaction
		txGas, err := traceTx(ctx, msg, blockCtx, txCtx, blockHash, txIndex, ibs, config, chainConfig, stream, callTimeout)
		if err != nil {
			// We continue with other transactions even if one fails
			api.logger.Warn("Transaction tracing failed", "tx", txHash.Hex(), "err", err)
		}

		// Update used gas
		usedGas += txGas

		// Finalize transaction state changes
		if err == nil {
			err = ibs.FinalizeTx(rules, state.NewNoopWriter())
			if err != nil {
				api.logger.Warn("Failed to finalize transaction", "tx", txHash.Hex(), "err", err)
			}
		}

		stream.WriteObjectEnd()

		// Add separator between transactions
		if len(txs) > 1 && txIndex < len(txs)-1 {
			stream.WriteMore()
		}

		// Flush stream to avoid buffer overflow
		if err := stream.Flush(); err != nil {
			return err
		}
	}

	stream.WriteArrayEnd()
	if err := stream.Flush(); err != nil {
		return err
	}

	return nil
}

// traceTx executes a transaction and traces it using the specified configuration
// This is an implementation similar to transactions.TraceTx from Erigon
func traceTx(
	ctx context.Context,
	message types.Message,
	blockCtx evmtypes.BlockContext,
	txCtx evmtypes.TxContext,
	blockHash common.Hash,
	txIndex int,
	ibs evmtypes.IntraBlockState,
	config *tracersConfig.TraceConfig,
	chainConfig *chain.Config,
	stream *jsoniter.Stream,
	callTimeout time.Duration,
) (usedGas uint64, err error) {
	// Set up the tracer according to config
	var tracer vm.EVMLogger
	var streaming bool
	var cancel context.CancelFunc

	switch {
	case config != nil && config.Tracer != nil:
		// Set up a custom JavaScript tracer
		timeout := callTimeout
		if config.Timeout != nil {
			var err error
			timeout, err = time.ParseDuration(*config.Timeout)
			if err != nil {
				stream.WriteNil()
				return 0, err
			}
		}

		// Construct the JavaScript tracer
		cfg := json.RawMessage("{}")
		if config != nil && config.TracerConfig != nil {
			cfg = *config.TracerConfig
		}

		tracer, err = tracers.New(*config.Tracer, &tracers.Context{
			TxHash:    txCtx.TxHash,
			TxIndex:   txIndex,
			BlockHash: blockHash,
		}, cfg)
		if err != nil {
			stream.WriteNil()
			return 0, err
		}

		// Set up timeout context
		deadlineCtx, cancelFn := context.WithTimeout(ctx, timeout)
		cancel = cancelFn
		go func() {
			<-deadlineCtx.Done()
			if errors.Is(deadlineCtx.Err(), context.DeadlineExceeded) {
				//	tracer.Stop(errors.New("execution timeout"))
			}
		}()

		streaming = false
	case config == nil:
		// Default JSON logger
		tracer = logger.NewJsonStreamLogger(nil, ctx, stream)
		streaming = true
		cancel = func() {}
	default:
		// JSON logger with config
		tracer = logger.NewJsonStreamLogger(config.LogConfig, ctx, stream)
		streaming = true
		cancel = func() {}
	}

	defer cancel()

	// Run the transaction with tracing enabled
	evm := vm.NewEVM(blockCtx, txCtx, ibs, chainConfig, vm.Config{Debug: true, Tracer: tracer, NoBaseFee: true})

	// Determine if refunds should be applied
	var refunds = true
	if config != nil && config.NoRefunds != nil && *config.NoRefunds {
		refunds = false
	}

	// If we're using the structured logger, prepare output format
	if streaming {
		stream.WriteObjectStart()
		stream.WriteObjectField("structLogs")
		stream.WriteArrayStart()
	}

	// Execute the transaction
	gp := new(core.GasPool).AddGas(message.Gas())
	if message.BlobGas() > 0 {
		gp.AddBlobGas(message.BlobGas())
	}

	result, err := core.ApplyMessage(evm, message, gp, refunds, false /* gasBailout */)
	if err != nil {
		if streaming {
			stream.WriteArrayEnd()
			stream.WriteObjectEnd()
		} else {
			stream.WriteNil()
		}
		return 0, fmt.Errorf("tracing failed: %w", err)
	}

	usedGas = result.UsedGas

	// Format and return output based on tracer type
	if streaming {
		// For structured logger, format the output
		stream.WriteArrayEnd()
		stream.WriteMore()
		stream.WriteObjectField("gas")
		stream.WriteUint64(result.UsedGas)
		stream.WriteMore()
		stream.WriteObjectField("failed")
		stream.WriteBool(result.Failed())
		stream.WriteMore()

		// If the result contains a revert reason, return it
		returnVal := fmt.Sprintf("%x", result.Return())
		if len(result.Revert()) > 0 {
			returnVal = fmt.Sprintf("%x", result.Revert())
		}
		stream.WriteObjectField("returnValue")
		stream.WriteString(returnVal)
		stream.WriteObjectEnd()
	} else {
		// For JavaScript tracers, get the custom result
		r, err := tracer.(tracers.Tracer).GetResult()
		if err != nil {
			stream.WriteNil()
			return usedGas, err
		}

		_, err = stream.Write(r)
		if err != nil {
			stream.WriteNil()
			return usedGas, err
		}
	}

	return usedGas, nil
}

// TraceTransaction implements debug_traceTransaction. Returns Geth style transaction traces.
func (api *DebugAPI) TraceTransaction(ctx context.Context, hash common.Hash, config *tracersConfig.TraceConfig, stream *jsoniter.Stream) error {
	// Look up the transaction
	api.logger.Info("Tracing transaction", "hash", hash.Hex())

	// Find the block number for this transaction
	txnKey := fmt.Sprintf("tx:%s", hash.Hex())
	txInfoStr, err := api.stateProvider.GetRedisClient().Get(ctx, txnKey).Result()
	if err != nil {
		stream.WriteNil()
		return fmt.Errorf("transaction not found: %s", hash.Hex())
	}

	var txInfo map[string]interface{}
	if err := json.Unmarshal([]byte(txInfoStr), &txInfo); err != nil {
		stream.WriteNil()
		return fmt.Errorf("failed to parse transaction info: %w", err)
	}

	// Extract block number and index
	blockNumStr, ok := txInfo["blockNumber"].(string)
	if !ok {
		stream.WriteNil()
		return fmt.Errorf("invalid transaction info format: missing blockNumber")
	}

	blockNum, err := strconv.ParseUint(blockNumStr, 10, 64)
	if err != nil {
		// Try parsing as hex if decimal fails
		if strings.HasPrefix(blockNumStr, "0x") {
			blockNum, err = hexutil.DecodeUint64(blockNumStr)
			if err != nil {
				stream.WriteNil()
				return fmt.Errorf("invalid block number format: %s", blockNumStr)
			}
		} else {
			stream.WriteNil()
			return fmt.Errorf("invalid block number format: %s", blockNumStr)
		}
	}

	txIndexStr, ok := txInfo["transactionIndex"].(string)
	if !ok {
		stream.WriteNil()
		return fmt.Errorf("invalid transaction info format: missing transactionIndex")
	}

	txIndex, err := strconv.ParseUint(txIndexStr, 10, 64)
	if err != nil {
		// Try parsing as hex if decimal fails
		if strings.HasPrefix(txIndexStr, "0x") {
			txIndex, err = hexutil.DecodeUint64(txIndexStr)
			if err != nil {
				stream.WriteNil()
				return fmt.Errorf("invalid transaction index format: %s", txIndexStr)
			}
		} else {
			stream.WriteNil()
			return fmt.Errorf("invalid transaction index format: %s", txIndexStr)
		}
	}

	// Get block details
	blockInfo, err := api.stateProvider.GetBlockByNumber(ctx, rpc.BlockNumber(blockNum), false)
	if err != nil {
		stream.WriteNil()
		return fmt.Errorf("block not found: %d", blockNum)
	}

	// Create header from block info
	header := &types.Header{
		ParentHash: common.HexToHash(blockInfo["parentHash"].(string)),
		Root:       common.HexToHash(blockInfo["stateRoot"].(string)),
		Number:     new(big.Int).SetUint64(blockNum),
	}

	// Parse timestamp if available
	if timestamp, ok := blockInfo["timestamp"]; ok {
		if timestampHex, ok := timestamp.(string); ok && strings.HasPrefix(timestampHex, "0x") {
			timestampVal, err := hexutil.DecodeUint64(timestampHex)
			if err == nil {
				header.Time = timestampVal
			}
		}
	}

	// Set up default config if none provided
	if config == nil {
		config = &tracersConfig.TraceConfig{}
	}

	// Get chain config
	chainConfig := api.stateProvider.GetChainConfig()

	// Create a consensus engine
	var consensusEngine consensus.EngineReader = ethash.New(ethashcfg.Config{
		PowMode:       ethashcfg.ModeNormal,
		CachesInMem:   3,
		DatasetsInMem: 1,
	}, nil, false)

	// Construct block hash getter function
	getHashFn := func(n uint64) common.Hash {
		if n > blockNum {
			return common.Hash{}
		}

		// Try to get block hash from Redis
		blockKey := fmt.Sprintf("block:%d", n)
		blockData, err := api.stateProvider.GetRedisClient().HGet(ctx, blockKey, "hash").Result()
		if err != nil || blockData == "" {
			return common.Hash{}
		}

		return common.HexToHash(blockData)
	}

	// Create state reader for this block
	stateReader := NewPointInTimeRedisStateReader(api.stateProvider.GetRedisClient(), blockNum)
	defer stateReader.Close()

	// Create intra-block state
	ibs := state.New(stateReader)

	// Create block context
	blockCtx := core.NewEVMBlockContext(header, getHashFn, consensusEngine, nil, chainConfig)

	blockHash := common.HexToHash(blockInfo["hash"].(string))

	// Get the specific transaction data
	txKey := fmt.Sprintf("txs:%d", blockNum)
	txDataStr, err := api.stateProvider.GetRedisClient().HGet(ctx, txKey, fmt.Sprintf("%d", txIndex)).Result()
	if err != nil {
		stream.WriteNil()
		return fmt.Errorf("transaction not found in block: %d, index: %d", blockNum, txIndex)
	}

	// Parse transaction data
	var txData map[string]interface{}
	if err := json.Unmarshal([]byte(txDataStr), &txData); err != nil {
		stream.WriteNil()
		return fmt.Errorf("failed to parse transaction data: %w", err)
	}

	// Set transaction-specific state
	ibs.SetTxContext(int(txIndex))

	// Construct transaction message
	from := common.HexToAddress(txData["from"].(string))

	// Retrieve "to" address (might be nil for contract creation)
	var to *common.Address
	if toStr, ok := txData["to"].(string); ok && toStr != "" && toStr != "null" {
		toAddr := common.HexToAddress(toStr)
		to = &toAddr
	}

	// Parse transaction value
	value := new(uint256.Int)
	if valueStr, ok := txData["value"].(string); ok && valueStr != "" {
		// Remove 0x prefix if present
		if strings.HasPrefix(valueStr, "0x") {
			valueStr = valueStr[2:]
		}
		value.SetFromHex(valueStr)
	}

	// Parse gas limit
	var gas uint64 = 0
	if gasStr, ok := txData["gas"].(string); ok && gasStr != "" {
		if strings.HasPrefix(gasStr, "0x") {
			gas, err = hexutil.DecodeUint64(gasStr)
			if err != nil {
				gas = 0
			}
		} else {
			gas, err = strconv.ParseUint(gasStr, 10, 64)
			if err != nil {
				gas = 0
			}
		}
	}

	// Parse gas price
	gasPrice := new(uint256.Int)
	if gasPriceStr, ok := txData["gasPrice"].(string); ok && gasPriceStr != "" {
		// Remove 0x prefix if present
		if strings.HasPrefix(gasPriceStr, "0x") {
			gasPriceStr = gasPriceStr[2:]
		}
		gasPrice.SetFromHex(gasPriceStr)
	}

	// Parse input data
	var data []byte
	if inputStr, ok := txData["input"].(string); ok && inputStr != "" {
		data = common.FromHex(inputStr)
	}

	// Create the message
	msg := types.NewMessage(
		from,     // from
		to,       // to
		0,        // nonce (not used for tracing)
		value,    // value
		gas,      // gas limit
		gasPrice, // gas price
		gasPrice, // fee cap (same as gas price for legacy txs)
		gasPrice, // tip cap (same as gas price for legacy txs)
		data,     // data
		nil,      // access list
		false,    // check nonce
		false,    // is free
		nil,      // blob fee
	)

	// Set up transaction context
	txCtx := core.NewEVMTxContext(msg)

	// Define call timeout
	callTimeout := 5 * time.Second

	// Trace the transaction
	_, err = traceTx(ctx, msg, blockCtx, txCtx, blockHash, int(txIndex), ibs, config, chainConfig, stream, callTimeout)
	return err
}

// TraceCall implements debug_traceCall.
func (api *DebugAPI) TraceCall(ctx context.Context, args CallArgs, blockNrOrHash rpc.BlockNumberOrHash, config *tracersConfig.TraceConfig, stream *jsoniter.Stream) error {
	api.logger.Info("Tracing call", "block", blockNrOrHash, "from", args.From, "to", args.To)

	// Convert block number or hash to a specific block number
	var blockNum uint64
	var blockHash common.Hash

	if blockNumber, ok := blockNrOrHash.Number(); ok {
		switch blockNumber {
		case rpc.LatestBlockNumber, rpc.PendingBlockNumber:
			latest, err := api.stateProvider.GetLatestBlockNumber(ctx)
			if err != nil {
				stream.WriteNil()
				return err
			}
			blockNum = latest
		case rpc.EarliestBlockNumber:
			blockNum = 0
		default:
			blockNum = uint64(blockNumber)
		}

		// Get the hash for this block number
		blockInfo, err := api.stateProvider.GetBlockByNumber(ctx, rpc.BlockNumber(blockNum), false)
		if err != nil {
			stream.WriteNil()
			return err
		}
		blockHash = common.HexToHash(blockInfo["hash"].(string))
	} else if hash, ok := blockNrOrHash.Hash(); ok {
		blockHash = hash
		// Get block number from hash using Redis
		hashKey := fmt.Sprintf("blockHash:%s", hash.Hex())
		blockNumStr, err := api.stateProvider.GetRedisClient().Get(ctx, hashKey).Result()
		if err != nil {
			stream.WriteNil()
			return fmt.Errorf("block hash not found: %s", hash.Hex())
		}
		blockNum, err = strconv.ParseUint(blockNumStr, 10, 64)
		if err != nil {
			stream.WriteNil()
			return fmt.Errorf("invalid block number for hash: %s", hash.Hex())
		}
	} else {
		stream.WriteNil()
		return errors.New("invalid block number or hash")
	}

	// Get block details
	blockInfo, err := api.stateProvider.GetBlockByNumber(ctx, rpc.BlockNumber(blockNum), false)
	if err != nil {
		stream.WriteNil()
		return err
	}

	// Create header from block info
	header := &types.Header{
		ParentHash: common.HexToHash(blockInfo["parentHash"].(string)),
		Root:       common.HexToHash(blockInfo["stateRoot"].(string)),
		Number:     new(big.Int).SetUint64(blockNum),
	}

	// Parse timestamp if available
	if timestamp, ok := blockInfo["timestamp"]; ok {
		if timestampHex, ok := timestamp.(string); ok && strings.HasPrefix(timestampHex, "0x") {
			timestampVal, err := hexutil.DecodeUint64(timestampHex)
			if err == nil {
				header.Time = timestampVal
			}
		}
	}

	// Set up default config if none provided
	if config == nil {
		config = &tracersConfig.TraceConfig{}
	}

	// Get chain config
	chainConfig := api.stateProvider.GetChainConfig()

	// Create a consensus engine
	var consensusEngine consensus.EngineReader = ethash.New(ethashcfg.Config{
		PowMode:       ethashcfg.ModeNormal,
		CachesInMem:   3,
		DatasetsInMem: 1,
	}, nil, false)

	// Construct block hash getter function
	getHashFn := func(n uint64) common.Hash {
		if n > blockNum {
			return common.Hash{}
		}

		// Try to get block hash from Redis
		blockKey := fmt.Sprintf("block:%d", n)
		blockData, err := api.stateProvider.GetRedisClient().HGet(ctx, blockKey, "hash").Result()
		if err != nil || blockData == "" {
			return common.Hash{}
		}

		return common.HexToHash(blockData)
	}

	// Create state reader for this block
	stateReader := NewPointInTimeRedisStateReader(api.stateProvider.GetRedisClient(), blockNum)
	defer stateReader.Close()

	// Create intra-block state
	ibs := state.New(stateReader)

	// Apply state overrides if specified
	if config != nil && config.StateOverrides != nil {
		err := config.StateOverrides.Override(ibs)
		if err != nil {
			stream.WriteNil()
			return fmt.Errorf("state override failed: %w", err)
		}
	}

	// Create block context
	blockCtx := core.NewEVMBlockContext(header, getHashFn, consensusEngine, nil, chainConfig)

	// Set default gas cap if not provided
	var gasCap uint64 = 50_000_000 // High default gas limit for trace_call

	// Handle BaseFee which might be present in the header
	var baseFee *uint256.Int
	if header.BaseFee != nil {
		var overflow bool
		baseFee, overflow = uint256.FromBig(header.BaseFee)
		if overflow {
			stream.WriteNil()
			return fmt.Errorf("header.BaseFee uint256 overflow")
		}
	}

	// Convert CallArgs to Message
	msg, err := args.ToMessage(gasCap, baseFee)
	if err != nil {
		stream.WriteNil()
		return err
	}

	// Create transaction context
	txCtx := core.NewEVMTxContext(msg)

	// Define call timeout
	callTimeout := 5 * time.Second

	// Trace the call
	_, err = traceTx(ctx, msg, blockCtx, txCtx, blockHash, 0, ibs, config, chainConfig, stream, callTimeout)
	return err
}
