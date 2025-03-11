# Redis State Integration for Erigon

## Introduction

The Redis state integration allows Erigon to store blockchain data in Redis, making it possible to instantly access historical state from any block height. Instead of having to replay transactions or recreate historical states, you can query an account or storage slot's value at any point in history with consistent O(1) performance.

This document explains how the integration works, how data is stored, and how to query it - all without requiring prior knowledge of the system.

## Why Use Redis for Blockchain State?

Traditional blockchain nodes struggle to provide efficient access to historical state. Redis integration solves this by:

- **Speed**: O(1) access to any historical state regardless of chain length
- **Simplicity**: Simple query patterns to retrieve historical data
- **Flexibility**: Ability to support APIs, transaction simulations, and debugging tools
- **Efficiency**: Skip list technology minimizes storage by only tracking changes

## Component Overview

The Redis integration consists of three main components:

1. **RedisState** (`redis_state.go`): Core client that manages Redis connection and provides data access
2. **RedisStateMonitor** (`redis_state_v3.go`): Captures block, transaction, and receipt data
3. **StateWriterWithRedis** (`redis_writer.go`): Captures state changes (accounts, storage, code)

These components work together to track all blockchain data without requiring major changes to Erigon's core code.

## How Data is Stored in Redis

### The BSSL Module

At the heart of this integration is the **Blockchain State Skip List (BSSL)** Redis module, which provides efficient storage and retrieval of historical state using specialized skip list data structures.

The BSSL module stores data with a block number index, allowing you to find state "at or before" any given block. It only stores data when it changes, which means storage requirements are proportional to the number of state changes rather than the total number of blocks.

### Data Types and Key Patterns

#### 1. Account Data

```
Key: account:{address}
Example: account:0x1f9840a85d5aF5bf1D1762F925BDADdC4201F984
Storage: BSSL module
```

Account data is stored as JSON with these fields:

```json
{
  "balance": "1234567890",
  "nonce": 5,
  "codeHash": "0x1234abcd...",
  "incarnation": 1
}
```

#### 2. Contract Storage

```
Key: storage:{address}:{slot}
Example: storage:0x1f9840a85d5aF5bf1D1762F925BDADdC4201F984:0x8f0162f3d08f8d0712b4c2b179f86fce4e93a879b8b7d140aa82eb91cf66215e
Storage: BSSL module
```

Storage data is stored as JSON:

```json
{
  "value": "0x000000000000000000000000000000000000000000000000000000000000002a"
}
```

When a storage slot is deleted (set to zero), the value is stored as:

```json
{
  "value": "null"
}
```

#### 3. Contract Code

```
Key: code:{codeHash}
Example: code:0xabcd1234...
Storage: Regular Redis string (immutable)
```

Code is stored as raw binary data. Since code is immutable (can't change once deployed), it's stored using `SetNX` to ensure it's only written once.

#### 4. Block Data

```
Key: block:{blockNum}
Example: block:15000000
Storage: Redis string (SET)
```

Block data is stored as RLP-encoded binary data of the entire block header. This allows for efficient storage and retrieval of complete block information.

#### 5. Block Hash Mapping

```
Key: blockHash:{blockHash}
Example: blockHash:0x8f5bab218b6bb34476f51ca588e9f4553a3a7ce5e13a66c660a5283e97e9a85a
Storage: Redis string (SET)
```

Block hash mapping is stored as JSON:

```json
{
  "number": 15000000,
  "timestamp": 1676392017
}
```

#### 6. Transactions

```
Key: txs:{blockNum}:{txIndex}
Example: txs:15000000:0, txs:15000000:1, txs:15000000:2...
Storage: Redis string (SET)
```

Each transaction is stored as RLP-encoded binary data in its own key, with the transaction index included in the key.

#### 7. Receipts

```
Key: receipts:{blockNum}:{txIndex}
Example: receipts:15000000:0, receipts:15000000:1, receipts:15000000:2...
Storage: Redis string (SET)
```

Each receipt is stored as RLP-encoded binary data in its own key, with the transaction index included in the key.

#### 8. Current Block Tracker

```
Key: currentBlock
Storage: Simple string value
```

Stores the latest processed block number.

### Behind the Scenes: BSSL Internal Keys

While your application code shouldn't need to interact with these directly, it's helpful to understand that BSSL internally uses these key patterns:

```
bssl:state_list:{type}:{address}[:slot] - Stores the skip list data structure
bssl:meta:{type}:{address}[:slot] - Stores metadata about state changes
bssl:last:{type}:{address}[:slot] - Caches the latest state
```

You might see these when using `KEYS` or other Redis commands, but you should use the BSSL module commands to interact with the data instead.

## How to Query Data

### Account State at a Block Height

To get an account's state at a specific block:

```
BSSL.GETSTATEATBLOCK account:{address} {blockNum}
```

Example:

```
BSSL.GETSTATEATBLOCK account:0x1f9840a85d5aF5bf1D1762F925BDADdC4201F984 15000000
```

Response (JSON string):

```
{"balance":"1234567890","nonce":5,"codeHash":"0x1234abcd...","incarnation":1}
```

If the account didn't exist at that block, you'll get a null response.

### Storage Value at a Block Height

To get a storage slot's value at a specific block:

```
BSSL.GETSTATEATBLOCK storage:{address}:{slot} {blockNum}
```

Example:

```
BSSL.GETSTATEATBLOCK storage:0x1f9840a85d5aF5bf1D1762F925BDADdC4201F984:0x8f0162f3d08f8d0712b4c2b179f86fce4e93a879b8b7d140aa82eb91cf66215e 15000000
```

Response (JSON string):

```
{"value":"0x000000000000000000000000000000000000000000000000000000000000002a"}
```

### Block Data

To get block information:

```
GET block:{blockNum}
```

Example:

```
GET block:15000000
```

Response:
RLP-encoded binary data of the block header. This needs to be decoded using an RLP decoder to access individual fields.

### Block Hash Mapping

To get block number from hash:

```
GET blockHash:{blockHash}
```

Example:

```
GET blockHash:0x8f5bab218b6bb34476f51ca588e9f4553a3a7ce5e13a66c660a5283e97e9a85a
```

Response (JSON string):

```
{"number":15000000,"timestamp":1676392017}
```

### Transactions in a Block

To get all transactions in a block, first get all transaction keys:

```
KEYS txs:{blockNum}:*
```

Example:

```
KEYS txs:15000000:*
```

Response:

```
1) "txs:15000000:0"
2) "txs:15000000:1"
3) "txs:15000000:2"
...
```

Then get each transaction:

```
GET txs:15000000:0
GET txs:15000000:1
...
```

Each response contains RLP-encoded transaction data.

### Receipts in a Block

To get all receipts in a block, first get all receipt keys:

```
KEYS receipts:{blockNum}:*
```

Example:

```
KEYS receipts:15000000:*
```

Response:

```
1) "receipts:15000000:0"
2) "receipts:15000000:1"
3) "receipts:15000000:2"
...
```

Then get each receipt:

```
GET receipts:15000000:0
GET receipts:15000000:1
...
```

Each response contains RLP-encoded receipt data.

### Contract Code

To get contract code:

```
GET code:{codeHash}
```

Example:

```
GET code:0xabcd1234...
```

Response: raw binary code

### Latest Block Number

To get the latest processed block number:

```
GET currentBlock
```

### Metadata About an Address's History

To get information about when an address's state has changed:

```
BSSL.INFO account:{address}
```

Example:

```
BSSL.INFO account:0x1f9840a85d5aF5bf1D1762F925BDADdC4201F984
```

Response:

```
1) (integer) 12000000  # First block where state exists
2) (integer) 15000000  # Last block where state changed
3) (integer) 27        # Number of state changes
```

## Understanding State Queries

When you query for state at a block, the system returns the state as it was at that block. If the state didn't change at exactly that block, it returns the most recent state change before or at the requested block.

For example:

- Account changes at block 100
- Account changes at block 500
- Account changes at block 1000

If you query for block 750, you'll get the state from block 500, since that was the most recent change before block 750.

## Understanding Reorgs

Chain reorganizations (reorgs) happen when the blockchain forks and a non-canonical chain becomes canonical. The Redis integration handles this by:

1. Detecting the reorg point (where the chain diverged)
2. Deleting all block data, transactions, and receipts from the non-canonical chain
3. Restoring state at the reorg point to match what it was before the reorg
4. Updating Redis to reflect the new canonical chain

This ensures that historical queries return accurate results even after reorgs.

## Go API Examples

Here are examples of querying data using the Go API:

### Get Account at Block

```go
// Get account state at block 1,000,000
accountState, err := redisState.GetAccountAtBlock(
    common.HexToAddress("0x1f9840a85d5aF5bf1D1762F925BDADdC4201F984"),
    1000000,
)
if err != nil {
    log.Fatal("Error getting account:", err)
}
fmt.Printf("Balance at block 1M: %s\n", accountState.Balance)
fmt.Printf("Nonce at block 1M: %d\n", accountState.Nonce)
```

### Get Storage at Block

```go
// Get storage value at block 1,000,000
storageValue, err := redisState.GetStorageAtBlock(
    common.HexToAddress("0x1f9840a85d5aF5bf1D1762F925BDADdC4201F984"),
    common.HexToHash("0x8f0162f3d08f8d0712b4c2b179f86fce4e93a879b8b7d140aa82eb91cf66215e"),
    1000000,
)
if err != nil {
    log.Fatal("Error getting storage:", err)
}
fmt.Printf("Storage value at block 1M: %s\n", storageValue)
```

### Get Block Data

```go
// Get block information
header, err := redisState.GetBlockByNumber(1000000)
if err != nil {
    log.Fatal("Error getting block:", err)
}
fmt.Printf("Block hash: %s\n", header.Hash().Hex())
fmt.Printf("Parent hash: %s\n", header.ParentHash.Hex())
fmt.Printf("State root: %s\n", header.Root.Hex())
fmt.Printf("Timestamp: %d\n", header.Time)
```

### Get Transactions

```go
// Get all transactions in a block
txs, err := redisState.GetTransactionsByBlockNumber(1000000)
if err != nil {
    log.Fatal("Error getting transactions:", err)
}
for i, tx := range txs {
    fmt.Printf("Transaction %d hash: %s\n", i, tx.Hash().Hex())
}
```

### Get Transaction Hashes

```go
// Get all transaction hashes in a block
txHashes, err := redisState.GetTransactionHashesByBlockNumber(1000000)
if err != nil {
    log.Fatal("Error getting transaction hashes:", err)
}
for i, hash := range txHashes {
    fmt.Printf("Transaction %d hash: %s\n", i, hash)
}
```

## Best Practices

1. **Use BSSL Commands**: Always use the BSSL module commands for state queries rather than trying to access internal keys directly.

2. **Batch Operations**: For bulk operations, use Redis pipelines to reduce round-trip time.

3. **Handle Null Results**: State might not exist at a particular block, so always check for null/nil results.

4. **Timeout Handling**: Set appropriate timeouts for Redis operations to prevent hanging during network issues.

5. **Error Recovery**: The integration includes automatic pipeline recovery for errors, but your application should handle Redis errors gracefully.

## Troubleshooting

### Common Issues

1. **No Data Found**: Make sure you're querying at or after the block where the state was created. If you query before the first state change, you'll get a null result.

2. **Inconsistent State**: After a reorg, there might be a brief period where data is being updated. If you encounter inconsistencies, try again after a few seconds.

3. **Redis Connection Issues**: Check Redis connection settings and network connectivity. The integration includes automatic reconnection, but persistent connectivity problems will affect functionality.

4. **Performance Degradation**: If you're experiencing slow queries, check Redis memory usage and consider increasing the available memory.

## Monitoring and Diagnostics

You can check the status of the Redis integration using:

1. **Check BSSL Module**: Make sure the BSSL module is loaded

   ```
   BSSL.PING
   ```

   Should return "PONG"

2. **Check Current Block**: See the latest processed block

   ```
   GET currentBlock
   ```

3. **Check Account Info**: Get metadata about an address
   ```
   BSSL.INFO account:0x1f9840a85d5aF5bf1D1762F925BDADdC4201F984
   ```
