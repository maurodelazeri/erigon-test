# Redis State Integration for Erigon - Complete Implementation Guide

This document provides a comprehensive guide to the Redis state integration in Erigon. It details what data is stored, how it's structured, where the integration points are, and how to implement and verify the integration.

## Overview

The Redis state integration allows Erigon to write all blockchain data to Redis in parallel with its normal database operations. This provides:

1. **O(1) Access to Historical State**: Access any account state at any block height with constant time complexity
2. **Complete Blockchain Data**: Store accounts, storage, code, transactions, receipts, logs, and blocks
3. **Real-time Updates**: Data is written to Redis as blocks are processed
4. **Chain Reorganization Handling**: Proper tracking of canonical chain during reorgs

## Data Storage Model

All blockchain data is stored in Redis using these key patterns:

### Account Data

- `account:{address}` → Sorted set with block numbers as scores
  - Example: `account:0x742d35Cc6634C0532925a3b844Bc454e4438f44e`
  - Contains: JSON with `nonce`, `balance`, `codeHash`, `incarnation`

### Contract Code

- `code:{codeHash}` → Raw bytecode
  - Example: `code:0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef`
  - Contains: Raw contract bytecode (immutable)

### Storage Slots

- `storage:{address}:{key}` → Sorted set with block numbers as scores
  - Example: `storage:0x742d35Cc6634C0532925a3b844Bc454e4438f44e:0x0000000000000000000000000000000000000000000000000000000000000001`
  - Contains: Raw storage slot values

### Block Data

- `block:{number}` → Hash map with block details
  - Example: `block:15000000`
  - Contains: Header, timestamp, state root, hash, parent hash
- `blockHash:{hash}` → Hash map mapping hash to block number
  - Example: `blockHash:0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef`
  - Contains: Block number, timestamp, canonical status
- `canonicalChain` → Sorted set of block hashes with block numbers as scores
  - Contains: All canonical block hashes in order

### Transaction Data

- `tx:{hash}` → Sorted set with block numbers as scores
  - Example: `tx:0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef`
  - Contains: Serialized transaction data
- `block:{number}:txs` → Set of transaction hashes in this block
  - Example: `block:15000000:txs`
- `sender:{address}:txs` → Sorted set of transaction hashes sent by address
  - Example: `sender:0x742d35Cc6634C0532925a3b844Bc454e4438f44e:txs`

### Receipt Data

- `receipt:{hash}` → Sorted set with block numbers as scores
  - Example: `receipt:0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef`
  - Contains: Serialized receipt data (status, logs, gas used)
- `block:{number}:receipts` → Sorted set of receipts in block order
  - Example: `block:15000000:receipts`

### Event Logs

- `log:{index}` → Raw log data
  - Example: `log:1234567`
  - Contains: JSON with address, topics, data, block, tx info
- `topic:{hash}` → Sorted set of log indices with block numbers as scores
  - Example: `topic:0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef`
- `address:{address}:logs` → Sorted set of log indices with block numbers as scores
  - Example: `address:0x742d35Cc6634C0532925a3b844Bc454e4438f44e:logs`

### Chain Management

- `currentBlock` → Latest block number processed
- `reorg:{blockNum}` → Markers for blocks that were reorged out

## Implementation Architecture

The Redis integration is structured with these key components:

1. **RedisStateReader**: Reads state from Redis at specific block heights
2. **RedisStateWriter**: Writes account and storage state to Redis
3. **RedisHistoricalWriter**: Adds block and history tracking capabilities
4. **Factory Pattern**: Creates appropriate writers per block

## Integration Points

The Redis integration hooks into Erigon's execution flow at these critical points:

### 1. Initialization

**File**: `eth/backend.go`

```go
// Initialize Redis if enabled
if stack.Config().Redis.Enabled {
    logger.Info("Initializing Redis state integration", "url", stack.Config().Redis.URL)
    if err := redisstate.InitializeRedisClient(context.Background(), stack.Config().Redis.URL, stack.Config().Redis.Password, logger); err != nil {
        logger.Warn("Failed to initialize Redis client", "err", err)
    } else {
        // Register the Redis writer factory with core state
        writerFactoryFn := redisstate.GetRedisWriterFactoryFn()
        state.SetRedisWriterFactory(writerFactoryFn)

        // Verify Redis connection
        diagnostics := redisstate.DiagnoseRedisConnection()
        logger.Info("Redis integration initialized", "status", diagnostics["status"])
    }
}
```

### 2. Block Context Setup

**File**: `cmd/state/exec3/state.go`

```go
func (rw *Worker) RunTxTaskNoLock(txTask *state.TxTask, isMining bool) {
    // Set the block context for Redis integration
    if state.IsRedisEnabled() && txTask.BlockNum > 0 {
        rw.ibs.SetBlockContext(txTask.BlockNum)
    }

    // Rest of transaction processing...
}
```

### 3. Block Initialization

**File**: `eth/stagedsync/exec3_serial.go`

```go
func (se *serialExecutor) execute(ctx context.Context, tasks []*state.TxTask) (cont bool, err error) {
    for _, txTask := range tasks {
        // Initialize Redis state for this block if Redis is enabled
        if state.IsRedisEnabled() && txTask.BlockNum > 0 {
            blockNum := txTask.BlockNum
            redisWriter := state.GetRedisStateWriter(blockNum)
            if redisWriter != nil {
                if redisHistoryWriter, ok := redisWriter.(state.RedisHistoricalWriter); ok {
                    redisHistoryWriter.WriteBlockStart(blockNum)
                }
            }
        }

        // Execute transaction...
    }
}
```

### 4. State Changes

**File**: `core/state/intra_block_state.go`

```go
// CommitBlock finalizes the state
func (sdb *IntraBlockState) CommitBlock(chainRules *chain.Rules, stateWriter StateWriter) error {
    // Regular state commit
    err := sdb.MakeWriteSet(chainRules, stateWriter)

    // Redis state commit
    if IsRedisEnabled() && sdb.currentBlock > 0 {
        if redisWriter := GetRedisStateWriter(sdb.currentBlock); redisWriter != nil {
            sdb.MakeWriteSet(chainRules, redisWriter)
        }
    }

    return nil
}
```

### 5. Transaction Processing

**File**: `core/state_processor.go`

```go
func applyTransaction(config *chain.Config, engine consensus.EngineReader, gp *GasPool, ibs *state.IntraBlockState,
    stateWriter state.StateWriter, header *types.Header, txn types.Transaction, usedGas, usedBlobGas *uint64,
    evm *vm.EVM, cfg vm.Config) (*types.Receipt, []byte, error) {

    // Transaction execution...

    // Add Redis transaction and receipt handling
    if state.IsRedisEnabled() && receipt != nil {
        if redisWriter := state.GetRedisStateWriter(header.Number.Uint64()); redisWriter != nil {
            redisWriter.SetTxNum(uint64(receipt.TransactionIndex))

            if redisHistoryWriter, ok := redisWriter.(state.RedisHistoricalWriter); ok {
                redisHistoryWriter.HandleTransaction(txn, receipt, header.Number.Uint64(), uint64(receipt.TransactionIndex))
            }
        }
    }

    return receipt, result.ReturnData, err
}
```

### 6. Block Finalization

**File**: `eth/stagedsync/exec3_serial.go`

```go
func (se *serialExecutor) commit(ctx context.Context, txNum uint64, blockNum uint64, useExternalTx bool) (t2 time.Duration, err error) {
    // Finalize Redis writes for this block
    if state.IsRedisEnabled() && blockNum > 0 {
        if redisWriter := state.GetRedisStateWriter(blockNum); redisWriter != nil {
            if redisHistoryWriter, ok := redisWriter.(state.RedisHistoricalWriter); ok {
                // Store block header and state root
                if header := se.getHeader(ctx, libcommon.Hash{}, blockNum); header != nil {
                    redisHistoryWriter.StoreBlockInfo(header, header.Root)
                }

                // Finalize block in Redis
                redisHistoryWriter.WriteChangeSets()
                redisHistoryWriter.WriteHistory()
            }
        }
    }

    // Regular commit operations...
}
```

### 7. Chain Reorganization

**File**: `eth/stagedsync/stage_execute.go`

```go
func unwindExec3(u *UnwindState, s *StageState, txc wrap.TxContainer, ctx context.Context, br services.FullBlockReader, accumulator *shards.Accumulator, logger log.Logger) (err error) {
    // Track blocks to unwind in Redis
    if state.IsRedisEnabled() {
        blocksToUnwind := make([]uint64, 0, u.CurrentBlockNumber-u.UnwindPoint)

        // Track blocks being unwound
        for currentBlock := u.CurrentBlockNumber; currentBlock > u.UnwindPoint; currentBlock-- {
            blocksToUnwind = append(blocksToUnwind, currentBlock)
        }

        // Handle Redis unwind
        if err := redisstate.HandleChainReorg(u.CurrentBlockNumber, u.UnwindPoint, logger); err != nil {
            logger.Warn("Failed to handle Redis chain reorg", "err", err)
        }
    }

    // Regular unwind operations...
}
```

## Implementation Methods

The core Redis functionality is implemented in these files:

### redis_state.go

Contains the implementation of Redis state readers and writers:

1. **Account State Methods**:

   - `ReadAccountData` - Reads account state
   - `UpdateAccountData` - Writes account state
   - `DeleteAccount` - Handles account deletion

2. **Storage Methods**:

   - `ReadAccountStorage` - Reads contract storage
   - `WriteAccountStorage` - Writes contract storage

3. **Code Methods**:

   - `ReadAccountCode` - Reads contract code
   - `UpdateAccountCode` - Stores contract code

4. **Transaction Methods**:

   - `HandleTransaction` - Stores transaction data
   - `StoreReceipt` - Stores transaction receipts and logs

5. **Block Methods**:
   - `WriteBlockStart` - Initializes block data
   - `StoreBlockInfo` - Stores block header and state
   - `WriteChangeSets` - Updates canonical chain
   - `WriteHistory` - Handles special cases like reorgs

### factory.go

Manages Redis client and writer lifecycle:

1. **Initialization**:

   - `InitializeRedisClient` - Sets up Redis connection
   - `DiagnoseRedisConnection` - Verifies Redis health

2. **Writer Factory**:

   - `CreateRedisWriter` - Creates appropriate writer for block
   - `GetRedisWriterFactoryFn` - Factory function registration

3. **Reorg Handling**:
   - `HandleChainReorg` - Updates Redis during chain reorgs

### state_provider.go

Implements provider interfaces for RPC access:

1. **Account Queries**:

   - `BalanceAt` - Gets account balance
   - `NonceAt` - Gets account nonce
   - `CodeAt` - Gets contract code

2. **Transaction Queries**:

   - `GetTransactionByHash` - Retrieves transactions
   - `GetTransactionReceipt` - Retrieves receipts

3. **Log Queries**:
   - `GetLogs` - Filters logs by criteria

## Configuration and Usage

To enable Redis integration, add these flags to your Erigon command:

```bash
erigon --redis.enabled=true --redis.url="redis://localhost:6379/0" --redis.password="password" [other flags]
```

## Verification

To verify the Redis integration is working:

1. **Run the Redis Check Tool**:

```bash
go build -o build/bin/redis-check ./cmd/redis-check
build/bin/redis-check --url="redis://localhost:6379/0" --password="password" --write-test --check-keys
```

2. **Check Redis Data**:

```bash
redis-cli -a password

# Check for keys
> KEYS *

# Check account data
> ZRANGE account:0x742d35Cc6634C0532925a3b844Bc454e4438f44e 0 -1 WITHSCORES

# Check block data
> HGETALL block:15000000
```

3. **Monitor Redis During Sync**:

```bash
redis-cli -a password MONITOR
```

## Troubleshooting

1. **No Data in Redis**:

   - Verify Redis connection is successful in logs
   - Check if Redis is enabled with the correct flags
   - Ensure Erigon is processing new blocks (not idle)

2. **Missing Data Types**:

   - If accounts appear but no transactions, check `HandleTransaction` integration
   - If blocks appear but no state, check `CommitBlock` integration

3. **Performance Issues**:
   - Enable pipelining for better performance
   - Consider Redis cluster for large-scale deployments
   - Implement pruning for historical data

## Extending the Integration

To add new data types to Redis:

1. Add new key patterns in `redis_state.go`
2. Implement corresponding read/write methods
3. Hook into appropriate execution points
4. Update the provider interface for RPC access
