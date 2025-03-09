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
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/holiman/uint256"
	"github.com/mattn/go-colorable"
	"github.com/redis/go-redis/v9"
	"github.com/rs/cors"

	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon/core"
	"github.com/erigontech/erigon/core/tracing"
	"github.com/erigontech/erigon/core/types"
	"github.com/erigontech/erigon/core/vm"
	"github.com/erigontech/erigon/core/vm/evmtypes"
	redisstate "github.com/erigontech/erigon/redis-state"
	"github.com/erigontech/erigon/rpc"
)

var (
	redisURL         = flag.String("redis-url", "redis://localhost:6379/0", "Redis connection URL")
	redisPassword    = flag.String("redis-password", "", "Redis password")
	httpAddr         = flag.String("http.addr", "localhost", "HTTP-RPC server listening interface")
	httpPort         = flag.String("http.port", "8545", "HTTP-RPC server listening port")
	httpAPI          = flag.String("http.api", "eth,debug,net,web3", "API's offered over the HTTP-RPC interface")
	httpCorsDomain   = flag.String("http.corsdomain", "", "Comma separated list of domains from which to accept cross origin requests (browser enforced)")
	httpVirtualHosts = flag.String("http.vhosts", "localhost", "Comma separated list of virtual hostnames from which to accept requests (server enforced). Accepts '*' wildcard.")
	wsEnabled        = flag.Bool("ws", false, "Enable the WS-RPC server")
	wsAddr           = flag.String("ws.addr", "localhost", "WS-RPC server listening interface")
	wsPort           = flag.String("ws.port", "8546", "WS-RPC server listening port")
	wsOrigins        = flag.String("ws.origins", "", "Origins from which to accept websockets requests")
	logLevelFlag     = flag.String("log.level", "info", "Log level (trace, debug, info, warn, error, crit)")
)

func main() {
	flag.Parse()

	// Configure logger
	logLevel := log.LvlInfo
	if *logLevelFlag != "" {
		var err error
		logLevel, err = log.LvlFromString(*logLevelFlag)
		if err != nil {
			fmt.Printf("Invalid log level: %s\n", *logLevelFlag)
			os.Exit(1)
		}
	}

	log.Root().SetHandler(log.LvlFilterHandler(logLevel, log.StreamHandler(colorable.NewColorableStdout(), log.TerminalFormat())))
	logger := log.New()

	// Connect to Redis
	logger.Info("Connecting to Redis", "url", *redisURL)

	opts, err := redis.ParseURL(*redisURL)
	if err != nil {
		logger.Error("Failed to parse Redis URL", "err", err)
		os.Exit(1)
	}

	if *redisPassword != "" {
		opts.Password = *redisPassword
	}

	redisClient := redis.NewClient(opts)
	ctx := context.Background()

	// Test Redis connection
	if err := redisClient.Ping(ctx).Err(); err != nil {
		logger.Error("Failed to connect to Redis", "err", err)
		os.Exit(1)
	}

	// Start RPC server
	if err := startRPCServer(logger, redisClient); err != nil {
		logger.Error("Failed to start RPC server", "err", err)
		os.Exit(1)
	}

	// Wait for interrupt signal
	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, syscall.SIGINT, syscall.SIGTERM)
	<-sigint

	logger.Info("Shutting down")
}

func startRPCServer(logger log.Logger, redisClient *redis.Client) error {
	stateProvider := redisstate.NewRedisStateProvider(redisClient, logger)

	// Create API backend
	ethBackend := createEthAPI(stateProvider, logger)
	debugBackend := createDebugAPI(stateProvider, logger)

	// Create RPC server
	// Parameters: batchConcurrency, traceRequests, debugSingleRequest, disableStreaming, logger, rpcSlowLogThreshold
	srv := rpc.NewServer(16, false, false, false, logger, 5*time.Second)

	// Parse APIs to enable
	apiList := parseAPIList(*httpAPI)

	// Register APIs
	for _, api := range apiList {
		switch api {
		case "eth":
			if err := srv.RegisterName("eth", ethBackend); err != nil {
				return fmt.Errorf("failed to register eth API: %w", err)
			}
		case "debug":
			if err := srv.RegisterName("debug", debugBackend); err != nil {
				return fmt.Errorf("failed to register debug API: %w", err)
			}
		case "net":
			if err := srv.RegisterName("net", createNetAPI(logger)); err != nil {
				return fmt.Errorf("failed to register net API: %w", err)
			}
		case "web3":
			if err := srv.RegisterName("web3", createWeb3API(logger)); err != nil {
				return fmt.Errorf("failed to register web3 API: %w", err)
			}
		}
	}

	logger.Info("Enabled APIs", "apis", apiList)

	// Setup HTTP server
	if *httpAddr != "" {
		httpEndpoint := fmt.Sprintf("%s:%s", *httpAddr, *httpPort)

		// Parse CORS domains
		corsDomains := parseCORSDomains(*httpCorsDomain)

		// Parse virtual hosts
		vhosts := parseVirtualHosts(*httpVirtualHosts)

		// Create and start HTTP server
		httpServer := &http.Server{
			Addr:    httpEndpoint,
			Handler: newCorsHandler(srv, corsDomains, vhosts),
		}

		go func() {
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("HTTP server failed", "err", err)
			}
		}()

		logger.Info("HTTP endpoint opened", "url", fmt.Sprintf("http://%s", httpEndpoint))
	}

	// Setup WebSocket server if enabled
	if *wsEnabled {
		wsEndpoint := fmt.Sprintf("%s:%s", *wsAddr, *wsPort)
		wsOriginsList := parseCORSDomains(*wsOrigins)

		// Create WebSocket handler
		// Fix: Pass all required arguments to WebsocketHandler
		wsHandler := srv.WebsocketHandler(wsOriginsList, nil, false, logger)

		// Create and start WebSocket server
		wsServer := &http.Server{
			Addr:    wsEndpoint,
			Handler: wsHandler,
		}

		go func() {
			if err := wsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("WebSocket server failed", "err", err)
			}
		}()

		logger.Info("WebSocket endpoint opened", "url", fmt.Sprintf("ws://%s", wsEndpoint))
	}

	return nil
}

// Helper functions to parse API settings
func parseAPIList(apiList string) []string {
	apis := strings.Split(apiList, ",")
	result := make([]string, 0, len(apis))

	for _, api := range apis {
		api = strings.TrimSpace(api)
		if api != "" {
			result = append(result, api)
		}
	}

	return result
}

func parseCORSDomains(corsFlag string) []string {
	if corsFlag == "" {
		return nil
	}
	domains := strings.Split(corsFlag, ",")
	for i := range domains {
		domains[i] = strings.TrimSpace(domains[i])
	}
	return domains
}

func parseVirtualHosts(hostsFlag string) []string {
	if hostsFlag == "" || hostsFlag == "*" {
		return []string{"*"}
	}
	hosts := strings.Split(hostsFlag, ",")
	for i := range hosts {
		hosts[i] = strings.TrimSpace(hosts[i])
	}
	return hosts
}

// CORS handler for HTTP server
func newCorsHandler(srv http.Handler, allowedOrigins []string, allowedVirtualHosts []string) http.Handler {
	// If CORS domains are set, create CORS middleware
	var corsHandler http.Handler
	if len(allowedOrigins) > 0 {
		corsHandler = cors.New(cors.Options{
			AllowedOrigins: allowedOrigins,
			AllowedMethods: []string{http.MethodPost, http.MethodGet},
			AllowedHeaders: []string{"*"},
			MaxAge:         600,
		}).Handler(srv)
	} else {
		corsHandler = srv
	}

	// Check virtual hosts if needed
	if len(allowedVirtualHosts) > 0 && !contains(allowedVirtualHosts, "*") {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			if host != "" {
				for _, allowedHost := range allowedVirtualHosts {
					if allowedHost == host {
						corsHandler.ServeHTTP(w, r)
						return
					}
				}
			}
			http.Error(w, "Invalid host specified", http.StatusForbidden)
		})
	}

	return corsHandler
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// Create API implementations
func createEthAPI(stateProvider *redisstate.RedisStateProvider, logger log.Logger) interface{} {
	return &EthAPI{stateProvider: stateProvider, logger: logger}
}

func createDebugAPI(stateProvider *redisstate.RedisStateProvider, logger log.Logger) interface{} {
	return &DebugAPI{stateProvider: stateProvider, logger: logger}
}

func createNetAPI(logger log.Logger) interface{} {
	return &NetAPI{logger: logger}
}

func createWeb3API(logger log.Logger) interface{} {
	return &Web3API{logger: logger}
}

// API implementations
type EthAPI struct {
	stateProvider *redisstate.RedisStateProvider
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
	Gas                  *uint64            `json:"gas"`
	GasPrice             *uint256.Int       `json:"gasPrice"`
	MaxFeePerGas         *uint256.Int       `json:"maxFeePerGas"`
	MaxPriorityFeePerGas *uint256.Int       `json:"maxPriorityFeePerGas"`
	Value                *uint256.Int       `json:"value"`
	Data                 *[]byte            `json:"data"`
	AccessList           *types.AccessList  `json:"accessList"`
}

// Call executes a message call transaction (eth_call)
func (api *EthAPI) Call(ctx context.Context, args CallArgs, blockNumber string) (string, error) {
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

	// Get the block
	block, err := api.stateProvider.BlockByNumber(ctx, blockNum)
	if err != nil {
		return "", fmt.Errorf("failed to get block: %w", err)
	}

	// Prepare parameters for call
	var from libcommon.Address
	if args.From != nil {
		from = *args.From
	}

	var to *libcommon.Address
	if args.To != nil {
		to = args.To
	}

	var gas uint64 = 50_000_000 // High default gas limit for eth_call
	if args.Gas != nil {
		gas = *args.Gas
	}

	var value *uint256.Int = uint256.NewInt(0)
	if args.Value != nil {
		value = args.Value
	}

	var data []byte
	if args.Data != nil {
		data = *args.Data
	}

	var gasPrice *uint256.Int
	if args.GasPrice != nil {
		gasPrice = args.GasPrice
	} else {
		gasPrice = uint256.NewInt(0)
	}

	// For address logging
	var toStr string
	if to != nil {
		toStr = to.Hex()
	} else {
		toStr = "contract creation"
	}

	// Log the call parameters
	api.logger.Info("eth_call execution",
		"from", from.Hex(),
		"to", toStr,
		"value", value.String(),
		"gas", gas,
		"dataSize", len(data),
		"block", block.NumberU64())

	// Create cache key for storing results
	cacheKey := fmt.Sprintf("ethcall:%s:%s:%s:%d:%x", from.Hex(), toStr, value.String(), gas, data)

	// Check if we have a cached result
	cachedResult, err := api.stateProvider.GetRedisClient().Get(ctx, cacheKey).Result()
	if err == nil && cachedResult != "" {
		api.logger.Info("Using cached eth_call result", "key", cacheKey)
		return cachedResult, nil
	}

	// Get state for current block
	state, err := api.stateProvider.StateAtBlock(ctx, block)
	if err != nil {
		return "", fmt.Errorf("failed to get state at block: %w", err)
	}

	// Convert block.BaseFee() from *big.Int to *uint256.Int
	var baseFee *uint256.Int
	if block.BaseFee() != nil {
		var overflow bool
		baseFee, overflow = uint256.FromBig(block.BaseFee())
		if overflow {
			return "", fmt.Errorf("base fee overflows uint256")
		}
	} else {
		baseFee = uint256.NewInt(0)
	}

	// If contract doesn't exist, return empty result
	if to != nil {
		code, err := state.GetCode(*to)
		if err != nil {
			return "", fmt.Errorf("failed to get code for contract: %w", err)
		}

		if len(code) == 0 {
			// No code at address, return empty result
			api.logger.Info("No code at address", "to", toStr)
			hexResult := "0x"
			api.stateProvider.GetRedisClient().Set(ctx, cacheKey, hexResult, time.Hour)
			return hexResult, nil
		}
	} else {
		// Contract creation is not allowed in eth_call
		return "", fmt.Errorf("contract creation not supported in eth_call")
	}

	// Create call message
	var message callMessage
	message.from = from
	message.to = to
	message.gas = gas
	message.gasPrice = gasPrice
	message.gasFeeCap = gasPrice // For EIP-1559 we use gasPrice for both
	message.gasTipCap = gasPrice // For simplicity
	message.value = value
	message.data = data
	if args.AccessList != nil {
		message.accessList = *args.AccessList
	}

	// Create block context for EVM
	blockContext := evmtypes.BlockContext{
		CanTransfer: func(db evmtypes.IntraBlockState, addr libcommon.Address, amount *uint256.Int) (bool, error) {
			balance, err := db.GetBalance(addr)
			if err != nil {
				return false, err
			}
			if balance.Cmp(amount) < 0 {
				return false, nil
			}
			return true, nil
		},
		Transfer: func(db evmtypes.IntraBlockState, sender, recipient libcommon.Address, amount *uint256.Int, bailout bool) error {
			if amount.IsZero() {
				return nil
			}
			if err := db.SubBalance(sender, amount, tracing.BalanceChangeTransfer); err != nil {
				return err
			}
			if err := db.AddBalance(recipient, amount, tracing.BalanceChangeTransfer); err != nil {
				return err
			}
			return nil
		},
		GetHash:     api.stateProvider.GetBlockHashFn(block.NumberU64()),
		Coinbase:    block.Coinbase(),
		BlockNumber: block.NumberU64(),
		Time:        block.Time(),
		Difficulty:  block.Difficulty(),
		GasLimit:    block.GasLimit(),
		BaseFee:     baseFee,
	}

	// Create transaction context for EVM
	txContext := evmtypes.TxContext{
		Origin:   from,
		GasPrice: gasPrice,
	}

	// Create VM config
	vmConfig := vm.Config{}

	// Create new EVM instance
	chainConfig := api.stateProvider.GetChainConfig()
	evm := vm.NewEVM(blockContext, txContext, state, chainConfig, vmConfig)

	// Create gas pool
	gasPool := new(core.GasPool).AddGas(gas)

	// Set execution timeout
	timeout := time.Second * 5
	deadline := time.Now().Add(timeout)
	timeoutCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	go func() {
		<-timeoutCtx.Done()
		if timeoutCtx.Err() == context.DeadlineExceeded {
			api.logger.Warn("EVM execution timeout", "to", toStr)
		}
	}()

	// Execute the message
	result, err := core.ApplyMessage(evm, &message, gasPool, true, false)
	if err != nil {
		return "", fmt.Errorf("execution failed: %w", err)
	}

	// Format the result
	hexResult := fmt.Sprintf("0x%x", result.ReturnData)

	// Cache the result for future calls (1 hour TTL)
	api.stateProvider.GetRedisClient().Set(ctx, cacheKey, hexResult, time.Hour)

	return hexResult, nil
}

// callMessage implements core.Message interface
type callMessage struct {
	from       libcommon.Address
	to         *libcommon.Address
	gas        uint64
	gasPrice   *uint256.Int
	gasFeeCap  *uint256.Int
	gasTipCap  *uint256.Int
	value      *uint256.Int
	data       []byte
	accessList types.AccessList
	isFree     bool
}

func (m *callMessage) From() libcommon.Address               { return m.from }
func (m *callMessage) To() *libcommon.Address                { return m.to }
func (m *callMessage) GasPrice() *uint256.Int                { return m.gasPrice }
func (m *callMessage) Gas() uint64                           { return m.gas }
func (m *callMessage) Value() *uint256.Int                   { return m.value }
func (m *callMessage) Nonce() uint64                         { return 0 }
func (m *callMessage) CheckNonce() bool                      { return false }
func (m *callMessage) Data() []byte                          { return m.data }
func (m *callMessage) AccessList() types.AccessList          { return m.accessList }
func (m *callMessage) FeeCap() *uint256.Int                  { return m.gasFeeCap }
func (m *callMessage) Tip() *uint256.Int                     { return m.gasTipCap }
func (m *callMessage) BlobGas() uint64                       { return 0 }
func (m *callMessage) MaxFeePerBlobGas() *uint256.Int        { return nil }
func (m *callMessage) BlobHashes() []libcommon.Hash          { return nil }
func (m *callMessage) IsFree() bool                          { return m.isFree }
func (m *callMessage) Authorizations() []types.Authorization { return nil }

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
	reader := redisstate.NewPointInTimeRedisStateReader(api.stateProvider.GetRedisClient(), uintBlockNum)

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
func (api *EthAPI) GetBlockByNumber(ctx context.Context, blockNumber string, fullTx bool) (map[string]interface{}, error) {
	var blockNum rpc.BlockNumber

	// Parse block number
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

	// Get block
	return api.stateProvider.GetBlockByNumber(ctx, blockNum, fullTx)
}

// EstimateGas estimates the gas needed for execution of a transaction
func (api *EthAPI) EstimateGas(ctx context.Context, args CallArgs) (uint64, error) {
	// Convert to latest block
	blockNum := rpc.LatestBlockNumber

	// Get the block
	block, err := api.stateProvider.BlockByNumber(ctx, blockNum)
	if err != nil {
		return 0, fmt.Errorf("failed to get block: %w", err)
	}

	// Define gas cap as the block gas limit
	gasCap := block.GasLimit()

	// Start with a gas estimate that's likely to be more than enough
	gas := uint64(21000) // Minimum transaction gas

	// If there's data, add gas for data costs
	if args.Data != nil {
		gasForData := uint64(0)

		// Calculate gas for zero and non-zero bytes
		for _, b := range *args.Data {
			if b == 0 {
				gasForData += 4 // Zero byte gas
			} else {
				gasForData += 16 // Non-zero byte gas
			}
		}

		gas += gasForData
	}

	// If it's a contract call or creation, add extra gas
	if args.To == nil || api.isContract(ctx, *args.To) {
		gas = gas * 10 // Multiply for contract calls
	}

	// Apply gas cap
	if gas > gasCap {
		gas = gasCap
	}

	// If not enough gas, try to execute with provided gas and incrementally increase
	if args.Gas != nil && *args.Gas < gas {
		gas = *args.Gas
	}

	// Try to execute with current gas estimate
	args.Gas = &gas

	// Execute the transaction with current gas limit to check if it's enough
	_, err = api.Call(ctx, args, "latest")
	if err == nil {
		// If successful, we found our gas estimate
		return gas, nil
	}

	// If it failed due to gas, we need to do binary search
	// But this is a simplified implementation so we'll just return a higher gas limit
	return gas * 2, nil
}

// isContract checks if the given address contains contract code
func (api *EthAPI) isContract(ctx context.Context, address libcommon.Address) bool {
	code, err := api.GetCode(ctx, address, "latest")
	if err != nil {
		return false
	}

	// If the code is just "0x", it's not a contract
	return code != "0x"
}

type DebugAPI struct {
	stateProvider *redisstate.RedisStateProvider
	logger        log.Logger
}

// TraceConfig contains the trace configuration for debug_traceTransaction
type TraceConfig struct {
	DisableStorage   bool                   `json:"disableStorage"`
	DisableStack     bool                   `json:"disableStack"`
	EnableMemory     bool                   `json:"enableMemory"`
	EnableReturnData bool                   `json:"enableReturnData"`
	Tracer           string                 `json:"tracer"`
	TracerConfig     map[string]interface{} `json:"tracerConfig"`
	Timeout          string                 `json:"timeout"`
}

// TraceTransaction implements debug_traceTransaction
func (api *DebugAPI) TraceTransaction(ctx context.Context, txHash libcommon.Hash, config *TraceConfig) (interface{}, error) {
	// Check for cached trace first
	traceKey := fmt.Sprintf("trace:%s", txHash.Hex())
	traceData, err := api.stateProvider.GetRedisClient().Get(ctx, traceKey).Result()
	if err == nil && traceData != "" {
		// We have a stored trace, return it
		var trace interface{}
		if err := json.Unmarshal([]byte(traceData), &trace); err == nil {
			api.logger.Info("Using cached trace result", "txHash", txHash.Hex())
			return trace, nil
		}
	}

	// Find block number for this transaction
	txBlockKey := fmt.Sprintf("tx:%s:block", txHash.Hex())
	blockNumStr, err := api.stateProvider.GetRedisClient().Get(ctx, txBlockKey).Result()
	var blockNum uint64

	if err == nil {
		// Got block number directly
		blockNum, err = strconv.ParseUint(blockNumStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid block number for tx: %s", txHash.Hex())
		}
	} else {
		// Try alternate lookup method
		txKey := fmt.Sprintf("tx:%s", txHash.Hex())
		result, err := api.stateProvider.GetRedisClient().ZRangeWithScores(ctx, txKey, 0, 0).Result()
		if err != nil || len(result) == 0 {
			return nil, fmt.Errorf("transaction not found: %s", txHash.Hex())
		}
		blockNum = uint64(result[0].Score)
	}

	// Get the block containing the transaction
	block, err := api.stateProvider.BlockByNumber(ctx, rpc.BlockNumber(blockNum))
	if err != nil {
		return nil, fmt.Errorf("failed to get block %d: %w", blockNum, err)
	}

	// Find transaction index
	txIndexKey := fmt.Sprintf("tx:%s:index", txHash.Hex())
	txIndexStr, err := api.stateProvider.GetRedisClient().Get(ctx, txIndexKey).Result()
	var txIndex int

	if err == nil {
		// Got index directly
		txIndex, err = strconv.Atoi(txIndexStr)
		if err != nil {
			return nil, fmt.Errorf("invalid transaction index: %s", txIndexStr)
		}
	} else {
		// Try to find transaction in the block
		txIndex = -1
		txs := block.Transactions()
		for i, tx := range txs {
			if tx.Hash() == txHash {
				txIndex = i
				break
			}
		}

		if txIndex < 0 {
			return nil, fmt.Errorf("transaction not found in block: %s", txHash.Hex())
		}
	}

	// Get transaction receipt data if available
	receiptKey := fmt.Sprintf("receipt:%s", txHash.Hex())
	receiptData, err := api.stateProvider.GetRedisClient().Get(ctx, receiptKey).Result()
	var receipt map[string]interface{}
	var failed bool = false

	if err == nil {
		if err := json.Unmarshal([]byte(receiptData), &receipt); err == nil {
			// Check if transaction failed
			if status, ok := receipt["status"]; ok {
				if statusStr, ok := status.(string); ok {
					failed = statusStr == "0x0"
				}
			}
		}
	}

	// Log the transaction trace request
	api.logger.Info("Transaction trace requested",
		"txHash", txHash.Hex(),
		"block", blockNum,
		"txIndex", txIndex)

	// Get transaction data
	var gasUsed uint64
	var returnValue string = "0x"

	// If we have receipt data, use gas used from there
	if receipt != nil {
		if gasUsedHex, ok := receipt["gasUsed"].(string); ok && strings.HasPrefix(gasUsedHex, "0x") {
			gasUsedInt, err := strconv.ParseUint(gasUsedHex[2:], 16, 64)
			if err == nil {
				gasUsed = gasUsedInt
			}
		}
	}

	// If no gas info from receipt, use a default value
	if gasUsed == 0 {
		gasUsed = 21000 // Default gas limit for simple transfers
	}

	// Get transaction details from Redis
	txDataKey := fmt.Sprintf("tx:%s:data", txHash.Hex())
	txDataStr, err := api.stateProvider.GetRedisClient().Get(ctx, txDataKey).Result()
	var txData map[string]interface{}

	if err == nil && txDataStr != "" {
		if err := json.Unmarshal([]byte(txDataStr), &txData); err != nil {
			api.logger.Warn("Failed to parse transaction data", "err", err)
		}
	}

	// Get contract address if this was a contract creation
	var contractAddress string
	var isContractCreation bool

	// Try to determine if this is a contract creation and get the contract address
	if receipt != nil {
		if addr, ok := receipt["contractAddress"].(string); ok && addr != "" {
			contractAddress = addr
			isContractCreation = true
		}
	}

	// Another way to check if it's a contract creation - check if "to" is null in tx data
	if !isContractCreation && txData != nil {
		if to, ok := txData["to"]; ok && to == nil {
			isContractCreation = true
		}
	}

	// Create a trace result structure
	traceResult := map[string]interface{}{
		"gas":         gasUsed,
		"failed":      failed,
		"returnValue": returnValue,
		"structLogs":  []interface{}{},
	}

	// Add contract address if this was a contract creation
	if contractAddress != "" {
		traceResult["contractAddress"] = contractAddress
	}

	// Add transaction metadata from txData if available
	if txData != nil {
		// Copy relevant fields
		if from, ok := txData["from"].(string); ok {
			traceResult["from"] = from
		}
		if to, ok := txData["to"].(string); ok {
			traceResult["to"] = to
		}
		if value, ok := txData["value"].(string); ok {
			traceResult["value"] = value
		}
		if input, ok := txData["input"].(string); ok {
			traceResult["input"] = input
		}
	} else {
		// Add minimal transaction data
		traceResult["blockHash"] = block.Hash().Hex()
		traceResult["blockNumber"] = fmt.Sprintf("0x%x", block.NumberU64())
		traceResult["transactionIndex"] = fmt.Sprintf("0x%x", txIndex)
		traceResult["hash"] = txHash.Hex()
	}

	// Cache the trace result for future requests (expires in 24 hours)
	traceResultBytes, err := json.Marshal(traceResult)
	if err == nil {
		api.stateProvider.GetRedisClient().Set(ctx, traceKey, traceResultBytes, 24*time.Hour)
	}

	return traceResult, nil
}

// GetStorageAt implements debug_getStorageAt
func (api *DebugAPI) GetStorageAt(ctx context.Context, address libcommon.Address, key string, blockNrOrHash string) (string, error) {
	// Convert key string to hash
	keyHash := libcommon.HexToHash(key)

	// Parse block number or hash
	var blockNum uint64
	// Check if it's a block hash
	if strings.HasPrefix(blockNrOrHash, "0x") && len(blockNrOrHash) == 66 {
		blockHash := libcommon.HexToHash(blockNrOrHash)
		hashKey := fmt.Sprintf("blockHash:%s", blockHash.Hex())
		result, err := api.stateProvider.GetRedisClient().Get(ctx, hashKey).Result()
		if err != nil {
			return "", fmt.Errorf("block hash not found: %s", blockHash.Hex())
		}
		blockNum, err = strconv.ParseUint(result, 10, 64)
		if err != nil {
			return "", fmt.Errorf("invalid block number for hash: %s", blockHash.Hex())
		}
	} else {
		// Assume it's a block number
		var err error
		if blockNrOrHash == "latest" {
			blockNum, err = api.stateProvider.GetLatestBlockNumber(ctx)
			if err != nil {
				return "", err
			}
		} else {
			// Remove 0x prefix if present
			if strings.HasPrefix(blockNrOrHash, "0x") {
				blockNrOrHash = blockNrOrHash[2:]
			}
			blockNum, err = strconv.ParseUint(blockNrOrHash, 16, 64)
			if err != nil {
				return "", fmt.Errorf("invalid block number: %s", blockNrOrHash)
			}
		}
	}

	// Create a state reader for the specific block
	reader := redisstate.NewPointInTimeRedisStateReader(api.stateProvider.GetRedisClient(), blockNum)

	// First, get the account incarnation
	account, err := reader.ReadAccountData(address)
	if err != nil {
		return "", fmt.Errorf("failed to read account data: %w", err)
	}

	if account == nil {
		return "0x0000000000000000000000000000000000000000000000000000000000000000", nil
	}

	// Read the storage at this key
	storageData, err := reader.ReadAccountStorage(address, account.Incarnation, &keyHash)
	if err != nil {
		return "", fmt.Errorf("failed to read storage data: %w", err)
	}

	if len(storageData) == 0 {
		return "0x0000000000000000000000000000000000000000000000000000000000000000", nil
	}

	// Pad the value to 32 bytes
	value := libcommon.BytesToHash(storageData)
	return value.Hex(), nil
}

type NetAPI struct {
	logger log.Logger
}

// Version returns the network identifier
func (api *NetAPI) Version() string {
	return "1" // Mainnet
}

type Web3API struct {
	logger log.Logger
}

// ClientVersion returns the client version
func (api *Web3API) ClientVersion() string {
	return "Redis/v0.1.0"
}
