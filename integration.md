# Redis State Integration for Erigon

This document explains how the Redis state integration works in Erigon. This integration adds the ability to shadow Erigon's state to Redis while maintaining all the original behavior.

## Architecture

The Redis integration follows a "shadowing" approach - it doesn't replace or modify core Erigon functionality but runs alongside it, capturing state changes as they happen and mirroring them to Redis.

### Key Components

1. **RedisState** (`redis_state.go`): Core Redis client management and state storage.
2. **RedisStateMonitor** (`redis_state_v3.go`): Monitors block, transaction, and receipt data.
3. **Direct State Writing Integration**: State writing methods directly write to Redis when enabled.

## How It Works

### Command Line Flags

Two command line flags control the Redis integration:
- `--redis.url`: Redis server URL (e.g., `redis://localhost:6379/0`)
- `--redis.password`: Redis server password

### Direct State Integration

The Redis integration directly integrates with Erigon's state writing methods:

```go
// In StateWriterV3.UpdateAccountData
func (w *StateWriterV3) UpdateAccountData(address common.Address, original, account *accounts.Account) error {
    // Standard Erigon operations
    
    // Write to Redis if enabled
    redisState := GetRedisState()
    if redisState.Enabled() && w.rs.domains != nil {
        redisState.writeAccount(w.rs.domains.BlockNum(), w.rs.domains.BlockHash(), address, account)
    }
    
    // Proceed with normal operations
    // ...
}
```

This approach minimizes code changes while ensuring all state changes are captured.

### Block Data Shadowing

Block, transaction, and receipt data is shadowed by monitoring key execution points:

1. **Block Processing**: During block commitment in `flushAndCheckCommitmentV3`
2. **Transaction Processing**: When transactions are added to tasks for execution
3. **Receipt Processing**: After transactions are executed and receipts are created

### State Change Shadowing

State changes (accounts, storage, code) are captured directly in the state writer methods:

1. **Account Updates**: In `StateWriterV3.UpdateAccountData`
2. **Storage Changes**: In `StateWriterV3.WriteAccountStorage`
3. **Code Updates**: In `StateWriterV3.UpdateAccountCode`
4. **Account Deletions**: In `StateWriterV3.DeleteAccount`

Notes: 
- When capturing state changes directly in the state writer, we don't have access to the block hash. We use a zero hash in these cases, which is acceptable since the primary lookup is by block number.
- The Redis pipeline is explicitly flushed when a block is committed in `flushAndCheckCommitmentV3`, ensuring Redis is always in sync with the client's state.

## Usage

To enable Redis state integration, simply start Erigon with:

```
./build/bin/erigon --redis.url="redis://your-redis-server:6379/0" --redis.password="your-password"
```

All blockchain state changes will now be written to Redis alongside normal operation.

## Data Organization

The Redis integration stores data with the following key patterns:

### Blocks
- `block:{blockNum}` - Hash containing block metadata
- `blockHash:{blockHash}` - Hash mapping block hash to block number and timestamp
- `canonicalChain` - Sorted set of block hashes with block numbers as scores

### Transactions
- `txs:{blockNum}` - Hash where keys are tx indices and values are RLP-encoded transactions
- `block:{blockNum}:txs` - Set of transaction hashes in the block

### Receipts
- `receipts:{blockNum}` - Hash where keys are tx indices and values are RLP-encoded receipts
- `block:{blockNum}:receipts` - Sorted set of transaction hashes with indices as scores

### Accounts
- `account:{address}` - Sorted set with block numbers as scores and account data as values
- Account data is stored as JSON with balance, nonce, codeHash, and incarnation fields

### Storage
- `storage:{address}:{slot}` - Sorted set with block numbers as scores and storage values

### Code
- `code:{codeHash}` - Code bytes stored by hash (immutable)

## Query Examples

Here are some examples of how to query data from Redis:

### Get block data
```
HGETALL block:12345
```

### Get all transactions in a block
```
HGETALL txs:12345
```

### Get all receipts in a block
```
HGETALL receipts:12345
```

### Get account state at a specific block
```
ZREVRANGEBYSCORE account:0x1234...abcd 12345 0 LIMIT 0 1 WITHSCORES
```

### Get storage value at a specific block
```
ZREVRANGEBYSCORE storage:0x1234...abcd:0x5678...efab 12345 0 LIMIT 0 1 WITHSCORES
```

### Get contract code
```
GET code:0xcodeHash
```

## Technical Decisions

1. **Block-Indexed Storage**: Transactions and receipts are indexed primarily by block number for better retrieval performance.
2. **No Log Storage**: Logs are not directly stored in Redis to reduce storage requirements. They can be retrieved from the receipts.
3. **Shadow vs. Replace**: The implementation shadows state rather than replacing it, ensuring no impact on core functionality.
4. **Error Handling**: Redis errors are logged but don't interrupt normal Erigon operation.
5. **Complete Deletion on Reorganization**: Non-canonical data is deleted rather than marked, ensuring O(1) query complexity.

## Performance Considerations

- The Redis integration uses pipelines to batch write operations for better performance
- During chain reorganizations, data from non-canonical blocks is completely deleted
- All queries have O(1) complexity regardless of chain history length

For more technical details on the implementation, see the developer documentation in `core/state/redis_state.md`.

## Implementation Details

### Modified Files

The following files were modified to implement the Redis state integration:

1. **`core/state/redis_state.go`**
   - Removed log storage functionality
   - Modified transaction storage to index by block number using `txs:{blockNum}` keys
   - Modified receipt storage to index by block number using `receipts:{blockNum}` keys
   - Simplified transaction and receipt hash-to-block mapping
   - Updated reorganization handling to properly clean up block-indexed data

2. **`core/state/redis_state_v3.go`**
   - No significant changes (monitor functionality remains the same)

3. **`core/state/redis_writer.go`**
   - No significant changes (writer functionality remains the same)

4. **`core/state/rw_v3.go`**
   - Added integration points for Redis state updates in state writer methods
   - Modified to send account, storage, and code changes to Redis

5. **`eth/stagedsync/exec3.go`**
   - Added Redis pipeline flushing during block commit
   - Ensures all buffered Redis operations are committed after block execution

6. **`eth/stagedsync/exec3_serial.go`**
   - Added Redis flush operations for serial execution

7. **`eth/backend.go`**
   - Added Redis initialization from config params
   - Ensures Redis connection is established at startup

8. **`cmd/utils/flags.go`**
   - Added Redis URL and password flags 
   - Added flag handling in SetEthConfig

9. **`eth/ethconfig/config.go`**
   - Added Redis URL and password config fields
   - Ensures Redis connection params are passed through the system

10. **`turbo/cli/default_flags.go`**
   - Added Redis URL and password flags to the default flags list
   - Ensures Redis options are available in the CLI interface

11. **`core/state/redis_state.md`**
   - Updated to reflect the new data organization
   - Removed log-related documentation
   - Added code retrieval documentation
   - Updated query patterns for block-indexed transactions and receipts

12. **`core/state/redis_query.md`**
   - Complete rewrite to match the new implementation
   - Updated all examples to use correct Redis commands
   - Removed log-related sections
   - Added code retrieval documentation

13. **`integration.md`**
   - Updated to include information about the modified implementation
   - Added examples for the new block-indexed data access patterns

### Key Changes

1. **Transaction Storage**
   - Transactions are stored as entries in `txs:{blockNum}` hash
   - Transaction lookup is only available by block number and index
   - Removed `tx:{txHash}:block` and `sender:{address}:txs` mappings

2. **Receipt Storage**
   - Receipts are stored as entries in `receipts:{blockNum}` hash
   - Receipt lookup is only available by block number and index
   - Removed `receipt:{txHash}:block` mapping

3. **Log Storage**
   - Logs are not stored separately (they can be retrieved from receipts if needed)

4. **Data Access**
   - All historical queries remain O(1) complexity
   - Block-based queries are now more efficient
   - Reduced storage requirements by eliminating transaction hash and sender mappings
   - Reduced storage requirements by eliminating receipt hash mappings