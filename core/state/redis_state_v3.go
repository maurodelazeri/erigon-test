package state

import (
	"context"

	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/kv"
	"github.com/erigontech/erigon-lib/kv/dbutils"
	"github.com/erigontech/erigon-lib/log/v3"
	stateLib "github.com/erigontech/erigon-lib/state"
	"github.com/erigontech/erigon/turbo/shards"
)

// RedisStateV3 wraps StateV3 with Redis integration
type RedisStateV3 struct {
	delegate *StateV3 // Composition instead of embedding
	redis    *RedisState
}

// NewRedisStateV3 creates a new StateV3 with Redis integration
func NewRedisStateV3(domains *stateLib.SharedDomains, logger log.Logger) *RedisStateV3 {
	return &RedisStateV3{
		delegate: NewStateV3(domains, logger),
		redis:    GetRedisState(),
	}
}

// Forward all required methods to the delegate

// RegisterSender forwards to the delegate
func (rs *RedisStateV3) RegisterSender(txTask *TxTask) bool {
	return rs.delegate.RegisterSender(txTask)
}

// ReTry forwards to the delegate
func (rs *RedisStateV3) ReTry(txTask *TxTask, in *QueueWithRetry) {
	rs.delegate.ReTry(txTask, in)
}

// AddWork forwards to the delegate
func (rs *RedisStateV3) AddWork(ctx context.Context, txTask *TxTask, in *QueueWithRetry) {
	rs.delegate.AddWork(ctx, txTask, in)
}

// CommitTxNum forwards to the delegate
func (rs *RedisStateV3) CommitTxNum(sender *libcommon.Address, txNum uint64, in *QueueWithRetry) (count int) {
	return rs.delegate.CommitTxNum(sender, txNum, in)
}

// Domains forwards to the delegate
func (rs *RedisStateV3) Domains() *stateLib.SharedDomains {
	return rs.delegate.Domains()
}

// SetTxNum forwards to the delegate
func (rs *RedisStateV3) SetTxNum(txNum, blockNum uint64) {
	rs.delegate.SetTxNum(txNum, blockNum)
}

// Override methods that need Redis integration

// ApplyState4 applies state changes and updates Redis
func (rs *RedisStateV3) ApplyState4(ctx context.Context, txTask *TxTask) error {
	// Begin block processing in Redis
	rs.redis.beginBlockProcessing(txTask.BlockNum)

	// Process block data
	if txTask.Header != nil && txTask.Final {
		rs.redis.writeBlock(txTask.Header, txTask.BlockHash)
	}

	// Process transaction and receipt data
	if txTask.Tx != nil && txTask.TxIndex >= 0 {
		rs.redis.writeTx(txTask.BlockNum, txTask.BlockHash, txTask.Tx, txTask.TxIndex)

		// Write receipt if available
		if txTask.BlockReceipts != nil && txTask.TxIndex < len(txTask.BlockReceipts) && txTask.BlockReceipts[txTask.TxIndex] != nil {
			rs.redis.writeReceipt(txTask.BlockNum, txTask.BlockHash, txTask.BlockReceipts[txTask.TxIndex])
		}
	}

	// Apply state changes to underlying StateV3
	err := rs.delegate.ApplyState4(ctx, txTask)

	// Flush Redis pipeline periodically
	if txTask.TxIndex == 0 || txTask.Final {
		rs.redis.flushPipeline()
	}

	return err
}

// Unwind handles chain reorganization in both the DB and Redis
func (rs *RedisStateV3) Unwind(ctx context.Context, tx kv.RwTx, blockUnwindTo, txUnwindTo uint64, accumulator *shards.Accumulator, changeset *[kv.DomainLen][]stateLib.DomainEntryDiff) error {
	// First get the new canonical block hash (the one we're rewinding to)
	newCanonicalBlock, err := tx.GetOne(kv.HeaderCanonical, dbutils.EncodeBlockNumber(blockUnwindTo))
	if err != nil {
		rs.redis.logger.Error("Failed to get canonical block hash during unwind", "block", blockUnwindTo, "error", err)
		// Continue with unwind even if we can't get the hash
	}

	// Convert to hash
	var newCanonicalHash libcommon.Hash
	if len(newCanonicalBlock) > 0 {
		newCanonicalHash = libcommon.BytesToHash(newCanonicalBlock)
	}

	// Handle reorganization in Redis by completely deleting non-canonical data
	rs.redis.handleReorg(blockUnwindTo+1, newCanonicalHash)

	// Perform unwind in underlying StateV3
	return rs.delegate.Unwind(ctx, tx, blockUnwindTo, txUnwindTo, accumulator, changeset)
}

// Forward other methods to the delegate

// DoneCount forwards to the delegate
func (rs *RedisStateV3) DoneCount() uint64 {
	return rs.delegate.DoneCount()
}

// SizeEstimate forwards to the delegate
func (rs *RedisStateV3) SizeEstimate() (r uint64) {
	return rs.delegate.SizeEstimate()
}

// ReadsValid forwards to the delegate
func (rs *RedisStateV3) ReadsValid(readLists map[string]*stateLib.KvList) bool {
	return rs.delegate.ReadsValid(readLists)
}
