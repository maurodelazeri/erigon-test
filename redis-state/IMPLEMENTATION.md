# Redis State Integration - Implementation Details

This document explains the implementation details of the Redis state integration for Erigon, focusing on how block, transaction, receipt, account, and state data are captured and stored.

## Files Changed

1. `/redis-state/redis_state.go` 
   - Added `HandleTransaction` and `StoreReceipt` methods to store transaction and receipt data in Redis
   - Enhanced storage pattern for transaction data using sorted sets

2. `/redis-state/state_provider.go`
   - Added `GetTransactionByHash` method to retrieve transactions
   - Added `GetTransactionReceipt` method to retrieve receipts
   - Added `GetLogs` method for event log filtering and retrieval
   - Implemented helper methods for log filtering and topic matching

3. `/redis-state/factory.go`
   - Contains chain reorganization handling for maintaining canonical chain

4. `/core/state/intra_block_state.go`
   - Added `currentBlock` field to track block context during execution
   - Modified state update methods to track block numbers

5. `/core/state/state_object.go`
   - Added `blockNum` field to track when state objects were modified
   - This enables proper block association for state changes

## Data Capture Architecture

### Block Context Tracking

1. **Initialization**: 
   - When a new block is being processed, a `RedisHistoricalWriter` is created with the block number
   - The block context flows through the execution pipeline

2. **State Object Tracking**:
   - As accounts and storage slots are modified, they record the current block number
   - This allows precise tracking of when each change occurred

3. **Transaction Flow**:
   - Transactions are captured with `HandleTransaction` after execution
   - Both transaction data and receipts are stored with proper indices

### Redis Data Structures

#### Account State

Accounts are stored as JSON in sorted sets:
```
Key: account:{address}
Score: blockNumber
Value: {JSON serialized account data}
```

Example:
```
account:0x742d35Cc6634C0532925a3b844Bc454e4438f44e -> {
  Score: 1500000,
  Value: {"nonce":5,"balance":"0x1a055690d9db80000","codeHash":"0x0","incarnation":0}
}
```

#### Storage Slots

Storage slots use a similar pattern:
```
Key: storage:{address}:{storageKey}
Score: blockNumber
Value: {binary value}
```

#### Code Storage

Contract code is stored immutably by hash:
```
Key: code:{codeHash}
Value: {binary code}
```

#### Transaction Data

Transactions are stored with block association:
```
Key: tx:{txHash}
Score: blockNumber
Value: {JSON serialized transaction}
```

Block-to-transaction mapping:
```
Key: block:{blockNumber}:txs
Value: Set of transaction hashes
```

Sender-to-transaction mapping:
```
Key: sender:{address}:txs
Score: blockNumber
Value: {transaction hash}
```

#### Receipt Data

Receipts are stored with block association:
```
Key: receipt:{txHash}
Score: blockNumber
Value: {JSON serialized receipt}
```

Block receipts index:
```
Key: block:{blockNumber}:receipts
Score: transactionIndex
Value: {transaction hash}
```

#### Event Log Indexing

Logs are indexed multiple ways for efficient filtering:

```
Key: log:{logIndex}
Value: {JSON serialized log}

Key: topic:{topicHash}
Score: blockNumber
Value: {logIndex}

Key: address:{address}:logs
Score: blockNumber
Value: {logIndex}
```

## Chain Reorganization Handling

When a chain reorganization occurs:

1. `HandleChainReorg` is called in `factory.go`
2. Special writers are created for each reorged block with `txNum=0` 
3. These blocks are marked with `reorg:{blockNum}` keys
4. The canonical chain is updated in the `canonicalChain` sorted set
5. Block hashes are marked as canonical or non-canonical

For state access after reorgs, the implementation:
- Checks if a block is canonical when retrieving data
- Warns when accessing non-canonical data
- Still provides access to reorged data when specifically requested

## Integration Points

The Redis state integration hooks into several key points in Erigon:

1. **Block Processing**: 
   - As blocks are processed, state changes are recorded

2. **Transaction Execution**:
   - After transactions execute, results are written to Redis

3. **State Modification**:
   - Account and storage changes are tracked through interfaces

4. **Chain Management**:
   - Canonical chain tracking keeps Redis state in sync with the actual chain

## Performance Considerations

1. **Efficient State Access**:
   - O(1) access to any historical state using sorted sets

2. **Minimal Storage**:
   - Only state changes are stored, not redundant data

3. **Selective Serialization**:
   - Only essential data is serialized to JSON
   - Binary storage is used where appropriate

4. **Indexing Optimizations**:
   - Multiple indices enable fast lookups by various criteria
   - Log filtering is optimized for common query patterns

## Usage Examples

### Getting State at Specific Block

```go
reader := redisstate.NewRedisStateReaderAtBlock(client, 10000000)
account, err := reader.ReadAccountData(address)
```

### Querying Transactions and Receipts

```go
provider := redisstate.NewRedisStateProvider(client, logger)
tx, blockHash, blockNum, txIndex, err := provider.GetTransactionByHash(ctx, txHash)
receipt, err := provider.GetTransactionReceipt(ctx, txHash)
```

### Filtering Logs

```go
filter := &types.LogFilter{
    FromBlock: rpc.BlockNumber(10000000),
    ToBlock:   rpc.BlockNumber(10001000),
    Addresses: []common.Address{contractAddress},
    Topics:    [][]common.Hash{{eventSignature}},
}
logs, err := provider.GetLogs(ctx, filter)
```