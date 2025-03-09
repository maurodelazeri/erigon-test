# Redis State Integration for Erigon

This module implements integration between Erigon and Redis for state persistence. It allows Erigon to write blockchain state to Redis in parallel with its normal database writes, providing O(1) access to any historical state.

## Overview

The Redis state integration enables several use cases:
- Maintaining a secondary copy of the blockchain state for redundancy
- Exposing blockchain state through a Redis interface for external applications
- Supporting custom analytical applications with live state updates
- Allowing blockchain explorers to access state without direct Erigon API access

## How O(1) Access Works with Redis

The key innovation is using Redis sorted sets to store state changes with block numbers as scores. This approach enables:

1. **Constant-Time Lookups**: Finding the state at any block takes the same amount of time, whether it's yesterday's block or from years ago
2. **No Chain Traversal**: Traditional systems need to replay or traverse the chain; ours doesn't
3. **Efficient Storage**: We only store states when they change, not for every block

## Configuration

To enable Redis state persistence, use the following command line flags:

```
--redis.enabled             Enable Redis state persistence (default: false)
--redis.url                 Redis connection URL (default: "redis://localhost:6379/0")
--redis.password            Redis password (default: "")
```

## Architecture

The Redis state integration uses a factory pattern to avoid circular dependencies between packages. The main components are:

1. **RedisStateReader**: Reads state from Redis at the latest block or a specific historical block
2. **RedisStateWriter**: Writes state changes to Redis
3. **RedisHistoricalWriter**: Extends RedisStateWriter with history awareness
4. **RedisStateProvider**: Implements provider interfaces for Erigon's RPC API

## Data Model

State data is stored in Redis using the following key patterns:

### Account and State Data
- `account:{address}` - Account data (sorted set with block numbers as scores)
- `storage:{address}:{key}` - Storage data (sorted set with block numbers as scores)
- `code:{codehash}` - Contract code

### Block Data
- `block:{number}` - Block data
- `blockHash:{hash}` - Mapping from block hash to block number
- `canonicalChain` - Sorted set of canonical block hashes with block numbers as scores
- `currentBlock` - Latest block number
- `reorg:{blockNum}` - Marker for blocks that were part of a chain reorganization

### Transaction and Receipt Data
- `tx:{hash}` - Transaction data (sorted set with block numbers as scores)
- `block:{number}:txs` - Set of transaction hashes in a block
- `sender:{address}:txs` - Transactions sent by an address (sorted set with block numbers as scores)
- `receipt:{hash}` - Receipt data (sorted set with block numbers as scores)
- `block:{number}:receipts` - Ordered list of receipts in a block (sorted set with tx indices as scores)

### Event Logs
- `log:{index}` - Log data
- `topic:{hash}` - Logs by topic (sorted set with block numbers as scores)
- `address:{address}:logs` - Logs by emitting address (sorted set with block numbers as scores)

## Chain Reorganization Handling

The Redis state integration handles chain reorganizations (reorgs) by:

1. Detecting when blocks are unwound during a reorg
2. Marking reorged blocks with special markers
3. Tracking the canonical chain to distinguish between canonical and non-canonical blocks
4. Providing accurate historical state access even after reorgs

## Practical Example

Let's walk through a concrete example of how this works:

### Scenario: Querying an Account Balance at Block 2000

Imagine an account that has changed its balance only at blocks 20, 450, and 3000:

```
Block 20:   Balance set to 5 ETH
Block 450:  Balance changed to 7.5 ETH
Block 3000: Balance changed to 10 ETH
```

When querying for the balance at block 2000, our Redis State system:

1. Uses `ZREVRANGEBYSCORE` to get the most recent state change ≤ block 2000
2. Immediately retrieves the value from block 450 (7.5 ETH) in a single operation
3. Performance remains constant regardless of chain size (O(1) complexity)

## Usage Examples

### Accessing Account State

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/erigontech/erigon-lib/common"
    "github.com/erigontech/erigon/redis-state"
    "github.com/redis/go-redis/v9"
)

func main() {
    // Connect to Redis
    client := redis.NewClient(&redis.Options{
        Addr: "localhost:6379",
    })

    // Create a state provider
    provider := redisstate.NewRedisStateProvider(client, log.Default())

    // Get balance of an account
    address := common.HexToAddress("0x742d35Cc6634C0532925a3b844Bc454e4438f44e")
    balance, err := provider.BalanceAt(context.Background(), address, -1) // Latest block
    if err != nil {
        log.Fatalf("Error getting balance: %v", err)
    }

    fmt.Printf("Balance: %s\n", balance.String())
}
```

### Accessing Transactions and Receipts

```go
// Get transaction by hash
tx, blockHash, blockNum, txIndex, err := provider.GetTransactionByHash(ctx, txHash)
if err != nil {
    log.Fatalf("Error getting transaction: %v", err)
}

// Get receipt for a transaction
receipt, err := provider.GetTransactionReceipt(ctx, txHash)
if err != nil {
    log.Fatalf("Error getting receipt: %v", err)
}

// Get logs by filter
filter := &types.LogFilter{
    FromBlock: rpc.BlockNumber(10000000),
    ToBlock:   rpc.BlockNumber(10001000),
    Addresses: []common.Address{contractAddress},
    Topics:    [][]common.Hash{{eventSignature}},
}
logs, err := provider.GetLogs(ctx, filter)
```

## Complete Functionality

The Redis system stores more than just account and storage state:

1. **Block Headers**: Complete block metadata is stored
2. **Transaction Data**: All transaction details are mirrored to Redis
3. **Event Logs**: Log entries emitted by contracts are captured
4. **Chain Structure**: Block relationships are tracked to handle reorganizations

## Implementation Details

For efficiency, we only store state changes when values actually change:

- If an account balance doesn't change for 10,000 blocks, we only store one entry
- If a storage slot is modified frequently, we store each unique change

For more detailed implementation information, please see [IMPLEMENTATION.md](IMPLEMENTATION.md)

## Limitations

- Redis storage can grow large for mainnet deployments - consider using Redis with persistence disabled for ephemeral use cases
- Performance under heavy load depends on Redis server capacity
- For production deployments, consider using Redis Cluster for scalability

## Recent Improvements

- **Enhanced Integration**: Full integration with Erigon's block, transaction and state processing
- **Self-destruct Handling**: Properly track account deletion during state transitions
- **Block Finalization**: Complete block metadata is now stored with state root and header hash
- **Chain Reorg Awareness**: Improved tracking of canonical chain during reorganizations
- **Cache Management**: Added writer caching for better performance during block processing
- **Factory Pattern**: Improved factory implementation to avoid circular dependencies

## Future Improvements

- Implement pruning strategies for old state data
- Add support for sharded Redis deployments 
- Optimize storage format for reduced memory usage
- Add streaming capabilities for real-time state updates
- Add support for Redis Cluster and Redis Sentinel for high availability
- Implement batch operations for improved write performance
- Add metrics and observability for Redis operations
