# Redis State Integration for Erigon

This document explains how the Redis state integration was implemented in Erigon. This integration adds the ability to shadow Erigon's state to Redis while maintaining all the original behavior.

## Architecture

The Redis integration follows a "shadowing" approach - it doesn't replace or modify core Erigon functionality but runs alongside it, capturing state changes as they happen and mirroring them to Redis.

### Key Components

1. **RedisState** (`redis_state.go`): Core Redis client management and state storage.
2. **RedisStateMonitor** (`redis_state_v3.go`): Monitors block, transaction, and receipt data.
3. **Direct State Writing Integration**: State writing methods directly write to Redis when enabled.

## How It Works

### Command Line Flags

Two new command line flags were added:
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

### Connection Management

Redis connection is established at Erigon startup and gracefully closed on shutdown:

1. **Initialization**: In `Ethereum.Init()` after other systems are initialized
2. **Graceful Shutdown**: In `Ethereum.Stop()` with proper error handling

## Usage

To enable Redis state integration, simply start Erigon with:

```
./build/bin/erigon --redis.url="redis://your-redis-server:6379/0" --redis.password="your-password"
```

All blockchain state changes will now be written to Redis alongside normal operation.

## Technical Decisions

1. **Shadow vs. Replace**: The implementation shadows state rather than replacing it, ensuring no impact on core functionality.
2. **Direct Integration**: State writing methods directly write to Redis when enabled, minimizing code changes.
3. **Monitor Pattern**: A monitor pattern is used to capture block and transaction data at key points.
4. **Error Handling**: Redis errors are logged but don't interrupt normal Erigon operation.

## Data Structure

Redis data follows the structure defined in `redis_state.md`:

1. Account data in sorted sets with block numbers as scores
2. Storage data in sorted sets with block numbers as scores
3. Transaction and receipt data keyed by hash
4. Code stored by code hash
5. Logs indexed by address and topic