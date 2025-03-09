# Redis State Integration: Transactions and Receipts Guide

This document details how to work with transactions and receipts in the Redis state integration for Erigon.

## Overview

The Redis state integration now includes comprehensive support for storing and querying:
- Transactions 
- Transaction receipts
- Event logs
- Log indexing by topic and address

These features enable applications to efficiently access the full transaction history with O(1) lookups.

## Data Model for Transactions and Receipts

### Transaction Storage

Transactions are stored using several Redis data structures:

1. **Transaction by Hash**:
   ```
   Key: tx:{txHash}
   Type: Sorted Set
   Score: blockNumber
   Value: JSON serialized transaction
   ```

2. **Block to Transactions Mapping**:
   ```
   Key: block:{blockNumber}:txs
   Type: Set
   Value: Set of transaction hashes in the block
   ```

3. **Sender to Transactions Mapping**:
   ```
   Key: sender:{address}:txs
   Type: Sorted Set
   Score: blockNumber
   Value: Transaction hash
   ```

### Receipt Storage

Transaction receipts are stored in a similar pattern:

1. **Receipt by Transaction Hash**:
   ```
   Key: receipt:{txHash}
   Type: Sorted Set
   Score: blockNumber
   Value: JSON serialized receipt
   ```

2. **Block to Receipts Mapping**:
   ```
   Key: block:{blockNumber}:receipts
   Type: Sorted Set
   Score: transactionIndex
   Value: Transaction hash
   ```

### Event Log Indexing

Event logs from transaction receipts are indexed for efficient filtering:

1. **Log by Index**:
   ```
   Key: log:{logIndex}
   Type: String
   Value: JSON serialized log
   ```

2. **Logs by Topic**:
   ```
   Key: topic:{topicHash}
   Type: Sorted Set
   Score: blockNumber
   Value: logIndex
   ```

3. **Logs by Address**:
   ```
   Key: address:{address}:logs
   Type: Sorted Set
   Score: blockNumber
   Value: logIndex
   ```

## Using the API

### Retrieving a Transaction

```go
// Get a transaction by its hash
tx, blockHash, blockNum, txIndex, err := provider.GetTransactionByHash(ctx, txHash)
if err != nil {
    // Handle error
}

fmt.Printf("Transaction found in block %d at index %d\n", blockNum, txIndex)
fmt.Printf("From: %s\n", tx.From().Hex())
if to := tx.GetTo(); to != nil {
    fmt.Printf("To: %s\n", to.Hex())
} else {
    fmt.Println("To: Contract Creation")
}
fmt.Printf("Value: %s\n", tx.GetValue().String())
```

### Retrieving a Receipt

```go
// Get a receipt by transaction hash
receipt, err := provider.GetTransactionReceipt(ctx, txHash)
if err != nil {
    // Handle error
}

fmt.Printf("Status: %d\n", receipt.Status)
fmt.Printf("Gas Used: %d\n", receipt.GasUsed)
fmt.Printf("Logs: %d\n", len(receipt.Logs))

// If it was a contract creation, get the contract address
if receipt.ContractAddress != (common.Address{}) {
    fmt.Printf("Contract created: %s\n", receipt.ContractAddress.Hex())
}
```

### Filtering Logs

The log filtering API follows the Ethereum JSON-RPC standard, allowing for filtering by address, topics, and block range:

```go
// Create a filter for ERC-20 Transfer events from a specific token
transferSig := common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
filter := &types.LogFilter{
    FromBlock: rpc.BlockNumber(10000000),
    ToBlock:   rpc.BlockNumber(10001000),
    Addresses: []common.Address{tokenAddress},
    Topics:    [][]common.Hash{{transferSig}},
}

logs, err := provider.GetLogs(ctx, filter)
if err != nil {
    // Handle error
}

// Process the logs
for _, log := range logs {
    fmt.Printf("Log in block %d, tx index %d\n", log.BlockNumber, log.TxIndex)
    fmt.Printf("  From: %s\n", log.Topics[1].Hex())
    fmt.Printf("  To: %s\n", log.Topics[2].Hex())
    // Decode amount
    amount := new(big.Int).SetBytes(log.Data)
    fmt.Printf("  Amount: %s\n", amount.String())
}
```

## Chain Reorganization Handling

When a chain reorganization occurs, the transaction and receipt data is handled consistently with account state:

1. Reorged blocks are marked with `reorg:{blockNum}` keys
2. The canonical chain tracking is updated
3. Transactions and receipts remain accessible but are marked as non-canonical

You can check if a block was reorged via the provider:

```go
reorged, err := provider.CheckAndHandleReorganization(blockNum)
if err != nil {
    // Handle error
}

if reorged {
    fmt.Printf("Block %d was part of a chain reorganization\n", blockNum)
}
```

## Performance Considerations

1. **Transaction Retrieval**: O(1) complexity regardless of chain size
2. **Log Filtering by Address**: Efficient using pre-built indices
3. **Log Filtering by Topic**: Highly optimized for common queries
4. **High-Volume Event Handling**: Consider sharding for applications with many logs

## Example: Simple Transaction Explorer

This example demonstrates how to build a simple transaction explorer using the Redis state integration:

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/erigontech/erigon-lib/common"
    "github.com/erigontech/erigon/redis-state"
    "github.com/erigontech/erigon/rpc"
    "github.com/redis/go-redis/v9"
)

func main() {
    // Connect to Redis
    client := redis.NewClient(&redis.Options{
        Addr: "localhost:6379",
    })

    // Create a state provider
    provider := redisstate.NewRedisStateProvider(client, log.Default())
    ctx := context.Background()

    // Get latest block number
    latestBlock, err := provider.GetLatestBlockNumber(ctx)
    if err != nil {
        log.Fatalf("Error getting latest block: %v", err)
    }

    fmt.Printf("Latest block: %d\n", latestBlock)

    // Get block details
    blockData, err := provider.GetBlockByNumber(ctx, rpc.BlockNumber(latestBlock), true)
    if err != nil {
        log.Fatalf("Error getting block: %v", err)
    }

    fmt.Printf("Block hash: %s\n", blockData["hash"])
    fmt.Printf("Block time: %s\n", blockData["timestamp"])
    fmt.Printf("Gas used: %s\n", blockData["gasUsed"])

    // List transactions in the block
    transactions, ok := blockData["transactions"].([]interface{})
    if !ok {
        log.Fatalf("Error parsing transactions")
    }

    fmt.Printf("Block contains %d transactions\n", len(transactions))

    // Look at the first transaction in detail
    if len(transactions) > 0 {
        txMap, ok := transactions[0].(map[string]interface{})
        if !ok {
            log.Fatalf("Error parsing transaction")
        }

        txHash := common.HexToHash(txMap["hash"].(string))
        fmt.Printf("Transaction: %s\n", txHash.Hex())
        fmt.Printf("  From: %s\n", txMap["from"])
        fmt.Printf("  To: %s\n", txMap["to"])
        fmt.Printf("  Value: %s\n", txMap["value"])

        // Get the receipt
        receipt, err := provider.GetTransactionReceipt(ctx, txHash)
        if err != nil {
            log.Fatalf("Error getting receipt: %v", err)
        }

        fmt.Printf("  Status: %d\n", receipt.Status)
        fmt.Printf("  Gas Used: %d\n", receipt.GasUsed)
        fmt.Printf("  Logs: %d\n", len(receipt.Logs))
    }
}
```

## Conclusion

The transaction and receipt handling in the Redis state integration provides a powerful foundation for building blockchain applications that require fast access to historical transaction data. By leveraging Redis sorted sets and efficient indexing, the implementation delivers O(1) access to transaction history regardless of chain size.