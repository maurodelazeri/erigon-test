# Redis State Integration Hook Instructions

This document explains how account state, storage, and code are captured and written to Redis in Erigon.

## How State Changes Flow in Erigon

State changes in Erigon follow this path:

1. **EVM execution** changes state through `IntraBlockState.SetState()`, `AddBalance()`, etc.
2. `IntraBlockState.FinalizeTx()` is called after each transaction
3. This calls `updateAccount()` on all modified accounts
4. `stateObject.updateTrie(stateWriter)` is called to write storage changes 
5. This calls `stateWriter.WriteAccountStorage()` for each storage slot
6. Our wrapped state writer interceptor captures this call and mirrors to Redis

## Current Implementation Status

The Redis state integration has already been implemented correctly in the codebase:

```go
// In core/state_processor.go (line ~42)
// REDIS INTEGRATION: Wrap the stateWriter with our registered wrappers
stateWriter = state.WrapStateWriter(stateWriter, header.Number.Uint64())
```

This line wraps the state writer with any registered state writer wrappers, including our Redis integration.

## Debugging Redis State Integration

If account state or storage is not being properly written to Redis, add these debug steps:

### 1. Add debugging to `stateObject.updateTrie()`

In `core/state/state_object.go`, around line 255:

```go
func (so *stateObject) updateTrie(stateWriter StateWriter) error {
    fmt.Printf("\n\n!!! UPDATING TRIE FOR %s, DIRTY STORAGE COUNT: %d !!!\n\n", 
        so.address.Hex(), len(so.dirtyStorage))
    
    for key, value := range so.dirtyStorage {
        original := so.blockOriginStorage[key]
        so.originStorage[key] = value
        
        fmt.Printf("\n!!! WRITING STORAGE: %s, key: %s, value: %s !!!\n",
            so.address.Hex(), key.Hex(), value.String())
            
        if err := stateWriter.WriteAccountStorage(so.address, so.data.GetIncarnation(), &key, &original, &value); err != nil {
            return err
        }
    }
    return nil
}
```

### 2. Add debugging to `RedisStateWriter.WriteAccountStorage()`

In `redis-state/redis_state.go`:

```go
func (w *RedisStateWriter) WriteAccountStorage(address libcommon.Address, incarnation uint64, key *libcommon.Hash, original, value *uint256.Int) error {
    // Add aggressive logging
    fmt.Printf("\n\n!!! REDIS WRITER: STORING %s -> %s = %s !!!\n\n", 
        address.Hex(), key.Hex(), value.String())
    
    // Rest of your implementation...
}
```

### 3. Make sure hooks are registered early

The Redis integration registers hooks via `RegisterAllHooks()` in `redis_hooks.go`. This must be called early in the initialization process, before any transactions are processed:

```go
// RegisterAllHooks registers all Redis hooks to capture data from Erigon
func RegisterAllHooks(integration *RedisIntegration) {
    if integration == nil || !integration.IsEnabled() {
        return
    }
    
    // Register the state writer hook
    state.RegisterStateWriterWrapper(integration.WrapStateWriter)
    
    // Register block, header, and transaction hooks
    rawdb.RegisterBlockWriteHook(integration.BlockWriteHook)
    rawdb.RegisterHeaderWriteHook(integration.HeaderWriteHook)
    rawdb.RegisterTransactionWriteHook(integration.TransactionWriteHook)
}
```

### 4. Test with a contract that changes storage

To test storage changes, deploy and interact with a simple contract that changes storage:

```solidity
// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;
contract SimpleStorage {
    uint256 public storedData;
    
    function set(uint256 x) public {
        storedData = x;
    }
    
    function get() public view returns (uint256) {
        return storedData;
    }
}
```

## Important Files and Functions

The key components for the Redis state integration are:

1. **Registration**: `redis-state/redis_hooks.go` - Registers hooks for state changes
2. **Interception**: `redis-state/integration.go` - `StateInterceptor` class intercepts state changes
3. **Storage**: `redis-state/redis_state.go` - Handles writing to Redis
4. **State Flow**: `core/state/state_object.go` - Contains `updateTrie()` that writes storage changes
5. **Transaction Flow**: `core/state_processor.go` - Wraps state writer during transaction processing

## Running Erigon with Redis Integration

Make sure to run Erigon with the Redis integration enabled:

```bash
./build/bin/erigon --redis.enabled --redis.url="redis://localhost:6379/0"
```

## Verifying Integration

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

## Block Execution Flow

The complete state change flow during block execution:

1. `applyTransaction()` in `core/state_processor.go`
2. `stateWriter` is wrapped with registered wrappers, including Redis
3. `evm.Reset()` prepares the EVM for execution
4. `ApplyMessage()` executes the transaction
5. `ibs.FinalizeTx()` finalizes the transaction state
6. `ibs.UpdateTrie()` writes state changes
7. `stateObject.updateTrie()` processes storage changes
8. `stateWriter.WriteAccountStorage()` is called for each storage change
9. Our `StateInterceptor` captures this call and mirrors to Redis