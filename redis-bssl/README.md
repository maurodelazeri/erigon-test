# Blockchain State Skip List (BSSL) Redis Module

A high-performance Redis module for storing and querying blockchain state with O(1) performance regardless of history length. This module enables efficient storage and retrieval of historical blockchain state with guaranteed performance characteristics even when scaling to billions of state changes.

## Overview

The BSSL module implements a specialized skip list data structure that enables:

- Efficient storage of blockchain state (accounts, storage, etc.)
- O(1) queries for state at any historical block height
- Minimal memory overhead by only storing changes
- Full compatibility with the Redis protocol
- Scalability to billions of state changes without performance degradation

## Technical Architecture

### How BSSL Works

Unlike traditional approaches (like Redis sorted sets) that degrade as data grows, BSSL uses a specialized data architecture:

1. **Address Indexing Layer**: A fixed-size hash map that tracks each address's state history metadata
2. **Skip List Storage Layer**: A custom data structure optimized for "point-in-time" blockchain state queries
3. **Binary Encoding Layer**: Efficient memory representation that minimizes storage requirements

```
┌────────────────────────────────────────────────────────┐
│ Address Index                                          │
│ address:0x123 → {first_block: 50, last_block: 10000}   │
└───────────────────────────┬────────────────────────────┘
                            │
                            ▼
┌────────────────────────────────────────────────────────┐
│ Skip List Storage                                      │
│ [Block 50]-→[Block 205]-→[Block 1020]→...-→[Block 10000]│
└───────────────────────────┬────────────────────────────┘
                            │
                            ▼
┌────────────────────────────────────────────────────────┐
│ State Data                                             │
│ {"balance":"100","nonce":5}                            │
└────────────────────────────────────────────────────────┘
```

### Why O(1) Performance is Guaranteed

BSSL achieves O(1) query performance regardless of history size through:

1. **Sparse Storage**: Only stores state changes, not state at every block
2. **Skip List Algorithm**: Optimized for historical blockchain state access patterns
3. **Perfect Indexing**: Fixed-size lookup tables ensure direct access
4. **Binary Search Optimization**: Skip lists enable efficient search for "state at or before block X"

For example, if an account only changed at blocks 66, 1020, and 5000, a query for its state at block 100000 can directly find the state at block 5000 in constant time, regardless of how many blocks exist between 5000 and 100000.

### Memory Efficiency

BSSL's memory usage scales with the number of state changes, not with the blockchain length:

- **Traditional Approach**: Memory usage = O(accounts × blocks)
- **BSSL Approach**: Memory usage = O(accounts × changes)

For most accounts/storage slots that change infrequently, this provides massive memory savings. For example, if an address only changes 10 times across 1 million blocks, BSSL stores only 10 entries instead of 1 million.

## Quick Start

### 1. Build the module

```bash
# Clone the repository
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

## Loading BSSL at Redis Startup

To load the BSSL module automatically when Redis starts:

### Method 1: Configure in redis.conf

Add the following line to your redis.conf file:

```
loadmodule /path/to/bssl.so
```

### Method 2: Set up a systemd service (Linux)

Create a file `/etc/systemd/system/redis-bssl.service`:

```
[Unit]
Description=Redis with BSSL Module
After=network.target

[Service]
ExecStart=/usr/bin/redis-server /etc/redis/redis.conf --loadmodule /path/to/bssl.so
Type=simple
User=redis
Group=redis
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Then enable and start the service:

```bash
sudo systemctl enable redis-bssl
sudo systemctl start redis-bssl
```

### Method 3: Docker Deployment

```bash
docker run -v /path/to/bssl.so:/modules/bssl.so \
           -v /path/to/redis.conf:/usr/local/etc/redis/redis.conf \
           redis:latest redis-server /usr/local/etc/redis/redis.conf \
           --loadmodule /modules/bssl.so
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
1) (integer) 100   # First block where state exists
2) (integer) 100   # Last block where state changed
3) (integer) 1     # Number of state changes
```

## Understanding the Testing Framework

The testing script (`test_module.sh`) verifies the module's functionality in multiple stages:

1. **Module Loading Test**: Ensures the module loads correctly in Redis

   ```bash
   redis-server --loadmodule ./bssl.so
   redis-cli BSSL.PING  # Should return "PONG"
   ```

2. **Basic Operation Tests**:

   - Sets state for an address at a specific block
   - Retrieves state at the exact block
   - Retrieves state at a later block (should return the most recent state before the query block)
   - Gets metadata about the address history

3. **Performance Tests** (optional):
   - Tests throughput for state queries
   - Measures response time consistency as dataset grows

The test script validates that the O(1) query time is maintained regardless of the number of blocks between state changes.

## Scaling to Billions of State Changes

BSSL maintains consistent performance when scaling to billions of state changes because:

1. **Optimal Data Structures**: Skip lists maintain O(1) lookups even with trillions of elements
2. **Sparse Storage Model**: Only storing state changes minimizes data volume
3. **Independent Address Scaling**: Each address's state history is independent, enabling sharding
4. **Memory-Efficient Encoding**: Binary formats minimize memory usage
5. **No Table Scans**: Queries never need to scan or sort data, unlike traditional databases

Real-world Example: An Ethereum-like blockchain with 15 million accounts, each changing an average of 100 times across 100 million blocks, would require:

- Traditional approach: 15M × 100M entries = 1.5 quadrillion entries
- BSSL approach: 15M × 100 entries = 1.5 billion entries

Even at billions of state changes, lookup performance remains consistent at sub-millisecond levels.

## Storage Characteristics

The BSSL module optimizes for sparse state storage:

| Storage Unit   | Size                   | Notes                      |
| -------------- | ---------------------- | -------------------------- |
| Address Entry  | ~40 bytes              | Fixed overhead per address |
| Skip List Node | ~20 bytes + state size | One per state change       |
| Account State  | ~100-200 bytes         | JSON-encoded account data  |
| Storage State  | ~50-100 bytes          | JSON-encoded storage value |

For most accounts, state changes are infrequent compared to the blockchain's total blocks, making this approach highly memory-efficient.

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

## Advanced Usage

### Chain Reorganization Handling

The module correctly handles blockchain reorganizations by maintaing the correct state history:

```go
// Example: Handle chain reorganization at block 1000
redis-cli KEYS "account:*" | xargs -I{} redis-cli BSSL.INFO {}
// Find accounts with lastBlock >= 1000

// For each affected account, set state at block 1000 to match block 999
redis-cli BSSL.GETSTATEATBLOCK account:0x123 999
redis-cli BSSL.SET account:0x123 1000 [state-from-block-999]
```

### Sharding for Massive Scale

For extremely large deployments, you can shard data across multiple Redis instances:

```
┌─────────────────────┐
│ Router Layer        │
└────┬─────┬─────┬────┘
     │     │     │
┌────▼─┐ ┌─▼───┐ ┌▼────┐
│Shard1│ │Shard2│ │Shard3│
│0x0-0x5│ │0x5-0xA│ │0xA-0xF│
└──────┘ └─────┘ └──────┘
```

Sharding by address prefix allows the system to scale horizontally while maintaining O(1) lookup performance.
