# Redis State Integration for Erigon - Developer Documentation

## Overview

The Redis state integration for Erigon enables storing blockchain state, transactions, receipts, and blocks in Redis alongside Erigon's main database. This provides fast O(1) access to historical state at any block height regardless of history length, useful for API services, debug tools, and transaction simulations.

## Component Architecture

Our implementation consists of two main components:

1. **RedisState** (`redis_state.go`) - Core Redis client and state management using the BSSL module
2. **RedisStateMonitor** (`redis_state_v3.go`) - Monitor for capturing block, transaction, and receipt data

The implementation uses a shadowing approach that captures state changes directly in state writing methods rather than using wrapper classes. This provides better integration with minimal modifications to the core codebase.

## File Descriptions

### 1. `redis_state.go`

**Purpose:** Provides the core Redis client functionality and methods to store and retrieve blockchain data using the BSSL module.

**Key Components:**

- **RedisState** - Main struct that manages the Redis connection and pipeline
- **Data Writing Methods** - Functions to write accounts, storage, code, blocks, transactions, and receipts
- **Reorg Handling** - Complete removal of non-canonical chain data with proper state restoration
- **Query Methods** - Functions to retrieve historical state at any block height

**How It Works:**

1. The Redis client is initialized with the provided URL and checks for BSSL module availability
2. State changes are batched using Redis pipelines for efficiency
3. During reorgs, data from non-canonical blocks is completely deleted
4. Historical state queries use the BSSL module for O(1) performance
5. All data access is O(1) complexity regardless of chain length

**Key Data Structures:**

- Account data stored via BSSL with block numbers as indexes
- Storage slots stored via BSSL with block numbers as indexes
- Code stored by hash (immutable)
- Blocks indexed by block number
- Transactions and receipts indexed by block number

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

### 3. `redis_writer.go`

**Purpose:** Provides a StateWriter wrapper for Redis integration.

**Key Components:**

- **StateWriterWithRedis** - Wraps a StateWriter and adds Redis operations
- **Integration Methods** - Methods that both update Redis and call the underlying writer methods

**How It Works:**

1. Each state modification method is implemented to write to both Redis and the underlying storage
2. Block number and hash are tracked to associate state changes with specific blocks
3. Uses composition to minimize changes to the core code while capturing all state changes

## Data Organization

This implementation stores data in Redis using the following key patterns:

1. **Blocks**

   - `block:{blockNum}` - Hash containing block metadata
   - `blockHash:{blockHash}` - Hash mapping block hash to block number and timestamp

2. **Transactions**

   - `txs:{blockNum}` - Hash where keys are tx indices and values are RLP-encoded transactions
   - `block:{blockNum}:txs` - Set of transaction hashes in the block

3. **Receipts**

   - `receipts:{blockNum}` - Hash where keys are tx indices and values are RLP-encoded receipts

4. **Accounts**

   - `account:{address}` - BSSL data structure with block numbers as indexes and account data as values
   - Account data is stored as JSON with balance, nonce, codeHash, and incarnation fields

5. **Storage**

   - `storage:{address}:{slot}` - BSSL data structure with block numbers as indexes and storage values

6. **Code**
   - `code:{codeHash}` - Code bytes stored by hash (immutable)

## The BSSL Module

This implementation specifically utilizes the **Blockchain State Skip List (BSSL)** Redis module, which provides:

- O(1) queries for state at any historical block height
- Minimal memory overhead by only storing changes
- Full compatibility with the Redis protocol
- Scalability to billions of state changes without performance degradation

### Key BSSL Commands Used

1. **BSSL.SET** - Sets state at a specific block number:

   ```
   BSSL.SET address block_num state
   ```

2. **BSSL.GETSTATEATBLOCK** - Gets state at or before a specific block:

   ```
   BSSL.GETSTATEATBLOCK address block_num
   ```

3. **BSSL.INFO** - Gets metadata about an address's state history:
   ```
   BSSL.INFO address
   ```

## Data Flow and Interaction

1. **Block Processing:**

   - When a block is processed, RedisStateMonitor captures block header and metadata
   - For each transaction, transaction data is recorded by the monitor
   - After execution, receipt data is also captured by the monitor

2. **State Modifications:**

   - State changes are captured directly in the state writing methods
   - Account updates, storage changes, and code deployment are tracked with block numbers
   - All state changes use BSSL.SET for efficient O(1) historical access

3. **Chain Reorganizations:**

   - When a reorg occurs, RedisStateMonitor.MonitorUnwind is called
   - The handleReorg method removes block data from the non-canonical chain
   - For each account and storage slot affected, the state at the block before reorg is restored at the reorg point
   - This ensures continuity of state across reorganizations

4. **Data Retrieval:**
   - Historical state is queried using BSSL.GETSTATEATBLOCK for O(1) access
   - Block data, transactions, and receipts are retrieved using standard Redis commands
   - All queries benefit from constant-time performance regardless of history length

## Query Patterns

### Getting Block Data

```
HGETALL block:{blockNum}
```

### Getting Transactions from a Block

```
HGETALL txs:{blockNum}
```

### Getting Transaction Hashes from a Block

```
SMEMBERS block:{blockNum}:txs
```

### Getting Receipts from a Block

```
HGETALL receipts:{blockNum}
```

### Getting Account State at a Block

```
BSSL.GETSTATEATBLOCK account:{address} {blockNum}
```

### Getting Storage Value at a Block

```
BSSL.GETSTATEATBLOCK storage:{address}:{slot} {blockNum}
```

### Getting Contract Code

```
GET code:{codeHash}
```

## Improved Design Features

1. **Pure BSSL Approach:**

   - The implementation now exclusively uses the BSSL module for historical state access
   - This ensures O(1) performance regardless of history length
   - Legacy sorted set approach has been completely removed

2. **Robust Reorg Handling:**

   - Chain reorganizations are handled with batch processing for efficiency
   - Pre-reorg state is properly restored at the reorg point
   - Timeout contexts prevent operations from hanging during large reorgs

3. **Transactional Consistency:**

   - Pipeline batching ensures atomic operations
   - Proper error handling prevents data corruption
   - State transitions are properly tracked even during reorgs

4. **Enhanced Query API:**

   - Comprehensive query functions for all data types
   - Consistent error handling and timeout contexts
   - Support for both low-level and high-level queries

5. **Memory Efficiency:**
   - BSSL's skip list architecture minimizes memory usage
   - Only state changes are stored, not state at every block
   - Immutable data like code is stored only once

## Performance Characteristics

The BSSL-based implementation maintains consistent performance regardless of:

1. **Chain Length** - Performance is independent of the total number of blocks
2. **State Size** - O(1) lookups regardless of how many accounts exist
3. **Change Frequency** - Optimized for both frequently and rarely changing data

Key metrics:

- **State Lookup**: O(1) constant time
- **State Writing**: O(1) constant time per state change
- **Reorg Processing**: O(n) where n is the number of affected state entries
- **Memory Usage**: Proportional to state changes, not chain length

## Integration Mechanism

The RedisState integration with Erigon is designed to be minimal and non-intrusive:

1. **Initialization** - Redis connection is established at startup based on config parameters
2. **State Capture** - State changes are captured directly in state writing methods
3. **Monitoring** - Block, transaction, and receipt data are captured via the monitor component
4. **Pipeline Flushing** - The Redis pipeline is flushed at strategic points to ensure persistence

This approach requires minimal changes to Erigon's core code while ensuring comprehensive state tracking.

## Usage Examples

### Querying Account State at a Historical Block

```go
// Get account state at block 1,000,000
accountState, err := redisState.GetAccountAtBlock(
    common.HexToAddress("0x1234..."),
    1000000,
)
fmt.Printf("Balance at block 1M: %s\n", accountState.Balance)
```

### Retrieving Storage at a Historical Block

```go
// Get storage value at block 1,000,000
storageValue, err := redisState.GetStorageAtBlock(
    common.HexToAddress("0x1234..."),
    common.HexToHash("0xabcd..."),
    1000000,
)
fmt.Printf("Storage value at block 1M: %s\n", storageValue)
```

### Getting Block Data

```go
// Get block information
blockData, err := redisState.GetBlockByNumber(1000000)
fmt.Printf("Block hash: %s\n", blockData["hash"])
fmt.Printf("Parent hash: %s\n", blockData["parentHash"])
```

### Retrieving Transactions for a Block

```go
// Get all transactions in a block
txData, err := redisState.GetTransactionsByBlockNumber(1000000)
for idx, txRLP := range txData {
    fmt.Printf("Transaction %s: %x...\n", idx, txRLP[:20])
}
```

## Error Handling and Recovery

The implementation includes robust error handling:

1. **Timeout Contexts** - All Redis operations have appropriate timeouts
2. **Batch Processing** - Large operations are processed in manageable batches
3. **Error Tracking** - Errors are logged with detailed context information
4. **Pipeline Recovery** - New pipelines are created after errors to prevent cascading failures
5. **Graceful Degradation** - Core functionality continues even if Redis operations fail
