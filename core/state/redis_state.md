# Redis State Integration for Erigon - Developer Documentation

## Overview

The Redis state integration for Erigon enables storing blockchain state, transactions, receipts, logs, and blocks in Redis alongside Erigon's main database. This provides fast O(1) access to historical state at any block height, useful for API services, debug tools, and transaction simulations.

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
- **Data Writing Methods** - Functions to write accounts, storage, code, blocks, transactions, receipts, and logs
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
- Blocks, transactions, and receipts indexed by hash and block number
- Logs indexed by address and topic for efficient querying

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

## Design Decisions

1. **Complete Deletion vs. Marking:**

   - Non-canonical data is completely deleted rather than marked as non-canonical
   - This simplifies queries to true O(1) operations without canonicality checks
   - Trade-off: Higher write overhead during reorgs for faster reads

2. **Block Hash Tracking:**

   - Each state entry is associated with its block hash and number
   - This enables proper handling of chain reorganizations
   - Block-specific indices allow quick access to all modified state in a block

3. **Optimized Batch Operations:**

   - Redis pipelines batch operations for efficiency
   - Lua scripts handle bulk operations during reorgs
   - Periodic flushing balances performance and durability

4. **Direct State Integration:**
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

3. **Log and Event Indexing:**

   - Quickly find logs by address or topic at any block height
   - Useful for dApp backends and analytics services

4. **Block and Transaction Lookup:**
   - Efficient access to block data, transactions, and receipts
   - Useful for block explorers and transaction tracking services

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
