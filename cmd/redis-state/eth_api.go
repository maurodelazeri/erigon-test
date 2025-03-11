package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/common/hexutil"
	"github.com/erigontech/erigon-lib/common/hexutility"
	"github.com/erigontech/erigon-lib/common/math"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon-lib/rlp"
	"github.com/erigontech/erigon/consensus"
	"github.com/erigontech/erigon/consensus/ethash"
	"github.com/erigontech/erigon/consensus/ethash/ethashcfg"
	"github.com/erigontech/erigon/core"
	"github.com/erigontech/erigon/core/state"
	"github.com/erigontech/erigon/core/types"
	"github.com/erigontech/erigon/core/vm"
	"github.com/erigontech/erigon/core/vm/evmtypes"
	"github.com/erigontech/erigon/rpc"
	"github.com/holiman/uint256"
)

// API implementations
type EthAPI struct {
	stateProvider *RedisStateProvider
	logger        log.Logger
}

// BlockNumber returns the latest block number
func (api *EthAPI) BlockNumber(ctx context.Context) (uint64, error) {
	return api.stateProvider.GetLatestBlockNumber(ctx)
}

// CallArgs represents the arguments for eth_call
type CallArgs struct {
	From                 *libcommon.Address `json:"from"`
	To                   *libcommon.Address `json:"to"`
	Gas                  *hexutil.Uint64    `json:"gas"`
	GasPrice             *hexutil.Big       `json:"gasPrice"`
	MaxFeePerGas         *hexutil.Big       `json:"maxFeePerGas"`
	MaxPriorityFeePerGas *hexutil.Big       `json:"maxPriorityFeePerGas"`
	MaxFeePerBlobGas     *hexutil.Big       `json:"maxFeePerBlobGas"`
	Value                *hexutil.Big       `json:"value"`
	Nonce                *hexutil.Uint64    `json:"nonce"`
	Data                 *hexutility.Bytes  `json:"data"`
	Input                *hexutility.Bytes  `json:"input"`
	AccessList           *types.AccessList  `json:"accessList"`
	ChainID              *hexutil.Big       `json:"chainId,omitempty"`
}

// from retrieves the transaction sender address.
func (arg *CallArgs) from() libcommon.Address {
	if arg.From == nil {
		return libcommon.Address{}
	}
	return *arg.From
}

// ToMessage converts CallArgs to the Message type used by the core evm
func (args *CallArgs) ToMessage(globalGasCap uint64, baseFee *uint256.Int) (types.Message, error) {
	// Reject invalid combinations of pre- and post-1559 fee styles
	if args.GasPrice != nil && (args.MaxFeePerGas != nil || args.MaxPriorityFeePerGas != nil) {
		return types.Message{}, errors.New("both gasPrice and (maxFeePerGas or maxPriorityFeePerGas) specified")
	}
	// Set sender address or use zero address if none specified.
	addr := args.from()

	// Set default gas & gas price if none were set
	gas := globalGasCap
	if gas == 0 {
		gas = uint64(1<<63 - 1) // Use max int64 as default gas limit
	}
	if args.Gas != nil {
		gas = uint64(*args.Gas)
	}
	if globalGasCap != 0 && globalGasCap < gas {
		log.Warn("Caller gas above allowance, capping", "requested", gas, "cap", globalGasCap)
		gas = globalGasCap
	}

	var (
		gasPrice         *uint256.Int
		gasFeeCap        *uint256.Int
		gasTipCap        *uint256.Int
		maxFeePerBlobGas *uint256.Int
	)
	if baseFee == nil {
		// If there's no basefee, then it must be a non-1559 execution
		gasPrice = new(uint256.Int)
		if args.GasPrice != nil {
			overflow := gasPrice.SetFromBig(args.GasPrice.ToInt())
			if overflow {
				return types.Message{}, errors.New("args.GasPrice higher than 2^256-1")
			}
		}
		gasFeeCap, gasTipCap = gasPrice, gasPrice
	} else {
		// A basefee is provided, necessitating 1559-type execution
		if args.GasPrice != nil {
			// User specified the legacy gas field, convert to 1559 gas typing
			gasPrice = new(uint256.Int)
			overflow := gasPrice.SetFromBig(args.GasPrice.ToInt())
			if overflow {
				return types.Message{}, errors.New("args.GasPrice higher than 2^256-1")
			}
			gasFeeCap, gasTipCap = gasPrice, gasPrice
		} else {
			// User specified 1559 gas fields (or none), use those
			gasFeeCap = new(uint256.Int)
			if args.MaxFeePerGas != nil {
				overflow := gasFeeCap.SetFromBig(args.MaxFeePerGas.ToInt())
				if overflow {
					return types.Message{}, errors.New("args.GasPrice higher than 2^256-1")
				}
			}
			gasTipCap = new(uint256.Int)
			if args.MaxPriorityFeePerGas != nil {
				overflow := gasTipCap.SetFromBig(args.MaxPriorityFeePerGas.ToInt())
				if overflow {
					return types.Message{}, errors.New("args.GasPrice higher than 2^256-1")
				}
			}
			// Backfill the legacy gasPrice for EVM execution, unless we're all zeroes
			gasPrice = new(uint256.Int)
			if !gasFeeCap.IsZero() || !gasTipCap.IsZero() {
				gasPrice = math.Min256(new(uint256.Int).Add(gasTipCap, baseFee), gasFeeCap)
			}
		}
		if args.MaxFeePerBlobGas != nil {
			blobFee, overflow := uint256.FromBig(args.MaxFeePerBlobGas.ToInt())
			if overflow {
				return types.Message{}, errors.New("args.MaxFeePerBlobGas higher than 2^256-1")
			}
			maxFeePerBlobGas = blobFee
		}
	}

	value := new(uint256.Int)
	if args.Value != nil {
		overflow := value.SetFromBig(args.Value.ToInt())
		if overflow {
			return types.Message{}, errors.New("args.Value higher than 2^256-1")
		}
	}
	var data []byte
	if args.Input != nil {
		data = *args.Input
	} else if args.Data != nil {
		data = *args.Data
	}
	var accessList types.AccessList
	if args.AccessList != nil {
		accessList = *args.AccessList
	}

	msg := types.NewMessage(addr, args.To, 0, value, gas, gasPrice, gasFeeCap, gasTipCap, data, accessList, false /* checkNonce */, false /* isFree */, maxFeePerBlobGas)
	return msg, nil
}

// Call executes a message call transaction (eth_call)
func (api *EthAPI) Call(ctx context.Context, args CallArgs, blockNumber string) (hexutility.Bytes, error) {
	// Convert block number string to rpc.BlockNumber
	var blockNum rpc.BlockNumber
	if blockNumber == "latest" {
		blockNum = rpc.LatestBlockNumber
	} else if blockNumber == "pending" {
		blockNum = rpc.PendingBlockNumber
	} else if blockNumber == "earliest" {
		blockNum = rpc.EarliestBlockNumber
	} else {
		// Parse hex string
		if strings.HasPrefix(blockNumber, "0x") {
			num, err := strconv.ParseUint(blockNumber[2:], 16, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid block number: %s", blockNumber)
			}
			blockNum = rpc.BlockNumber(num)
		} else {
			// Try parsing as decimal
			num, err := strconv.ParseUint(blockNumber, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid block number: %s", blockNumber)
			}
			blockNum = rpc.BlockNumber(num)
		}
	}

	// Get block number (for historical queries)
	var uintBlockNum uint64
	switch blockNum {
	case rpc.LatestBlockNumber, rpc.PendingBlockNumber:
		latestNum, err := api.stateProvider.GetLatestBlockNumber(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get latest block number: %w", err)
		}
		uintBlockNum = latestNum
	case rpc.EarliestBlockNumber:
		uintBlockNum = 0
	default:
		uintBlockNum = uint64(blockNum)
	}

	// Get block info for execution context
	blockData, err := api.stateProvider.GetRedisClient().Get(ctx, fmt.Sprintf("block:%d", uintBlockNum)).Bytes()
	if err != nil {
		return nil, fmt.Errorf("failed to get block: %w", err)
	}

	// Decode RLP-encoded block header
	var header types.Header
	if err := rlp.DecodeBytes(blockData, &header); err != nil {
		return nil, fmt.Errorf("failed to decode block header: %w", err)
	}

	// Set default gas cap if not provided
	var gasCap uint64 = 50_000_000 // High default gas limit for eth_call
	if args.Gas != nil {
		gasCap = uint64(*args.Gas)
	}

	// For address logging
	var toStr string
	if args.To != nil {
		toStr = args.To.Hex()
	} else {
		toStr = "contract creation"
	}

	fromAddr := libcommon.Address{}
	if args.From != nil {
		fromAddr = *args.From
	}

	// Log the call parameters
	api.logger.Info("eth_call execution",
		"from", fromAddr.Hex(),
		"to", toStr,
		"block", blockNum)

	// Create state reader for the specific block
	stateReader := NewPointInTimeRedisStateReader(api.stateProvider.GetRedisClient(), uintBlockNum)
	defer stateReader.Close()

	// Create intra-block state
	stateDB := state.New(stateReader)

	// Prepare call timeout context
	callTimeout := 5 * time.Second
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	chainConfig := api.stateProvider.GetChainConfig()

	// Construct block hash getter function
	getHashFn := func(n uint64) libcommon.Hash {
		if n > uintBlockNum {
			return libcommon.Hash{}
		}

		// Try to get block hash from Redis
		blockKeyHash := fmt.Sprintf("block:%d", n)
		blockDataHash, err := api.stateProvider.GetRedisClient().Get(ctx, blockKeyHash).Bytes()
		if err != nil {
			return libcommon.Hash{}
		}

		// Decode the block header to get its hash
		var headerHash types.Header
		if err := rlp.DecodeBytes(blockDataHash, &headerHash); err != nil {
			return libcommon.Hash{}
		}

		return headerHash.Hash()
	}

	// Create a consensus engine (using consensus.EngineReader interface)
	var consensusEngine consensus.EngineReader = ethash.New(ethashcfg.Config{
		PowMode:       ethashcfg.ModeNormal,
		CachesInMem:   3,
		DatasetsInMem: 1,
	}, nil, false)

	// Create block context
	blockContext := core.NewEVMBlockContext(&header, getHashFn, consensusEngine, nil, chainConfig)

	// Handle BaseFee which might be present in the header
	var baseFee *uint256.Int
	if header.BaseFee != nil {
		var overflow bool
		baseFee, overflow = uint256.FromBig(header.BaseFee)
		if overflow {
			return nil, fmt.Errorf("header.BaseFee uint256 overflow")
		}
	}

	// Convert CallArgs to Message
	msg, err := args.ToMessage(gasCap, baseFee)
	if err != nil {
		return nil, err
	}

	// Create transaction context
	txContext := core.NewEVMTxContext(msg)

	// Create EVM configuration
	vmConfig := vm.Config{NoBaseFee: true}

	// Create EVM - using evmtypes for block and tx context
	evm := vm.NewEVM(evmtypes.BlockContext(blockContext), evmtypes.TxContext(txContext), stateDB, chainConfig, vmConfig)

	// Handle timeout
	go func() {
		<-callCtx.Done()
		evm.Cancel()
	}()

	// Execute call
	gasPool := new(core.GasPool).AddGas(msg.Gas())
	if msg.BlobGas() > 0 {
		gasPool.AddBlobGas(msg.BlobGas())
	}

	result, err := core.ApplyMessage(evm, msg, gasPool, true, false)
	if err != nil {
		return nil, fmt.Errorf("execution failed: %w", err)
	}

	// Check for timeout
	if evm.Cancelled() {
		return nil, fmt.Errorf("execution aborted (timeout = %v)", callTimeout)
	}

	// Check for revert
	if result.Err != nil {
		if len(result.ReturnData) > 0 {
			return nil, fmt.Errorf("execution reverted: %x", result.ReturnData)
		}
		return nil, result.Err
	}

	return result.Return(), nil
}

// GetBalance implements eth_getBalance
func (api *EthAPI) GetBalance(ctx context.Context, address libcommon.Address, blockNumber string) (*uint256.Int, error) {
	// Convert block number string to rpc.BlockNumber
	var blockNum rpc.BlockNumber
	if blockNumber == "latest" {
		blockNum = rpc.LatestBlockNumber
	} else if blockNumber == "pending" {
		blockNum = rpc.PendingBlockNumber
	} else if blockNumber == "earliest" {
		blockNum = rpc.EarliestBlockNumber
	} else {
		// Parse hex string
		if strings.HasPrefix(blockNumber, "0x") {
			num, err := strconv.ParseUint(blockNumber[2:], 16, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid block number: %s", blockNumber)
			}
			blockNum = rpc.BlockNumber(num)
		} else {
			// Try parsing as decimal
			num, err := strconv.ParseUint(blockNumber, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid block number: %s", blockNumber)
			}
			blockNum = rpc.BlockNumber(num)
		}
	}

	// Get balance
	balance, err := api.stateProvider.BalanceAt(ctx, address, blockNum)
	if err != nil {
		return nil, err
	}

	// Convert to uint256
	result, overflow := uint256.FromBig(balance)
	if overflow {
		return nil, fmt.Errorf("balance overflows uint256: %s", balance.String())
	}

	return result, nil
}

// GetCode returns the code at the given address and block number
func (api *EthAPI) GetCode(ctx context.Context, address libcommon.Address, blockNumber string) (string, error) {
	// Convert block number string to rpc.BlockNumber
	var blockNum rpc.BlockNumber
	if blockNumber == "latest" {
		blockNum = rpc.LatestBlockNumber
	} else if blockNumber == "pending" {
		blockNum = rpc.PendingBlockNumber
	} else if blockNumber == "earliest" {
		blockNum = rpc.EarliestBlockNumber
	} else {
		// Parse hex string
		if strings.HasPrefix(blockNumber, "0x") {
			num, err := strconv.ParseUint(blockNumber[2:], 16, 64)
			if err != nil {
				return "", fmt.Errorf("invalid block number: %s", blockNumber)
			}
			blockNum = rpc.BlockNumber(num)
		} else {
			// Try parsing as decimal
			num, err := strconv.ParseUint(blockNumber, 10, 64)
			if err != nil {
				return "", fmt.Errorf("invalid block number: %s", blockNumber)
			}
			blockNum = rpc.BlockNumber(num)
		}
	}

	// Get block number (for historical queries)
	var uintBlockNum uint64
	switch blockNum {
	case rpc.LatestBlockNumber, rpc.PendingBlockNumber:
		latestNum, err := api.stateProvider.GetLatestBlockNumber(ctx)
		if err != nil {
			return "", fmt.Errorf("failed to get latest block number: %w", err)
		}
		uintBlockNum = latestNum
	case rpc.EarliestBlockNumber:
		uintBlockNum = 0
	default:
		uintBlockNum = uint64(blockNum)
	}

	// Create reader for specified block
	reader := NewPointInTimeRedisStateReader(api.stateProvider.GetRedisClient(), uintBlockNum)
	defer reader.Close()

	// Get account data to find code hash and incarnation
	account, err := reader.ReadAccountData(address)
	if err != nil {
		return "", fmt.Errorf("failed to read account data: %w", err)
	}

	if account == nil {
		return "0x", nil // No account, no code
	}

	// Read code
	code, err := reader.ReadAccountCode(address, account.Incarnation)
	if err != nil {
		return "", fmt.Errorf("failed to read code: %w", err)
	}

	if len(code) == 0 {
		return "0x", nil // No code
	}

	// Return code as hex
	return fmt.Sprintf("0x%x", code), nil
}

// GetBlockByNumber returns the block details for the given block number
func (api *EthAPI) GetBlockByNumber(ctx context.Context, blockNumber rpc.BlockNumber, fullTx bool) (map[string]interface{}, error) {
	// Get block
	return api.stateProvider.GetBlockByNumber(ctx, blockNumber, fullTx)
}

// EstimateGas estimates the gas needed for execution of a transaction
func (api *EthAPI) EstimateGas(ctx context.Context, args CallArgs) (uint64, error) {
	// For simplicity, just return a reasonable gas estimate
	// In a full implementation, this would simulate the transaction
	gas := uint64(21000) // Minimum gas for a simple transfer

	// Add gas for transaction data if any
	if args.Data != nil && len(*args.Data) > 0 {
		data := *args.Data

		// Calculate gas for data bytes (4 gas for zero bytes, 16 for non-zero)
		zeroBytes := 0
		nonZeroBytes := 0

		for _, b := range data {
			if b == 0 {
				zeroBytes++
			} else {
				nonZeroBytes++
			}
		}

		gas += uint64(zeroBytes*4 + nonZeroBytes*16)
	}

	// If it's a contract call (to != nil and has code), add more gas
	if args.To != nil {
		code, err := api.GetCode(ctx, *args.To, "latest")
		if err == nil && code != "0x" {
			// This is a contract call, add more gas
			gas += 40000 // Add a base amount for contract execution
		}
	} else {
		// Contract creation needs more gas
		gas += 53000
	}

	// Cap at a reasonable maximum
	if gas > 10000000 {
		gas = 10000000
	}

	// Return the gas estimate
	return gas, nil
}
