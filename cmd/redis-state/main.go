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
	"flag"
	"fmt"
	"math/big"
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

	"github.com/erigontech/erigon-lib/chain"
	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/common/hexutil"
	"github.com/erigontech/erigon-lib/common/hexutility"
	"github.com/erigontech/erigon-lib/common/math"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon-lib/rlp"
	"github.com/erigontech/erigon-lib/types/accounts"
	"github.com/erigontech/erigon/consensus"
	"github.com/erigontech/erigon/consensus/ethash"
	"github.com/erigontech/erigon/consensus/ethash/ethashcfg"
	"github.com/erigontech/erigon/core"
	"github.com/erigontech/erigon/core/state"
	"github.com/erigontech/erigon/core/types"
	"github.com/erigontech/erigon/core/vm"
	"github.com/erigontech/erigon/core/vm/evmtypes"
	"github.com/erigontech/erigon/params"
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

	// Set reasonable Redis client options
	opts.PoolSize = 20 // Increase connection pool size for better concurrency
	opts.MinIdleConns = 5
	opts.MaxRetries = 3
	opts.PoolTimeout = time.Second * 10
	opts.ReadTimeout = time.Second * 30
	opts.WriteTimeout = time.Second * 30

	redisClient := redis.NewClient(opts)
	ctx := context.Background()

	// Test Redis connection
	if err := redisClient.Ping(ctx).Err(); err != nil {
		logger.Error("Failed to connect to Redis", "err", err)
		os.Exit(1)
	}

	// Test BSSL module availability
	logger.Info("Testing BSSL module availability")
	_, err = redisClient.Do(ctx, "BSSL.PING").Result()
	if err != nil {
		if strings.Contains(err.Error(), "unknown command") {
			logger.Error("BSSL module not loaded in Redis - this is required for redis-state to function", "err", err)
			logger.Error("Please load the BSSL module using: redis-server --loadmodule /path/to/bssl.so")
			os.Exit(1)
		} else {
			logger.Warn("BSSL module test returned non-fatal error", "err", err)
		}
	} else {
		logger.Info("BSSL module available and working")
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
	stateProvider := NewRedisStateProvider(redisClient, logger)

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

// ==========================================================================
// RedisStateProvider implementation
// ==========================================================================

// BlockHashJSON represents block hash to number mapping
type BlockHashJSON struct {
	Number    uint64 `json:"number"`
	Timestamp uint64 `json:"timestamp"`
}

// RedisStateProvider provides blockchain state access via Redis
type RedisStateProvider struct {
	redisClient *redis.Client
	logger      log.Logger
	chainConfig *chain.Config // Chain configuration
}

// NewRedisStateProvider creates a new RedisStateProvider instance
func NewRedisStateProvider(redisClient *redis.Client, logger log.Logger) *RedisStateProvider {
	// For simplicity, we're using mainnet chain config
	// In a production implementation, this would be configured based on the chain data
	chainConfig := params.MainnetChainConfig

	return &RedisStateProvider{
		redisClient: redisClient,
		logger:      logger,
		chainConfig: chainConfig,
	}
}

// GetRedisClient returns the underlying Redis client
func (r *RedisStateProvider) GetRedisClient() *redis.Client {
	return r.redisClient
}

// GetChainConfig returns the chain configuration
func (r *RedisStateProvider) GetChainConfig() *chain.Config {
	return r.chainConfig
}

// GetLatestBlockNumber retrieves the latest processed block number from Redis
func (r *RedisStateProvider) GetLatestBlockNumber(ctx context.Context) (uint64, error) {
	result, err := r.redisClient.Get(ctx, "currentBlock").Result()
	if err != nil {
		if err == redis.Nil {
			return 0, fmt.Errorf("no current block found in Redis")
		}
		return 0, err
	}

	blockNum, err := strconv.ParseUint(result, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse block number: %w", err)
	}

	return blockNum, nil
}

// GetBlockByNumber retrieves block data by block number
func (r *RedisStateProvider) GetBlockByNumber(ctx context.Context, blockNumber rpc.BlockNumber, fullTx bool) (map[string]interface{}, error) {
	// Convert block number to uint64
	var blockNum uint64

	switch blockNumber {
	case rpc.LatestBlockNumber, rpc.PendingBlockNumber:
		latest, err := r.GetLatestBlockNumber(ctx)
		if err != nil {
			return nil, err
		}
		blockNum = latest
	case rpc.EarliestBlockNumber:
		blockNum = 0
	default:
		blockNum = uint64(blockNumber)
	}

	// Get block data from Redis
	blockKey := fmt.Sprintf("block:%d", blockNum)
	blockData, err := r.redisClient.Get(ctx, blockKey).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil, fmt.Errorf("block %d not found", blockNum)
		}
		return nil, fmt.Errorf("failed to get block data: %w", err)
	}

	// Decode RLP-encoded block header
	var header types.Header
	if err := rlp.DecodeBytes(blockData, &header); err != nil {
		return nil, fmt.Errorf("failed to decode block header: %w", err)
	}

	// Format the block data
	blockInfo := make(map[string]interface{})

	// Set basic header fields
	blockInfo["number"] = hexutil.EncodeUint64(header.Number.Uint64())
	blockInfo["hash"] = header.Hash().Hex()
	blockInfo["parentHash"] = header.ParentHash.Hex()
	blockInfo["stateRoot"] = header.Root.Hex()
	blockInfo["timestamp"] = hexutil.EncodeUint64(header.Time)

	// Add additional fields
	blockInfo["nonce"] = hexutil.EncodeUint64(header.Nonce.Uint64())
	blockInfo["difficulty"] = (*hexutil.Big)(header.Difficulty)
	blockInfo["extraData"] = hexutil.Encode(header.Extra)
	blockInfo["size"] = hexutil.EncodeUint64(uint64(len(blockData)))
	blockInfo["gasLimit"] = hexutil.EncodeUint64(header.GasLimit)
	blockInfo["gasUsed"] = hexutil.EncodeUint64(header.GasUsed)
	blockInfo["miner"] = header.Coinbase.Hex()

	// Set totalDifficulty - in full implementation would be calculated
	blockInfo["totalDifficulty"] = (*hexutil.Big)(header.Difficulty)

	// Set empty uncles array
	blockInfo["uncles"] = []interface{}{}

	// Add empty transaction array or fetch transactions
	if fullTx {
		txs, err := r.GetTransactionsByBlockNumber(ctx, blockNum)
		if err == nil && len(txs) > 0 {
			txArray := make([]interface{}, 0, len(txs))
			// Parse and add transactions
			for i, tx := range txs {
				// This would decode the transaction in a full implementation
				// Here, we're just creating a placeholder
				txObj := map[string]interface{}{
					"hash":             tx.Hash().Hex(),
					"blockHash":        header.Hash().Hex(),
					"blockNumber":      blockInfo["number"],
					"transactionIndex": hexutil.EncodeUint64(uint64(i)),
					"from":             "0x0000000000000000000000000000000000000000", // Would need to recover sender
					"to":               tx.To().Hex(),                                // If contract creation, would be null
					"value":            (*hexutil.Big)(tx.Value().ToBig()),
					"gas":              hexutil.EncodeUint64(tx.Gas()),
					"gasPrice":         (*hexutil.Big)(tx.GasPrice().ToBig()),
					"input":            hexutil.Encode(tx.Data()),
					"nonce":            hexutil.EncodeUint64(tx.Nonce()),
				}
				txArray = append(txArray, txObj)
			}
			blockInfo["transactions"] = txArray
		} else {
			blockInfo["transactions"] = []interface{}{}
		}
	} else {
		// Just return transaction hashes
		txs, err := r.GetTransactionsByBlockNumber(ctx, blockNum)
		if err == nil && len(txs) > 0 {
			txHashes := make([]interface{}, 0, len(txs))
			for _, tx := range txs {
				txHashes = append(txHashes, tx.Hash().Hex())
			}
			blockInfo["transactions"] = txHashes
		} else {
			blockInfo["transactions"] = []interface{}{}
		}
	}

	return blockInfo, nil
}

// GetTransactionsByBlockNumber retrieves all transactions in a block
func (r *RedisStateProvider) GetTransactionsByBlockNumber(ctx context.Context, blockNum uint64) ([]types.Transaction, error) {
	// Find all transaction keys for this block
	pattern := fmt.Sprintf("txs:%d:*", blockNum)
	txKeys, err := r.redisClient.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to find transaction keys: %w", err)
	}

	if len(txKeys) == 0 {
		return []types.Transaction{}, nil
	}

	// Get each transaction and parse it
	txs := make([]types.Transaction, 0, len(txKeys))

	for _, key := range txKeys {
		// Get transaction data
		data, err := r.redisClient.Get(ctx, key).Bytes()
		if err != nil {
			if err == redis.Nil {
				continue // Skip if not found
			}
			return nil, fmt.Errorf("failed to get transaction data for key %s: %w", key, err)
		}

		// Decode transaction
		var tx types.Transaction
		if err := rlp.DecodeBytes(data, &tx); err != nil {
			r.logger.Warn("Failed to decode transaction", "key", key, "error", err)
			continue
		}

		txs = append(txs, tx)
	}

	return txs, nil
}

// BalanceAt returns the account balance at the given address and block number
func (r *RedisStateProvider) BalanceAt(ctx context.Context, address libcommon.Address, blockNumber rpc.BlockNumber) (*big.Int, error) {
	// Convert block number to uint64
	var blockNum uint64

	switch blockNumber {
	case rpc.LatestBlockNumber, rpc.PendingBlockNumber:
		latest, err := r.GetLatestBlockNumber(ctx)
		if err != nil {
			return nil, err
		}
		blockNum = latest
	case rpc.EarliestBlockNumber:
		blockNum = 0
	default:
		blockNum = uint64(blockNumber)
	}

	// Use Redis reader to get the account data
	reader := NewPointInTimeRedisStateReader(r.redisClient, blockNum)
	defer reader.Close()

	account, err := reader.ReadAccountData(address)
	if err != nil {
		return nil, err
	}

	if account == nil {
		return big.NewInt(0), nil
	}

	return account.Balance.ToBig(), nil
}

// ==========================================================================
// Redis State Reader Implementation
// ==========================================================================

// PointInTimeRedisStateReader implements the state.StateReader interface using Redis
type PointInTimeRedisStateReader struct {
	redisClient *redis.Client
	blockNumber uint64
}

// NewPointInTimeRedisStateReader creates a new PointInTimeRedisStateReader
func NewPointInTimeRedisStateReader(redisClient *redis.Client, blockNumber uint64) *PointInTimeRedisStateReader {
	return &PointInTimeRedisStateReader{
		redisClient: redisClient,
		blockNumber: blockNumber,
	}
}

// Close implements the state.StateReader interface
func (r *PointInTimeRedisStateReader) Close() {}

// ReadAccountData retrieves account data from Redis at the specified block number
func (r *PointInTimeRedisStateReader) ReadAccountData(address libcommon.Address) (*accounts.Account, error) {
	// Construct the Redis key for the account
	accountKey := fmt.Sprintf("account:%s", address.Hex())

	// Get account data using BSSL.GETSTATEATBLOCK
	result, err := r.redisClient.Do(context.Background(), "BSSL.GETSTATEATBLOCK", accountKey, r.blockNumber).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil // Account doesn't exist at this block number
		}
		return nil, fmt.Errorf("failed to get account data: %w", err)
	}

	if result == nil {
		return nil, nil // Account doesn't exist
	}

	// Parse account JSON
	var accountJSON struct {
		Balance     string `json:"balance"`
		Nonce       uint64 `json:"nonce"`
		CodeHash    string `json:"codeHash"`
		Incarnation uint64 `json:"incarnation"`
	}

	accountData, ok := result.(string)
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}

	// Check for deletion marker
	if accountData == "{\"deleted\":true}" {
		return nil, nil
	}

	if err := json.Unmarshal([]byte(accountData), &accountJSON); err != nil {
		return nil, fmt.Errorf("failed to parse account data: %w", err)
	}

	// Convert balance string to big.Int
	balance, ok := new(big.Int).SetString(accountJSON.Balance, 10)
	if !ok {
		return nil, fmt.Errorf("invalid balance format: %s", accountJSON.Balance)
	}

	// Convert balance to uint256
	balanceU256, overflow := uint256.FromBig(balance)
	if overflow {
		return nil, fmt.Errorf("balance overflow: %s", accountJSON.Balance)
	}

	// Parse code hash
	codeHash := libcommon.HexToHash(accountJSON.CodeHash)

	// Create account object
	account := &accounts.Account{
		Nonce:       accountJSON.Nonce,
		Balance:     *balanceU256,
		CodeHash:    codeHash,
		Incarnation: accountJSON.Incarnation,
	}

	return account, nil
}

// ReadAccountStorage retrieves account storage data from Redis at the specified block number
func (r *PointInTimeRedisStateReader) ReadAccountStorage(address libcommon.Address, incarnation uint64, key *libcommon.Hash) ([]byte, error) {
	// Construct the Redis key for storage
	storageKey := fmt.Sprintf("storage:%s:%s", address.Hex(), key.Hex())

	// Get storage data using BSSL.GETSTATEATBLOCK
	result, err := r.redisClient.Do(context.Background(), "BSSL.GETSTATEATBLOCK", storageKey, r.blockNumber).Result()
	if err != nil {
		if err == redis.Nil {
			// Storage doesn't exist at this block number
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get storage data: %w", err)
	}

	if result == nil {
		// Storage doesn't exist
		return nil, nil
	}

	// Parse storage JSON
	var storageJSON struct {
		Value string `json:"value"`
	}

	storageData, ok := result.(string)
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}

	if err := json.Unmarshal([]byte(storageData), &storageJSON); err != nil {
		return nil, fmt.Errorf("failed to parse storage data: %w", err)
	}

	// Handle null value
	if storageJSON.Value == "null" {
		return nil, nil
	}

	if storageJSON.Value == "0x" || storageJSON.Value == "" {
		return nil, nil
	}

	// Convert hex string to bytes
	value := libcommon.FromHex(storageJSON.Value)
	return value, nil
}

// ReadAccountCode retrieves account code from Redis
func (r *PointInTimeRedisStateReader) ReadAccountCode(address libcommon.Address, incarnation uint64) ([]byte, error) {
	// First, we need to get the account to find the code hash
	account, err := r.ReadAccountData(address)
	if err != nil {
		return nil, err
	}

	if account == nil {
		return nil, nil // Account doesn't exist
	}

	// Check if it's an EOA (no code)
	emptyCodeHash := libcommon.Hash{}
	if account.CodeHash == emptyCodeHash {
		return nil, nil
	}

	// Get code by hash
	codeKey := fmt.Sprintf("code:%s", account.CodeHash.Hex())
	code, err := r.redisClient.Get(context.Background(), codeKey).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil, nil // Code doesn't exist
		}
		return nil, fmt.Errorf("failed to get code data: %w", err)
	}

	return code, nil
}

// ReadAccountCodeSize retrieves the size of account code from Redis
func (r *PointInTimeRedisStateReader) ReadAccountCodeSize(address libcommon.Address, incarnation uint64) (int, error) {
	code, err := r.ReadAccountCode(address, incarnation)
	if err != nil {
		return 0, err
	}
	return len(code), nil
}

// ReadAccountIncarnation is not used in our implementation
func (r *PointInTimeRedisStateReader) ReadAccountIncarnation(address libcommon.Address) (uint64, error) {
	account, err := r.ReadAccountData(address)
	if err != nil {
		return 0, err
	}
	if account == nil {
		return 0, nil
	}
	return account.Incarnation, nil
}

// Implement additional methods required by state.StateReader interface
func (r *PointInTimeRedisStateReader) ReadAccountDataForDebug(address libcommon.Address) (*accounts.Account, error) {
	return r.ReadAccountData(address)
}

// Custom types for tracing to avoid importing the tracing package
type Tracer interface{}
type AccountReadTrace struct{}
type StorageReadTrace struct{}
type CodeReadTrace struct{}

// Implement additional methods required by state.StateReader interface
func (r *PointInTimeRedisStateReader) SetTracer(_ Tracer) {}

func (r *PointInTimeRedisStateReader) TraceAccountReads() bool {
	return false
}

func (r *PointInTimeRedisStateReader) TraceStorageReads() bool {
	return false
}

func (r *PointInTimeRedisStateReader) TraceCodeReads() bool {
	return false
}

func (r *PointInTimeRedisStateReader) ReadAccountTraces() []AccountReadTrace {
	return nil
}

func (r *PointInTimeRedisStateReader) ReadStorageTraces() []StorageReadTrace {
	return nil
}

func (r *PointInTimeRedisStateReader) ReadCodeTraces() []CodeReadTrace {
	return nil
}

// GetDelegatedDesignation implements eip-7702 designation support
func (r *PointInTimeRedisStateReader) GetDelegatedDesignation(address libcommon.Address) (libcommon.Address, bool, error) {
	// First check if the account has code
	account, err := r.ReadAccountData(address)
	if err != nil {
		return libcommon.Address{}, false, err
	}

	if account == nil {
		return libcommon.Address{}, false, nil
	}

	// Get the code
	code, err := r.ReadAccountCode(address, account.Incarnation)
	if err != nil {
		return libcommon.Address{}, false, err
	}

	// Check if the code is a designation (32 bytes long)
	if len(code) == 32 {
		// Convert 32 bytes to address (first 20 bytes)
		var designatedAddress libcommon.Address
		if len(code) >= 20 {
			copy(designatedAddress[:], code[:20])
		}
		return designatedAddress, true, nil
	}

	return libcommon.Address{}, false, nil
}

// ResolveCodeHash implements the eip-7702 designation support
func (r *PointInTimeRedisStateReader) ResolveCodeHash(address libcommon.Address) (libcommon.Hash, error) {
	originalAccount, err := r.ReadAccountData(address)
	if err != nil {
		return libcommon.Hash{}, err
	}

	if originalAccount == nil {
		return libcommon.Hash{}, nil
	}

	// Check if this address has designation
	designatedAddr, isDelegated, err := r.GetDelegatedDesignation(address)
	if err != nil {
		return libcommon.Hash{}, err
	}

	if !isDelegated {
		// No delegation, return the account's code hash
		return originalAccount.CodeHash, nil
	}

	// Get the code hash from the designated address
	designatedAccount, err := r.ReadAccountData(designatedAddr)
	if err != nil {
		return libcommon.Hash{}, err
	}

	if designatedAccount == nil {
		return libcommon.Hash{}, nil
	}

	return designatedAccount.CodeHash, nil
}

// ResolveCode implements the eip-7702 designation support
func (r *PointInTimeRedisStateReader) ResolveCode(address libcommon.Address) ([]byte, error) {
	// Check if this address has designation
	designatedAddr, isDelegated, err := r.GetDelegatedDesignation(address)
	if err != nil {
		return nil, err
	}

	if !isDelegated {
		// No delegation, return the account's code
		account, err := r.ReadAccountData(address)
		if err != nil {
			return nil, err
		}

		if account == nil {
			return nil, nil
		}

		return r.ReadAccountCode(address, account.Incarnation)
	}

	// Get the code from the designated address
	designatedAccount, err := r.ReadAccountData(designatedAddr)
	if err != nil {
		return nil, err
	}

	if designatedAccount == nil {
		return nil, nil
	}

	return r.ReadAccountCode(designatedAddr, designatedAccount.Incarnation)
}

// ==========================================================================
// API Implementations
// ==========================================================================

// Create API implementations
func createEthAPI(stateProvider *RedisStateProvider, logger log.Logger) interface{} {
	return &EthAPI{stateProvider: stateProvider, logger: logger}
}

func createDebugAPI(stateProvider *RedisStateProvider, logger log.Logger) interface{} {
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

type DebugAPI struct {
	stateProvider *RedisStateProvider
	logger        log.Logger
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
		hashData, err := api.stateProvider.GetRedisClient().Get(ctx, hashKey).Bytes()
		if err != nil {
			return "", fmt.Errorf("block hash not found: %s", blockHash.Hex())
		}

		// Parse the JSON data to get the block number
		var blockHashJSON BlockHashJSON
		if err := json.Unmarshal(hashData, &blockHashJSON); err != nil {
			return "", fmt.Errorf("failed to parse block hash data: %w", err)
		}

		blockNum = blockHashJSON.Number
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
			blockNrOrHash = strings.TrimPrefix(blockNrOrHash, "0x")

			blockNum, err = strconv.ParseUint(blockNrOrHash, 16, 64)
			if err != nil {
				return "", fmt.Errorf("invalid block number: %s", blockNrOrHash)
			}
		}
	}

	// Create a state reader for the specific block
	reader := NewPointInTimeRedisStateReader(api.stateProvider.GetRedisClient(), blockNum)
	defer reader.Close()

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
