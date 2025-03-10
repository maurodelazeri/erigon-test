# Redis State Integration for Erigon - Developer Documentation

## Overview

The Redis state integration for Erigon enables storing blockchain state, transactions, receipts, logs, and blocks in Redis alongside Erigon's main database. This provides fast O(1) access to historical state at any block height, useful for API services, debug tools, and transaction simulations.

## Component Architecture

Our implementation consists of three main components:

1. **RedisState** (`redis_state.go`) - Core Redis client and state management
2. **StateWriterWithRedis** (`redis_writer.go`) - Redis-enabled wrapper for state modifications
3. **RedisStateV3** (`redis_state_v3.go`) - Redis-enabled StateV3 for transaction processing

The components work together to capture state changes during block processing and handle chain reorganizations.

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

### 2. `redis_writer.go`

**Purpose:** Wraps the standard Erigon StateWriter to capture state changes and store them in Redis.

**Key Components:**

- **StateWriterWithRedis** - Wrapper that decorates a standard StateWriter
- **State Change Methods** - Override methods to intercept state modifications

**How It Works:**

1. Each state change is passed through to the underlying StateWriter
2. In parallel, the change is also written to Redis
3. Block number and hash are tracked to maintain chain identity
4. All state operations (account updates, storage changes, code updates) are captured

### 3. `redis_state_v3.go`

**Purpose:** Extends Erigon's StateV3 implementation to incorporate Redis storage during transaction execution.

**Key Components:**

- **RedisStateV3** - Wrapper that delegates to a standard StateV3 instance
- **Method Overrides** - Custom implementations for ApplyState4 and Unwind
- **Forwarding Methods** - Pass-through for most StateV3 functionality

**How It Works:**

1. Most operations are delegated to the underlying StateV3 implementation
2. Block and transaction data is captured during ApplyState4
3. Chain reorganizations are handled in the Unwind method
4. The Redis pipeline is flushed periodically to optimize performance

## Data Flow and Interaction

1. **Block Processing:**

   - When a block is processed, RedisStateV3 captures block header and metadata
   - For each transaction, transaction data and receipt are stored in Redis
   - State changes (account/storage/code) are captured by StateWriterWithRedis

2. **State Modifications:**

   - When state is modified, StateWriterWithRedis forwards changes to both Redis and the underlying StateWriter
   - Account updates, storage changes, and code deployment are tracked with block numbers

3. **Chain Reorganizations:**

   - When a reorg occurs, RedisStateV3.Unwind is called
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

4. **Composition over Inheritance:**
   - StateWriterWithRedis and RedisStateV3 use composition instead of inheritance
   - This minimizes changes to Erigon's core code and improves maintainability

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
