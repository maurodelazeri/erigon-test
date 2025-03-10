# Redis State Integration for Erigon - Developer Documentation

## Overview

The Redis state integration for Erigon enables storing blockchain state, transactions, receipts, and blocks in Redis alongside Erigon's main database. This provides fast O(1) access to historical state at any block height, useful for API services, debug tools, and transaction simulations.

## Component Architecture

Our implementation consists of two main components:

1. **RedisState** (`redis_state.go`) - Core Redis client and state management
2. **RedisStateMonitor** (`redis_state_v3.go`) - Monitor for capturing block, transaction, and receipt data

The implementation uses a shadowing approach that captures state changes directly in state writing methods rather than using wrapper classes. This provides better integration with minimal modifications to the core codebase.

## File Descriptions

### 1. `redis_state.go`

**Purpose:** Provides the core Redis client functionality and methods to store and retrieve blockchain data.

**Key Components:**

- **RedisState** - Main struct that manages the Redis connection and pipeline
- **Data Writing Methods** - Functions to write accounts, storage, code, blocks, transactions, and receipts
- **Reorg Handling** - Complete removal of non-canonical chain data for O(1) lookups
- **Query Methods** - Functions to retrieve historical state at any block height

**How It Works:**

1. The Redis client is initialized once with the provided URL
2. State changes are batched using Redis pipelines for efficiency
3. During reorgs, data from non-canonical blocks is completely deleted
4. Historical state queries use Redis sorted sets with block numbers as scores
5. All data access is O(1) complexity since only canonical data is kept

**Key Data Structures:**

- Account data stored as sorted sets with block numbers as scores
- Storage slots stored as sorted sets with block numbers as scores
- Code stored by hash (immutable)
- Blocks indexed by block number
- Transactions and receipts indexed by block number instead of hash

### 2. `redis_state_v3.go`

**Purpose:** Provides monitoring capabilities to capture block, transaction, and receipt data during execution.

**Key Components:**

- **RedisStateMonitor** - Monitors state changes for Redis integration
- **Monitoring Methods** - Functions to record block, transaction, and receipt data
- **Chain Reorganization Handler** - Special handling for chain reorganizations

**How It Works:**

1. Block data is captured in the execution flow during block processing
2. Transaction data is captured when transactions are executed
3. Receipt data is captured after transactions are processed
4. Chain reorganizations are properly handled to maintain data consistency

### 3. State Writing Integration

**Purpose:** Direct integration with Erigon's state writing methods to capture state changes.

**Key Components:**

- **Direct Integration** - State writing methods (UpdateAccountData, etc.) directly write to Redis when enabled
- **Block Number Tracking** - Block number is used as the primary index for historical state
- **Zero Hash Handling** - A zero hash is used in state writers since actual block hash isn't available

**How It Works:**

1. State writing methods check if Redis is enabled
2. If enabled, they write state changes directly to Redis while performing normal operations
3. The Redis pipeline is explicitly flushed when a block is committed in `flushAndCheckCommitmentV3`, ensuring Redis is always in sync with the client's state
4. This approach minimizes code changes while ensuring all state changes are correctly captured

## Data Organization

This implementation stores data in Redis using the following key patterns:

1. **Blocks**
   - `block:{blockNum}` - Hash containing block metadata
   - `blockHash:{blockHash}` - Hash mapping block hash to block number and timestamp
   - `canonicalChain` - Sorted set of block hashes with block numbers as scores

2. **Transactions**
   - `txs:{blockNum}` - Hash where keys are tx indices and values are RLP-encoded transactions
   - `block:{blockNum}:txs` - Set of transaction hashes in the block
   - `tx:{txHash}:block` - Transaction hash to block number mapping
   - `sender:{address}:txs` - Sorted set of transactions sent by an address with block numbers as scores

3. **Receipts**
   - `receipts:{blockNum}` - Hash where keys are tx indices and values are RLP-encoded receipts
   - `block:{blockNum}:receipts` - Sorted set of transaction hashes with indices as scores
   - `receipt:{txHash}:block` - Transaction hash to block number mapping

4. **Accounts**
   - `account:{address}` - Sorted set with block numbers as scores and account data as values
   - Account data is stored as JSON with balance, nonce, codeHash, and incarnation fields

5. **Storage**
   - `storage:{address}:{slot}` - Sorted set with block numbers as scores and storage values

6. **Code**
   - `code:{codeHash}` - Code bytes stored by hash (immutable)

## Data Flow and Interaction

1. **Block Processing:**
   - When a block is processed, RedisStateMonitor captures block header and metadata
   - For each transaction, transaction data is recorded by the monitor
   - After execution, receipt data is also captured by the monitor

2. **State Modifications:**
   - State changes are captured directly in the state writing methods
   - When these methods are called, they check if Redis is enabled and write to it if so
   - Account updates, storage changes, and code deployment are all tracked with block numbers
   - A zero block hash is used since the state writers don't have access to the actual block hash

3. **Chain Reorganizations:**
   - When a reorg occurs, RedisStateMonitor.MonitorUnwind is called
   - The handleReorg method completely removes all data from the non-canonical chain
   - Only canonical chain data remains in Redis, ensuring O(1) access times

4. **Data Retrieval:**
   - Historical state can be queried at any block height using GetAccountAtBlock and GetStorageAtBlock
   - Since only canonical data is stored, all queries are O(1) complexity
   - Sorted sets with block numbers as scores enable efficient historical access

## Query Patterns

### Getting Block Data
```
HGETALL block:{blockNum}
```

### Getting Transactions from a Block
```
HGETALL txs:{blockNum}
```

### Getting Receipts from a Block
```
HGETALL receipts:{blockNum}
```

### Getting Transaction Hash to Block Mapping
```
GET tx:{txHash}:block
```

### Getting Receipt Hash to Block Mapping
```
GET receipt:{txHash}:block
```

### Getting Account State at a Block
```
ZREVRANGEBYSCORE account:{address} {blockNum} 0 LIMIT 0 1 WITHSCORES
```

### Getting Storage Value at a Block
```
ZREVRANGEBYSCORE storage:{address}:{slot} {blockNum} 0 LIMIT 0 1 WITHSCORES
```

### Getting Transactions by Sender
```
ZRANGE sender:{address}:txs {startIndex} {endIndex} WITHSCORES
```

### Getting Contract Code
```
GET code:{codeHash}
```

## Design Decisions

1. **Block-Indexed Storage:**
   - Transactions and receipts are now indexed primarily by block number
   - This makes it easier to retrieve all data for a block in one operation
   - Cross-reference mappings maintain the ability to look up by transaction hash

2. **Complete Deletion vs. Marking:**
   - Non-canonical data is completely deleted rather than marked as non-canonical
   - This simplifies queries to true O(1) operations without canonicality checks
   - Trade-off: Higher write overhead during reorgs for faster reads

3. **No Log Storage:**
   - The implementation no longer stores logs in Redis
   - This reduces storage requirements and complexity
   - Applications that need log data can retrieve it from receipts

4. **Optimized Batch Operations:**
   - Redis pipelines batch operations for efficiency
   - Lua scripts handle bulk operations during reorgs
   - Periodic flushing balances performance and durability

5. **Direct State Integration:**
   - State writing methods directly check and write to Redis rather than using wrapper classes
   - This minimizes changes to Erigon's core code and improves maintainability
   - The approach is less intrusive while still ensuring all state changes are captured

## Usage Scenarios

1. **Fast Historical State Access:**
   - Get account state at any block height with O(1) complexity
   - Useful for API services that need quick access to past state

2. **Transaction Simulation:**
   - Efficiently simulate transactions at arbitrary block heights
   - Useful for debug tools and transaction analysis

3. **Block and Transaction Lookup:**
   - Efficient access to block data, transactions, and receipts by block number
   - Useful for block explorers and transaction tracking services

4. **Account History Analysis:**
   - Track historical changes to account state
   - Useful for analytics and debugging

--

# Erigon's Extension Points for Redis Integration

Erigon has a well-designed architecture that allows for integrating our Redis state functionality through key extension points:

## Core Extension Points

### 1. **Interface-based Design**

Erigon uses interfaces for state management components:

```go
// StateWriter interface defines how state changes are written
type StateWriter interface {
    UpdateAccountData(address libcommon.Address, original, account *accounts.Account) error
    // other methods...
}
```

### 2. **Direct Method Integration**

State changes are captured directly in the state writing methods:

```go
// In StateWriterV3.UpdateAccountData
func (w *StateWriterV3) UpdateAccountData(address common.Address, original, account *accounts.Account) error {
    // Standard operations
    
    // Redis integration - capture state changes directly
    redisState := GetRedisState()
    if redisState.Enabled() && w.rs.domains != nil {
        redisState.writeAccount(w.rs.domains.BlockNum(), w.rs.domains.BlockHash(), address, account)
    }
    
    // Regular state writing continues...
}
```

## Specific Hook Points

### 1. **State Modification Hooks**

- `StateWriterV3` methods directly check for Redis and write to it when enabled
- This avoids the need for wrapper classes while ensuring all state changes are captured

### 2. **Block/Transaction Monitoring**

- `RedisStateMonitor` is initialized in the execution flow
- It captures block data, transaction data, and receipts at key points

### 3. **Chain Reorganization Hooks**

- `RedisStateMonitor.MonitorUnwind` is called during chain reorganizations
- This ensures proper Redis state cleanup for non-canonical blocks

## Integration Mechanism

The process of integrating Redis with Erigon is minimal and non-intrusive:

1. **Initial Configuration**

   - Command-line flags define Redis URL and password
   - Redis state is initialized when Erigon starts

2. **Direct Integration**

   - State writing methods directly check if Redis is enabled
   - If enabled, they write to Redis in addition to their normal operations
   - This approach requires minimal changes to the core codebase

3. **Monitoring Integration**
   - Block, transaction, and receipt data are captured via monitoring
   - This ensures all blockchain data is properly shadowed to Redis

This direct integration approach allows our Redis functionality to work as a non-intrusive layer on top of Erigon's core functionality while capturing all state changes.

## Implementation Details

### Modified Files

The following files were modified to implement the Redis state integration:

1. **`core/state/redis_state.go`**
   - Removed log storage functionality
   - Modified transaction storage to index by block number using `txs:{blockNum}` keys
   - Modified receipt storage to index by block number using `receipts:{blockNum}` keys
   - Simplified transaction and receipt hash-to-block mapping
   - Updated reorganization handling to properly clean up block-indexed data

2. **`core/state/redis_state_v3.go`**
   - No significant changes (monitor functionality remains the same)

3. **`core/state/redis_writer.go`**
   - No significant changes (writer functionality remains the same)

4. **`core/state/rw_v3.go`**
   - Added integration points for Redis state updates in state writer methods
   - Modified to send account, storage, and code changes to Redis

5. **`eth/stagedsync/exec3.go`**
   - Added Redis pipeline flushing during block commit
   - Ensures all buffered Redis operations are committed after block execution

6. **`eth/stagedsync/exec3_serial.go`**
   - Added Redis flush operations for serial execution

7. **`eth/backend.go`**
   - Added Redis initialization from config params
   - Ensures Redis connection is established at startup

8. **`cmd/utils/flags.go`**
   - Added Redis URL and password flags 
   - Added flag handling in SetEthConfig

9. **`eth/ethconfig/config.go`**
   - Added Redis URL and password config fields
   - Ensures Redis connection params are passed through the system

10. **`turbo/cli/default_flags.go`**
   - Added Redis URL and password flags to the default flags list
   - Ensures Redis options are available in the CLI interface

### Key Changes

1. **Transaction Storage**
   - Before: Transactions were stored as `tx:{txHash}` with metadata in `tx:{txHash}:meta`
   - After: Transactions are stored as entries in `txs:{blockNum}` hash with `tx:{txHash}:block` mapping

2. **Receipt Storage**
   - Before: Receipts were stored as `receipt:{txHash}` with metadata in `receipt:{txHash}:meta`
   - After: Receipts are stored as entries in `receipts:{blockNum}` hash with `receipt:{txHash}:block` mapping

3. **Log Storage**
   - Before: Logs were stored separately with their own indices
   - After: Logs are not stored separately (they can be retrieved from receipts if needed)

4. **Data Access**
   - All historical queries remain O(1) complexity
   - Block-based queries are now more efficient
   - Reduced storage requirements by eliminating separate log storage
