# Erigon State Management: Commit and Rollback Processes

This document explains how Erigon handles state changes, commits them during block execution, and how it performs rollbacks during blockchain reorganizations.

## 1. State Structure in Erigon

Erigon organizes blockchain state into distinct domains:

- **AccountsDomain**: Contains account data (balances, nonces, incarnation, code hash)
- **CodeDomain**: Stores contract bytecode
- **StorageDomain**: Manages contract storage slots (key-value pairs for each contract)

These domains are managed through a temporal database that associates each state change with a transaction number (`txNum`), allowing the system to query state at any specific point in history.

## 2. State Change Tracking During Block Execution

### IntraBlockState

The `IntraBlockState` structure is the central component for tracking changes during block execution:

```go
type IntraBlockState struct {
    stateReader StateReader
    stateObjects      map[libcommon.Address]*stateObject
    stateObjectsDirty map[libcommon.Address]struct{}
    nilAccounts map[libcommon.Address]struct{}
    journal        *journal
    // Other fields...
}
```

- **stateObjects**: Caches account data during execution
- **stateObjectsDirty**: Tracks which accounts have been modified
- **journal**: Records all state transitions for potential rollback

### State Modification Operations

State changes occur through various operations:

- **AddBalance/SubBalance**: Adjust account balances
- **SetCode**: Update contract code
- **SetState**: Modify contract storage values
- **CreateAccount**: Create new accounts
- **Selfdestruct**: Mark contracts for deletion

All these operations are journaled to allow for transaction-level rollbacks:

```go
func (so *stateObject) SetBalance(amount *uint256.Int, reason tracing.BalanceChangeReason) {
    sdb.journal.append(balanceChange{
        account: &so.address,
        prev:    so.data.Balance,
    })
    so.setBalance(amount)
}
```

## 3. State Commit Process

The commit process happens in two phases:

### Transaction Finalization

After each transaction, changes are finalized but not yet permanently committed:

```go
func (sdb *IntraBlockState) FinalizeTx(chainRules *chain.Rules, stateWriter StateWriter) error {
    // Process balance increases
    for addr, bi := range sdb.balanceInc {
        if !bi.transferred {
            sdb.getStateObject(addr)
        }
    }

    // Update state for modified accounts
    for addr := range sdb.journal.dirties {
        so, exist := sdb.stateObjects[addr]
        if !exist {
            continue
        }

        // Update the account in the database
        if err := updateAccount(chainRules.IsSpuriousDragon, chainRules.IsAura,
                               stateWriter, addr, so, true, sdb.tracingHooks); err != nil {
            return err
        }

        // Mark as dirty for block commitment
        sdb.stateObjectsDirty[addr] = struct{}{}
    }

    sdb.clearJournalAndRefund()
    return nil
}
```

### Block Commitment

At the end of a block, all changes are permanently committed:

```go
func (sdb *IntraBlockState) CommitBlock(chainRules *chain.Rules, stateWriter StateWriter) error {
    for addr, bi := range sdb.balanceInc {
        if !bi.transferred {
            sdb.getStateObject(addr)
        }
    }
    return sdb.MakeWriteSet(chainRules, stateWriter)
}

func (sdb *IntraBlockState) MakeWriteSet(chainRules *chain.Rules, stateWriter StateWriter) error {
    // Make sure all changes are included in the dirty set
    for addr := range sdb.journal.dirties {
        sdb.stateObjectsDirty[addr] = struct{}{}
    }

    // Apply all pending changes to the database
    for addr, stateObject := range sdb.stateObjects {
        _, isDirty := sdb.stateObjectsDirty[addr]
        if err := updateAccount(chainRules.IsSpuriousDragon, chainRules.IsAura,
                               stateWriter, addr, stateObject, isDirty, sdb.tracingHooks); err != nil {
            return err
        }
    }

    sdb.clearJournalAndRefund()
    return nil
}
```

### Writing to Storage

The actual persistence is handled by the `WriterV4` implementation:

```go
func (w *WriterV4) UpdateAccountData(address libcommon.Address, original, account *accounts.Account) error {
    value := accounts.SerialiseV3(account)
    return w.tx.DomainPut(kv.AccountsDomain, address.Bytes(), nil, value, nil, 0)
}

func (w *WriterV4) UpdateAccountCode(address libcommon.Address, incarnation uint64, codeHash libcommon.Hash, code []byte) error {
    return w.tx.DomainPut(kv.CodeDomain, address.Bytes(), nil, code, nil, 0)
}

func (w *WriterV4) WriteAccountStorage(address libcommon.Address, incarnation uint64, key *libcommon.Hash, original, value *uint256.Int) error {
    return w.tx.DomainPut(kv.StorageDomain, address.Bytes(), key.Bytes(), value.Bytes(), nil, 0)
}
```

## 4. State Rollback During Reorganizations

When a blockchain reorganization occurs, Erigon needs to:

1. Rollback state changes from discarded blocks
2. Apply state changes from the new canonical chain

### Unwind Process

The core unwind functionality is in `StateV3.Unwind`:

```go
func (rs *StateV3) Unwind(ctx context.Context, tx kv.RwTx, blockUnwindTo, txUnwindTo uint64,
                         accumulator *shards.Accumulator,
                         changeset *[kv.DomainLen][]state.DomainEntryDiff) error {
    // Track metrics for unwinding
    mxState3UnwindRunning.Inc()
    defer mxState3UnwindRunning.Dec()
    st := time.Now()
    defer mxState3Unwind.ObserveDuration(st)

    // Process state changes for reverting
    handle := func(k, v []byte, table etl.CurrentTableReader, next etl.LoadNextFunc) error {
        if len(k) == length.Addr {
            // Process account data
            if len(v) > 0 {
                var acc accounts.Account
                if err := accounts.DeserialiseV3(&acc, v); err != nil {
                    return fmt.Errorf("%w, %x", err, v)
                }
                var address common.Address
                copy(address[:], k)
                newV := accounts.SerialiseV3(&acc)
                if accumulator != nil {
                    accumulator.ChangeAccount(address, acc.Incarnation, newV)
                }
            } else {
                var address common.Address
                copy(address[:], k)
                if accumulator != nil {
                    accumulator.DeleteAccount(address)
                }
            }
            return nil
        }

        // Process storage data
        var address common.Address
        var location common.Hash
        copy(address[:], k[:length.Addr])
        copy(location[:], k[length.Addr:])
        if accumulator != nil {
            accumulator.ChangeStorage(address, currentInc, location, common.Copy(v))
        }
        return nil
    }

    // Collect changes that need to be rolled back
    stateChanges := etl.NewCollector("", "", etl.NewOldestEntryBuffer(etl.BufferOptimalSize), rs.logger)
    defer stateChanges.Close()
    stateChanges.SortAndFlushInBackground(true)

    // Process account changes
    accountDiffs := changeset[kv.AccountsDomain]
    for _, kv := range accountDiffs {
        if err := stateChanges.Collect(toBytesZeroCopy(kv.Key)[:length.Addr], kv.Value); err != nil {
            return err
        }
    }

    // Process storage changes
    storageDiffs := changeset[kv.StorageDomain]
    for _, kv := range storageDiffs {
        if err := stateChanges.Collect(toBytesZeroCopy(kv.Key), kv.Value); err != nil {
            return err
        }
    }

    // Apply all changes
    if err := stateChanges.Load(tx, "", handle, etl.TransformArgs{Quit: ctx.Done()}); err != nil {
        return err
    }

    // Unwind domains to previous state
    if err := rs.domains.Unwind(ctx, tx, blockUnwindTo, txUnwindTo, changeset); err != nil {
        return err
    }

    return nil
}
```

### Domain Unwind

The low-level unwinding uses the temporal database's capabilities to revert to a previous state based on transaction numbers:

```go
// SharedDomain.Unwind rolls back state to a previous block/transaction
func (sd *SharedDomains) Unwind(ctx context.Context, tx kv.RwTx, blockUnwindTo, txUnwindTo uint64,
                               changeset *[kv.DomainLen][]state.DomainEntryDiff) error {
    // For each domain (accounts, storage, code)...
    // Find all changes after the target tx number
    // Restore previous values
    // ...
    return nil
}
```

## 5. Key Design Principles

Erigon's state management is built on several core design principles:

1. **Temporal Database**: By associating each state change with a transaction number, the system can efficiently query or restore state at any point in history.

2. **Domain Separation**: Splitting state into separate domains (accounts, code, storage) allows for more efficient reads, writes, and unwinding.

3. **Journaling**: All state changes are recorded in a journal to allow for transaction-level rollbacks.

4. **Lazy Loading**: State objects are loaded from the database only when needed and cached in memory.

5. **Efficient Reorgs**: The temporal architecture enables efficient reorganizations by allowing the system to restore state to any previous point.

This design enables Erigon to process state changes efficiently during normal operation and to handle blockchain reorganizations with minimal overhead.
