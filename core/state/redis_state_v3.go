package state

import (
	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/kv"
	"github.com/erigontech/erigon-lib/kv/dbutils"
	"github.com/erigontech/erigon/core/types"
)

// RedisStateMonitor provides Redis integration but doesn't replace StateV3
// It's a supplementary component that monitors state changes
type RedisStateMonitor struct {
	redis *RedisState
}

// NewRedisStateMonitor creates a new monitor for Redis state integration
func NewRedisStateMonitor() *RedisStateMonitor {
	return &RedisStateMonitor{
		redis: GetRedisState(),
	}
}

// Methods for Redis state monitoring

// MonitorBlockProcessing records a new block being processed in Redis
func (rm *RedisStateMonitor) MonitorBlockProcessing(blockNum uint64) {
	if !rm.redis.Enabled() {
		return
	}
	rm.redis.beginBlockProcessing(blockNum)
}

// MonitorBlockData records block data in Redis
func (rm *RedisStateMonitor) MonitorBlockData(header *types.Header, blockHash libcommon.Hash) {
	if !rm.redis.Enabled() {
		return
	}
	rm.redis.writeBlock(header, blockHash)
}

// MonitorTransaction records transaction data in Redis
func (rm *RedisStateMonitor) MonitorTransaction(blockNum uint64, blockHash libcommon.Hash, tx types.Transaction, txIndex int) {
	if !rm.redis.Enabled() {
		return
	}
	rm.redis.writeTx(blockNum, blockHash, tx, txIndex)
}

// MonitorReceipt records receipt data in Redis
func (rm *RedisStateMonitor) MonitorReceipt(blockNum uint64, blockHash libcommon.Hash, receipt *types.Receipt) {
	if !rm.redis.Enabled() {
		return
	}
	rm.redis.writeReceipt(blockNum, blockHash, receipt)
}

// FlushData ensures all data is written to Redis
func (rm *RedisStateMonitor) FlushData() {
	if !rm.redis.Enabled() {
		return
	}
	rm.redis.FlushPipeline()
}

// MonitorUnwind handles Redis data cleanup during chain reorganization
func (rm *RedisStateMonitor) MonitorUnwind(tx kv.RwTx, blockUnwindTo uint64) error {
	if !rm.redis.Enabled() {
		return nil
	}

	// First get the new canonical block hash (the one we're rewinding to)
	newCanonicalBlock, err := tx.GetOne(kv.HeaderCanonical, dbutils.EncodeBlockNumber(blockUnwindTo))
	if err != nil {
		rm.redis.logger.Error("Failed to get canonical block hash during unwind", "block", blockUnwindTo, "error", err)
		return err
	}

	// Convert to hash
	var newCanonicalHash libcommon.Hash
	if len(newCanonicalBlock) > 0 {
		newCanonicalHash = libcommon.BytesToHash(newCanonicalBlock)
	}

	// Handle reorganization in Redis
	rm.redis.handleReorg(blockUnwindTo+1, newCanonicalHash)

	return nil
}

// All other methods are automatically provided by the embedded StateV3
