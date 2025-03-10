# Redis Storage and Query Guide for Erigon Blockchain Data

This comprehensive documentation explains how Erigon blockchain data is stored in Redis and how to effectively query it.

## Table of Contents

1. [Data Storage Patterns](#data-storage-patterns)
2. [Query Patterns](#query-patterns)
3. [Historical Data Access](#historical-data-access)
4. [Performance Considerations](#performance-considerations)
5. [Common Use Cases](#common-use-cases)

## Data Storage Patterns

Erigon uses Redis to shadow blockchain data using consistent key patterns and appropriate data structures for each data type.

### Account State

- **Key Pattern**: `account:{address}`
- **Data Structure**: Sorted Set (ZSET)
- **Score**: Block number
- **Member**: JSON-encoded account data with fields: balance, nonce, codeHash, incarnation
- **Example**:
  ```
  > ZRANGE "account:0x1e141E00dB452a539e0E860292647E9337562Fc3" 0 -1 WITHSCORES
  {"balance":"69402016491624854","nonce":10,"codeHash":"0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470","incarnation":0}
  22013251
  ```

### Storage State

- **Key Pattern**: `storage:{address}:{storageKey}`
- **Data Structure**: Sorted Set (ZSET)
- **Score**: Block number
- **Member**: Hex-encoded storage value or "null" for zero values
- **Example**:
  ```
  > ZRANGE "storage:0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48:0x044c11cf8dbe1ae4f0b73436e762d4dee488bbfa02cc516bd88c195a54b91959" 0 -1 WITHSCORES
  0x0x1f3a48cd9
  22013109
  null
  22013203
  0x0x47ce
  22013224
  ```

### Contract Code

- **Key Pattern**: `code:{codeHash}`
- **Data Structure**: String
- **Value**: Raw bytecode
- **Example**:
  ```
  > GET code:0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470
  [code binary data]
  ```

### Blocks

- **Key Pattern**: `block:{blockNum}`
- **Data Structure**: Hash
- **Fields**: hash, parentHash, stateRoot, timestamp, number
- **Example**:
  ```
  > HGETALL block:22013269
  hash
  0xeccd7217e60a5e077858708864a622ebdf2bf50431e872fe55cc79cfa022b803
  parentHash
  0x20d54746c3a1e6866c8673ed40f6f83b8f3657b39f8abc56c402e31309ec43a7
  stateRoot
  0x3f3d951d13f2c7c0afe1fd87cba0a3c27d65ba0da2b2cb21f281458f8eadf03c
  timestamp
  1715693287
  number
  22013269
  ```

### Block Hash to Number Mapping

- **Key Pattern**: `blockHash:{blockHash}`
- **Data Structure**: Hash
- **Fields**: number, timestamp
- **Example**:
  ```
  > HGETALL blockHash:0xeccd7217e60a5e077858708864a622ebdf2bf50431e872fe55cc79cfa022b803
  number
  22013269
  timestamp
  1715693287
  ```

### Canonical Chain

- **Key Pattern**: `canonicalChain`
- **Data Structure**: Sorted Set (ZSET)
- **Score**: Block number
- **Member**: Block hash
- **Example**:
  ```
  > ZRANGE canonicalChain 22013269 22013269 WITHSCORES
  0xeccd7217e60a5e077858708864a622ebdf2bf50431e872fe55cc79cfa022b803
  22013269
  ```

### Transactions by Block

- **Key Pattern**: `txs:{blockNum}`
- **Data Structure**: Hash
- **Keys**: Transaction indices (as strings)
- **Values**: RLP-encoded transaction data
- **Example**:
  ```
  > HGETALL txs:22013269
  0
  [RLP encoded tx data]
  1
  [RLP encoded tx data]
  ...
  ```

### Transaction Hash to Block Mapping

- **Key Pattern**: `tx:{txHash}:block`
- **Data Structure**: String
- **Value**: Block number
- **Example**:
  ```
  > GET tx:0x5315704d3d739427b17da289510ca7d52f20644d48646f2a9f6b6f50bca254d5:block
  22013269
  ```

### Block's Transaction Hashes

- **Key Pattern**: `block:{blockNum}:txs`
- **Data Structure**: Set
- **Members**: Transaction hashes
- **Example**:
  ```
  > SMEMBERS block:22013269:txs
  0x09a2af891be42b5014124c7ac6fb9f944e63cd8f9ffd8ce48803a30fe1f6425f
  0x6cd2d7f978e1b28e49a8101d630d729d37709c0ae03b495661ce61c463947d33
  [...]
  ```

### Receipts by Block

- **Key Pattern**: `receipts:{blockNum}`
- **Data Structure**: Hash
- **Keys**: Transaction indices (as strings)
- **Values**: RLP-encoded receipt data
- **Example**:
  ```
  > HGETALL receipts:22013269
  0
  [RLP encoded receipt data]
  1
  [RLP encoded receipt data]
  ...
  ```

### Receipt Hash to Block Mapping

- **Key Pattern**: `receipt:{txHash}:block`
- **Data Structure**: String
- **Value**: Block number
- **Example**:
  ```
  > GET receipt:0x5315704d3d739427b17da289510ca7d52f20644d48646f2a9f6b6f50bca254d5:block
  22013269
  ```

### Block's Receipt Indices

- **Key Pattern**: `block:{blockNum}:receipts`
- **Data Structure**: Sorted Set (ZSET)
- **Score**: Transaction index
- **Member**: Transaction hash
- **Example**:
  ```
  > ZRANGE block:22013269:receipts 0 -1 WITHSCORES
  0x09a2af891be42b5014124c7ac6fb9f944e63cd8f9ffd8ce48803a30fe1f6425f
  0
  0x6cd2d7f978e1b28e49a8101d630d729d37709c0ae03b495661ce61c463947d33
  1
  [...]
  ```

### Sender Transactions

- **Key Pattern**: `sender:{address}:txs`
- **Data Structure**: Sorted Set (ZSET)
- **Score**: Block number
- **Member**: Transaction hash
- **Example**:
  ```
  > ZRANGE sender:0xE96dA1d4D34420Dec3eF41f0C4264B172B0666DC:txs 0 -1 WITHSCORES
  0x9351b234c3cec171780f9cef3bbbd84940038f5f918737827f8a3ee98e6987c2
  22013282
  ```

## Query Patterns

### Querying Account State at a Block

To find the account state at or before a specific block:

```
ZREVRANGEBYSCORE account:{address} {blockNum} 0 LIMIT 0 1 WITHSCORES
```

Example:

```
> ZREVRANGEBYSCORE account:0x1e141E00dB452a539e0E860292647E9337562Fc3 22013200 0 LIMIT 0 1 WITHSCORES
```

### Querying Storage Value at a Block

To find the storage value at or before a specific block:

```
ZREVRANGEBYSCORE storage:{address}:{storageKey} {blockNum} 0 LIMIT 0 1 WITHSCORES
```

Example:

```
> ZREVRANGEBYSCORE storage:0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48:0x044c11cf8dbe1ae4f0b73436e762d4dee488bbfa02cc516bd88c195a54b91959 22013200 0 LIMIT 0 1 WITHSCORES
```

### Getting Block Data

```
HGETALL block:{blockNum}
```

Example:

```
> HGETALL block:22013269
```

### Getting All Transactions in a Block

```
HGETALL txs:{blockNum}
```

Example:

```
> HGETALL txs:22013269
```

### Getting All Receipts in a Block

```
HGETALL receipts:{blockNum}
```

Example:

```
> HGETALL receipts:22013269
```

### Finding a Transaction's Block

```
GET tx:{txHash}:block
```

Example:

```
> GET tx:0x5315704d3d739427b17da289510ca7d52f20644d48646f2a9f6b6f50bca254d5:block
```

### Getting Transactions by Sender

```
ZRANGE sender:{address}:txs {startIndex} {endIndex} WITHSCORES
```

Example:

```
> ZRANGE sender:0xE96dA1d4D34420Dec3eF41f0C4264B172B0666DC:txs 0 -1 WITHSCORES
```

### Getting Contract Code

```
GET code:{codeHash}
```

Example:

```
> GET code:0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470
```

## Historical Data Access

Erigon's Redis implementation provides O(1) access to historical state by storing data in sorted sets with block numbers as scores. This allows efficient querying of state at any historical block.

### Key Concepts:

1. **Block Number as Score**: Most sorted sets use the block number as score
2. **Most Recent First**: Use ZREVRANGEBYSCORE to get the state at a specific block
3. **O(1) Complexity**: Regardless of how old the data is, queries have constant time complexity

### Getting the Value at a Specific Block:

The general pattern is:

```
ZREVRANGEBYSCORE {key} {blockNum} 0 LIMIT 0 1 WITHSCORES
```

This returns the most recent entry at or before the specified block number, with exactly O(1) complexity regardless of chain history length.

## Performance Considerations

1. **Optimal Historical Queries**: Use ZREVRANGEBYSCORE with LIMIT 0 1 for best performance
2. **Block-Indexed Access**: Access transactions and receipts by block number for efficient retrieval
3. **Redis Pipeline**: The implementation uses Redis pipelines for batched writes
4. **Storage Optimization**: Logs are not stored separately to reduce storage requirements

## Common Use Cases

### 1. Block Explorer Functions

**Get a block's data**

```
HGETALL block:{blockNum}
```

**Get all transactions in a block**

```
HGETALL txs:{blockNum}
```

**Get all receipts in a block**

```
HGETALL receipts:{blockNum}
```

**Find a transaction's block**

```
GET tx:{txHash}:block
```

### 2. Account/Contract Analysis

**Get account balance history**

```
ZRANGE account:{address} 0 -1 WITHSCORES
```

**Track storage value changes**

```
ZRANGE storage:{address}:{storageKey} 0 -1 WITHSCORES
```

**Get all transactions sent by an address**

```
ZRANGE sender:{address}:txs 0 -1 WITHSCORES
```

**Get contract code**

```
GET code:{codeHash}
```

### 3. Smart Contract Simulations

To simulate a contract at a specific block:

1. Get account state:

```
ZREVRANGEBYSCORE account:{contractAddress} {blockNum} 0 LIMIT 0 1
```

2. Get all relevant storage:

```
SCAN 0 MATCH storage:{contractAddress}:* COUNT 100
```

3. For each storage key found, get the value at that block:

```
ZREVRANGEBYSCORE {storageKey} {blockNum} 0 LIMIT 0 1
```

4. Get contract code:

```
GET code:{codeHash}
```
(where codeHash is obtained from the account data)