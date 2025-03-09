# Redis State Integration Guide

This guide provides instructions on how to use the Redis state mirroring solution with Erigon. The integration is designed to **keep Erigon working normally** while adding interceptors at key points to mirror state changes to Redis.

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
```

Example:
```
./build/bin/erigon --redis.enabled --redis.url="redis://localhost:6379/0"
```

## Implementation Details

The Redis state mirroring solution works by:

1. **State Writer Interception**: Wrapping the state accumulator during block execution to capture state changes
2. **Block Processing**: Mirroring block headers and receipts to Redis after a block is successfully processed
3. **Graceful Degradation**: Continuing normal Erigon operation even if Redis becomes unavailable

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

Use standard Redis monitoring tools like `redis-cli info` to track memory usage and operation counts.

## Implementation Details

The Redis integration intercepts three key points in Erigon:

1. During the initialization of Erigon in the `New` function in `eth/backend.go`, where we:
   - Create a Redis client based on configuration flags
   - Initialize the Redis integration

2. During block execution in the `ExecV3` function in `eth/stagedsync/exec3.go`, where we:
   - Wrap the state accumulator to capture all state changes and mirror them to Redis
   - This allows capturing all account, storage, and code changes in real-time

3. After block execution in the same file, where we:
   - Process the block header and receipts 
   - Store them in Redis for efficient retrieval

All Redis operations are designed to fail gracefully, allowing Erigon to continue operating normally even if Redis becomes unavailable.

## Fallback Handling

A critical aspect of this integration is graceful degradation. If Redis becomes unavailable, Erigon should continue to operate normally:

```go
// Example of proper error handling
func mirrorToRedis(redisClient *redis.Client, fn func() error) {
    if redisClient == nil {
        return // Do nothing if Redis is not configured
    }
    
    // Attempt the Redis operation
    err := fn()
    if err != nil {
        if errors.Is(err, redis.ErrClosed) || strings.Contains(err.Error(), "connection refused") {
            // Redis connection issue - log and continue
            log.Warn("Redis connection issue, continuing without state mirroring", "err", err)
        } else {
            // Other Redis error - log and continue
            log.Warn("Redis operation failed, continuing without state mirroring", "err", err)
        }
    }
}

// Use like this
mirrorToRedis(redisClient, func() error {
    return blockProcessor.ProcessBlockHeader(header)
})
```

## Verification

After integrating, you can verify it's working by:

1. Starting a Redis server
2. Running Erigon with Redis enabled
3. Letting it process some blocks
4. Using the Redis CLI to check if data is being stored:

```bash
redis-cli> KEYS *
redis-cli> ZRANGE account:0x1234567890abcdef 0 -1 WITHSCORES
```

## Summary

This integration approach:

1. Keeps Erigon working normally if Redis is unavailable
2. Captures all state changes in real-time
3. Has minimal impact on Erigon's performance
4. Requires changes in only a few, well-defined locations

By following these steps, you'll have a complete O(1) state mirroring solution that allows querying any historical state instantly via Redis, while maintaining full compatibility with the regular Erigon node functionality.