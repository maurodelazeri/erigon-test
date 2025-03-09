# Redis State Integration Guide

This guide provides instructions on how to use the Redis state mirroring solution with Erigon. The integration captures all state changes including account balances, storage, and code, and mirrors them to Redis for O(1) historical state access.

## Overview

The Redis state mirroring solution provides:

1. **O(1) Historical State Access**: Access any account, storage, or code at any block height in constant time
2. **Non-Intrusive Integration**: Keeps Erigon working normally, with Redis mirroring as an optional layer
3. **Real-Time State Mirroring**: Captures state changes as they happen during block execution

## Configuration

To enable Redis state mirroring, use the following command-line flags:

```
--redis.enabled            Enable Redis state mirroring for O(1) historical state access
--redis.url                Redis connection URL (default: "redis://localhost:6379/0")
--redis.password           Redis password (optional)
--redis.poolsize           Redis connection pool size (default: 10)
--redis.maxretries         Redis maximum retries (default: 3)
--redis.timeout            Redis connection timeout (default: 5s)
--redis.loglevel           Redis state integration log level (default: "info")
```

Example:
```
./build/bin/erigon --redis.enabled --redis.url="redis://localhost:6379/0"
```

## How State Capture Works

The Redis state integration works through several key interception points:

1. **State Writer Wrapping**: During transaction execution, the state writer is wrapped with our Redis interceptor in `core/state_processor.go`:

```go
// REDIS INTEGRATION: Wrap the stateWriter with our registered wrappers
stateWriter = state.WrapStateWriter(stateWriter, header.Number.Uint64())
```

2. **Account & Storage Interception**: When account data or storage changes:
   - Account changes are captured through `StateInterceptor.UpdateAccountData()`
   - Storage changes are captured through `StateInterceptor.WriteAccountStorage()`
   - Code changes are captured through `StateInterceptor.UpdateAccountCode()`

3. **Block & Transaction Hooks**: These items are captured through hooks registered with core components:
   - `rawdb.RegisterBlockWriteHook(integration.BlockWriteHook)`
   - `rawdb.RegisterHeaderWriteHook(integration.HeaderWriteHook)`
   - `rawdb.RegisterTransactionWriteHook(integration.TransactionWriteHook)`

## Data Model

The Redis state implementation uses the following key patterns:

- `account:{address}`: Sorted set of account states by block number
- `storage:{address}:{key}`: Sorted set of storage values by block number
- `code:{codeHash}`: Contract bytecode
- `block:{blockNum}`: Hash map of block data
- `blockHash:{blockHash}`: Mapping from block hash to block number
- `receipt:{txHash}`: Sorted set of transaction receipts by block number
- `logs:{blockNum}:{logIndex}`: Log entries
- `address:{address}`: Sorted set of log keys by block number for an address
- `topic:{topic}`: Sorted set of log keys by block number for a topic
- `currentBlock`: Latest block number

## Performance Considerations

- Redis memory usage scales with the size of the state and the number of historical changes
- For large-scale deployments, consider Redis Cluster for horizontal scaling
- Consider using Redis persistence options like RDB or AOF for data durability
- Use connection pooling to handle concurrent state access efficiently

## Architecture

```
┌────────────────┐      ┌───────────────┐      ┌────────────────┐
│                │      │               │      │                │
│  Erigon Node   │◄────►│ Redis State   │◄────►│  Redis Server  │
│                │      │ Integration   │      │                │
└────────────────┘      └───────────────┘      └────────────────┘
                               ▲
                               │
                               ▼
                        ┌────────────────┐
                        │                │
                        │  API Clients   │
                        │                │
                        └────────────────┘
```

## State Change Flow

The complete flow for state changes being captured and written to Redis:

1. `IntraBlockState.SetState()` is called during transaction execution
2. This updates the state in memory and records the change in the journal
3. During `FinalizeTx()`, `stateObject.updateTrie()` is called
4. `updateTrie()` calls `stateWriter.WriteAccountStorage()` for each dirty storage slot
5. Our `StateInterceptor.WriteAccountStorage()` intercepts this call
6. The interceptor writes to both Erigon's DB and Redis for each change

For account balance and nonce changes, a similar flow occurs through `UpdateAccountData()`.

## Debugging State Changes

If you're not seeing expected state changes in Redis, add these debug points:

1. In `core/state/state_object.go` - Add logging in `updateTrie()` to show storage writes
2. In `redis-state/redis_state.go` - Add logging in `WriteAccountStorage()` to show Redis writes
3. In transaction tests - Deploy a simple storage contract and check for its state changes
4. In Redis CLI - Use `KEYS "storage:*"` to verify storage slots are captured

## Function Call Flow for State Retrieval

```
Client Request
   │
   ▼
StateAtBlock(blockNumber)
   │
   ▼
PointInTimeRedisStateReader
   │
   ▼
ZRevRangeByScore(key, 0, blockNumber)
   │
   ▼
Process and return result
```

## Troubleshooting

Common issues:

- **Connection refused**: Ensure Redis server is running and accessible
- **Timeouts during operation**: For high-load systems, consider increasing Redis connection pool size
- **Memory usage**: Monitor Redis memory usage and consider tuning maxmemory settings
- **Performance degradation**: Check Redis persistence settings, consider disabling AOF for better performance
- **Missing account or storage data**: Ensure `--redis.enabled` flag is set and state writer hooks are registered

Use standard Redis monitoring tools like `redis-cli info` to track memory usage and operation counts.

## Implementation Details

The Redis integration intercepts three key points in Erigon:

1. During the initialization of Erigon in the `New` function in `eth/backend.go`, where we:
   - Create a Redis client based on configuration flags
   - Initialize the Redis integration
   - Register all state hooks via `RegisterAllHooks()`

2. During transaction execution in `core/state_processor.go`, where we:
   - Wrap the state writer with `state.WrapStateWriter(stateWriter, header.Number.Uint64())`
   - This intercepts all account, storage, and code changes

3. During block processing via registered hooks, where we:
   - Capture block headers and receipts
   - Store transaction data
   - Update canonical chain tracking

All Redis operations are designed to fail gracefully, allowing Erigon to continue operating normally even if Redis becomes unavailable.

## Verification

After integrating, you can verify it's working by:

1. Starting a Redis server
2. Running Erigon with Redis enabled
3. Letting it process some blocks
4. Using the Redis CLI to check if data is being stored:

```bash
redis-cli> KEYS *
redis-cli> ZRANGE account:0x1234567890abcdef 0 -1 WITHSCORES
redis-cli> KEYS "storage:*"
```

## Summary

This integration approach:

1. Keeps Erigon working normally if Redis is unavailable
2. Captures all state changes in real-time
3. Has minimal impact on Erigon's performance
4. Provides O(1) access to historical state at any block height

By following these steps, you'll have a complete O(1) state mirroring solution that allows querying any historical state instantly via Redis, while maintaining full compatibility with the regular Erigon node functionality.