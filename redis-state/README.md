# Redis State for Erigon

This package provides a Redis-backed state storage solution for Erigon, offering O(1) access to any historical Ethereum state without changing Erigon's core functionality.

## Overview

The Redis state implementation allows querying account state, storage, and contracts from any historical block height with constant-time performance, regardless of the chain height. This is achieved by mirroring all state changes into Redis sorted sets, using block numbers as scores, which enables quick retrieval of state at any point in time.

## Features

- **O(1) Historical State Access**: Retrieve any account state or storage slot from any block in constant time
- **Non-intrusive**: Works alongside Erigon without modifying its core database structure
- **High Performance**: Uses Redis sorted sets for efficient retrieval of historical state
- **Standalone API Server**: Provides a dedicated API for state queries

## Components

### 1. Core Redis State Module (`redis_state.go`)

Implements the state reader and writer interfaces backed by Redis:

- `RedisStateReader`: Implements state reading from Redis
- `RedisStateWriter`: Implements state writing to Redis
- `RedisHistoricalWriter`: Extends the writer with history tracking capabilities
- `RedisBlockWriter`: Handles writing block-related data like headers and receipts

### 2. State Provider (`state_provider.go`)

Implements the RPC interface for querying state:

- Provides `StateAtBlock` and related methods for state access
- Implements the Ethereum execution API via Redis
- Includes compatibility with account abstraction

### 3. Integration Layer (`integration.go`)

Provides utilities for integrating with a running Erigon node:

- `StateInterceptor`: Intercepts state changes and mirrors them to Redis
- `BlockHeaderProcessor`: Processes block headers and stores them in Redis
- Ensures state consistency between Erigon and Redis

### 4. CLI Tool (`main.go`)

Command-line interface for operating the Redis state system:

- Runs a standalone API server for state queries
- Configurable via command-line flags

## Getting Started

### Prerequisites

- Running Erigon node with access to its chaindata
- Redis server (v6.0+)
- Go 1.20+

### Configuration

The Redis state service uses the following environment variables and flags:

```
--datadir            Path to Erigon data directory
--chaindata          Path to Erigon chaindata directory (if different from <datadir>/chaindata)
--redis-url          Redis connection URL (default: "redis://localhost:6379/0")
--redis-password     Redis password
--http.addr          HTTP-RPC server listening interface (default: "localhost")
--http.port          HTTP-RPC server listening port (default: "8545")
--http.api           API's offered over the HTTP-RPC interface (default: "eth,debug,net,web3")
--http.corsdomain    Comma separated list of domains from which to accept cross origin requests
--http.vhosts        Comma separated list of virtual hostnames from which to accept requests
--ws                 Enable the WS-RPC server
--ws.addr            WS-RPC server listening interface (default: "localhost")
--ws.port            WS-RPC server listening port (default: "8546")
--ws.api             API's offered over the WS-RPC interface (default: "eth,debug,net,web3")
--ws.origins         Origins from which to accept websockets requests
--log.level          Log level (trace, debug, info, warn, error, crit)
```

## Usage

### 1. Start the State API Server

To run the standalone API server:

```bash
redis-state --redis-url redis://localhost:6379/0 --http.addr 0.0.0.0 --http.port 8545 --http.api eth,debug,net,web3
```

This starts an HTTP JSON-RPC server that provides Ethereum API endpoints backed by the Redis state.

### 2. Use WebSocket for Subscriptions (Optional)

To enable WebSocket support:

```bash
redis-state --redis-url redis://localhost:6379/0 --ws --ws.addr 0.0.0.0 --ws.port 8546 --ws.api eth,debug,net,web3
```

### 3. Integrating with a Running Erigon Node

To continuously mirror state changes from a running Erigon node to Redis, you need to:

1. Create the proper interceptors in your Erigon node
2. Use the provided integration utilities
3. See `INTEGRATION.md` for detailed instructions

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

## Diagrams

### Architecture

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

### Data Flow

```
1. State Changes in Erigon
   │
   ▼
2. StateInterceptor captures changes
   │
   ▼
3. RedisStateWriter writes to Redis
   │
   ▼
4. Redis stores data in sorted sets
   │
   ▼
5. RedisStateReader reads historical state
   │
   ▼
6. State API Server exposes data via JSON-RPC
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

## Continuous Operation

For continuous operation where the Redis state tracks new blocks as they arrive:

1. Set up the Erigon node with the integration module
2. The StateInterceptor will automatically mirror all state changes to Redis
3. Run the Redis State API server to expose the data

## Troubleshooting

### Common Issues

- **Connection refused**: Ensure Redis server is running and accessible
- **Memory usage**: Monitor Redis memory usage and consider tuning maxmemory settings
- **Performance degradation**: Check Redis persistence settings, consider disabling AOF for better performance

### Monitoring

The Redis state system logs useful information for monitoring:

- Error conditions during operation
- Performance metrics

Use standard Redis monitoring tools like `redis-cli info` to track memory usage and operation counts.

## Recent Improvements

The Redis state implementation has been recently enhanced with the following improvements:

### Performance Optimizations

- Added context timeouts to all Redis operations to prevent hanging
- Implemented Redis pipelines for batching operations and reducing network roundtrips
- Added optimization for code storage to avoid redundant writes for immutable data
- Improved indexing of logs for more efficient filtering by address and topics

### Reliability Enhancements

- Added comprehensive error handling with proper error propagation
- Improved error logging with context-specific messages
- Implemented fallback mechanism to continue operation despite Redis failures
- Added optional expiration for certain indices to manage memory usage

### Data Model Improvements

- Enhanced the block transactions indexing for faster retrieval
- Added block summary data for quick access to critical block information
- Improved logging organization with block+address+topic composite keys
- Implemented more efficient transaction storage pattern

## Contributing

Contributions to the Redis state implementation are welcome! Please follow the Erigon project's contribution guidelines.

## License

This implementation is part of Erigon and is licensed under the GNU Lesser General Public License v3 or later.
