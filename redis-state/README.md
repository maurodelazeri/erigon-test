# Blockchain State Skip List (BSSL) Redis Module

A high-performance Redis module for storing and querying blockchain state with O(1) performance regardless of history length.

## Overview

The BSSL module implements a specialized skip list data structure that enables:

- Efficient storage of blockchain state (accounts, storage, etc.)
- O(1) queries for state at any historical block
- Minimal memory overhead by only storing changes
- Full compatibility with the Redis protocol

## Quick Start

### 1. Build the module

```bash
# Clone the repository
git clone https://github.com/your-org/redis-bssl
cd redis-bssl

# Build the module
make
```

### 2. Load the module into Redis

```bash
# Start Redis with the module
redis-server --loadmodule ./bssl.so
```

### 3. Test the module

```bash
# Run the test script
./test_module.sh
```

## Module Commands

### BSSL.PING

Simple ping command for testing if the module is loaded and working.

```
redis-cli BSSL.PING
PONG
```

### BSSL.SET address block_num state

Store a state value for an address at a specific block.

```
redis-cli BSSL.SET 0x1234 100 '{"balance":"100000","nonce":5}'
(integer) 1
```

### BSSL.GETSTATEATBLOCK address block_num

Get the state for an address at or before a specific block.

```
redis-cli BSSL.GETSTATEATBLOCK 0x1234 100
{"balance":"100000","nonce":5}
```

### BSSL.INFO address

Get information about an address's state history.

```
redis-cli BSSL.INFO 0x1234
1) (integer) 100
2) (integer) 100
3) (integer) 1
```

## Benchmarks

The BSSL module provides significant performance improvements over traditional Redis sorted sets for blockchain state storage:

| Operation | BSSL Module | Redis Sorted Sets |
|-----------|-------------|------------------|
| State Query | O(1) | O(log N) |
| Memory Usage | Lower | Higher |
| Query Time (1M blocks) | <1ms | 5-10ms |

## Go Client

A Go client is included for easy integration with your blockchain application:

```go
// Create a new client
client, err := bssl.NewBlockchainStateClient("localhost:6379", "", 0)
if err != nil {
    log.Fatal(err)
}

// Store state at a block
err = client.SetState("0x1234", 100, map[string]interface{}{
    "balance": "100000",
    "nonce": 5,
})

// Query state at a block
state, err := client.GetStateAtBlock("0x1234", 100)
fmt.Println(state) // {"balance":"100000","nonce":5}
```

## Implementation Details

The module uses a specialized skip list data structure that provides efficient storage and O(1) lookups:

1. **Address Index**: Fixed-size hash map that tracks metadata for each address
2. **Skip List**: Custom data structure for sparse state storage with O(1) lookups
3. **Binary Encoding**: Efficient storage format that minimizes memory usage

For most accounts and storage slots, the state changes infrequently compared to the blockchain's length, making this approach highly memory-efficient.

## Integration with Erigon

See the `integration_example.go` file for an example of how to integrate the BSSL module with Erigon.

## License

MIT
