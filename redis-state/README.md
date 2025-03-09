# Redis State for Erigon

This package provides a Redis-backed state storage solution for Erigon, offering O(1) access to any historical Ethereum state without changing Erigon's core functionality.

## Overview

The Redis state implementation allows querying account state, storage, and contracts from any historical block height with constant-time performance, regardless of the chain height. This is achieved by mirroring all state changes into Redis sorted sets, using block numbers as scores, which enables quick retrieval of state at any point in time.

## Features

- **O(1) Historical State Access**: Retrieve any account state or storage slot from any block in constant time
- **Non-intrusive**: Works alongside Erigon without modifying its core database structure
- **Full State Coverage**: Captures account balances, nonces, storage, and contract code
- **High Performance**: Uses Redis sorted sets for efficient retrieval of historical state
- **Standalone API Server**: Provides a dedicated API for state queries

## Components

### 1. Core Redis State Module (`redis_state.go`)

Implements the state reader and writer interfaces backed by Redis:

- `RedisStateReader`: Implements state reading from Redis
- `RedisStateWriter`: Implements state writing to Redis
- `RedisHistoricalWriter`: Extends the writer with history tracking capabilities

### 2. Block and Transaction Writer (`block_writer.go`)

Manages block-related data storage in Redis:

- Stores block headers and transaction data
- Tracks receipts and event logs
- Handles chain reorganizations

### 3. Integration Layer (`integration.go`)

Provides utilities for integrating with a running Erigon node:

- `StateInterceptor`: Intercepts state changes and mirrors them to Redis
- `BlockHeaderProcessor`: Processes block headers and stores them in Redis
- Ensures state consistency between Erigon and Redis

### 4. Hook Management (`redis_hooks.go`)

Manages the registration and usage of hooks to capture state changes:

- Registers state writer wrappers
- Sets up block write hooks
- Handles transaction write hooks

### 5. State Provider (`state_provider.go`)

Implements the RPC interface for querying state:

- Provides `StateAtBlock` and related methods for state access
- Implements the Ethereum execution API via Redis
- Includes compatibility with account abstraction

### 6. CLI Tool (`cmd/redis-state/main.go`)

Command-line interface for operating the Redis state system:

- Runs a standalone API server for state queries
- Configurable via command-line flags

## State Flow in Erigon Integration

The critical flow for capturing state changes:

1. **State Writer Wrapping**: During transaction execution in `core/state_processor.go`, the state writer is wrapped:
   ```go
   stateWriter = state.WrapStateWriter(stateWriter, header.Number.Uint64())
   ```

2. **State Interception**: When state changes occur:
   - Account changes go through `StateInterceptor.UpdateAccountData()`
   - Storage changes go through `StateInterceptor.WriteAccountStorage()`
   - Contract code changes go through `StateInterceptor.UpdateAccountCode()`

3. **Storage Call Path**:
   - `IntraBlockState.SetState()` is called by the EVM
   - `stateObject.SetState()` records changes in `dirtyStorage`
   - During `FinalizeTx()`, `stateObject.updateTrie()` is called
   - `updateTrie()` calls `stateWriter.WriteAccountStorage()` for each dirty slot
   - Our wrapper intercepts this and writes to both Erigon DB and Redis

## Getting Started

### Prerequisites

- Running Erigon node with access to its chaindata
- Redis server (v6.0+)
- Go 1.20+

### Configuration

The Redis state integration is enabled with the following flags:

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
```bash
./build/bin/erigon --redis.enabled --redis.url="redis://localhost:6379/0"
```

The standalone API server supports additional parameters:

```
--http.addr          HTTP-RPC server listening interface (default: "localhost")
--http.port          HTTP-RPC server listening port (default: "8545")
--http.api           API's offered over the HTTP-RPC interface (default: "eth,debug,net,web3")
--http.corsdomain    Comma separated list of domains from which to accept cross origin requests
--ws                 Enable the WS-RPC server
```

## Usage

### Running Erigon with Redis Integration

To capture state in Redis while running Erigon:

```bash
./build/bin/erigon --redis.enabled --redis.url="redis://localhost:6379/0"
```

### Using the Standalone State API Server

To run the standalone API server:

```bash
./build/bin/redis-state --redis-url="redis://localhost:6379/0" --http.addr="0.0.0.0" --http.port="8545"
```

### Verifying Integration

You can verify the integration is working using Redis CLI:

```bash
# List all Redis keys
redis-cli KEYS *

# Check for account states
redis-cli KEYS "account:*"

# Check for storage values
redis-cli KEYS "storage:*"

# Check for a specific contract's storage
redis-cli KEYS "storage:0x1234567890abcdef:*"
```

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
- Consider using Redis persistence options like RDB or AOF for data durability
- For large-scale deployments, consider Redis Cluster for horizontal scaling

## Debugging and Troubleshooting

If you're not seeing account state or storage data in Redis:

1. **Verify Redis integration is enabled**:
   - Make sure you're running with `--redis.enabled`
   - Check for logs indicating "Redis state integration initialized successfully"

2. **Add debug logging in key places**:
   - In `stateObject.updateTrie()` to verify storage is being written
   - In `RedisStateWriter.WriteAccountStorage()` to verify Redis operations

3. **Test with active contracts**:
   - Deploy a simple storage contract
   - Make transactions that modify its state
   - Check Redis for the storage changes

4. **Common Redis Issues**:
   - Connection refused: Ensure Redis server is running and accessible
   - Memory issues: Monitor Redis memory usage
   - Connection limits: Adjust poolsize parameter for high-load systems

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
                        │  State API     │
                        │  Server        │
                        │                │
                        └────────────────┘
                               ▲
                               │
                               ▼
                        ┌────────────────┐
                        │                │
                        │  Client Apps   │
                        │                │
                        └────────────────┘
```

## Future Improvements

Planned enhancements for the Redis state integration:

1. **Pruning Options**: Configure retention policies for historical state
2. **Optimized Storage**: Compression and deduplication of repeated values
3. **Sharding**: Support for Redis Cluster with strategic key distribution
4. **Streaming Updates**: WebSocket-based state change notifications
5. **Plugin System**: Extensible hooks for custom data processing

## License

This implementation is part of Erigon and is licensed under the GNU Lesser General Public License v3 or later.