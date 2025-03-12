package state

import (
	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/kv"
	"github.com/erigontech/erigon-lib/types/accounts"
	"github.com/holiman/uint256"
)

// StateWriterWithRedis wraps a StateWriter with Redis integration
type StateWriterWithRedis struct {
	underlying StateWriter
	redis      *RedisState
	blockNum   uint64
	blockHash  libcommon.Hash // Track current block hash for proper chain identification
}

// NewStateWriterWithRedis creates a new StateWriter with Redis integration
func NewStateWriterWithRedis(underlying StateWriter, blockNum uint64, blockHash libcommon.Hash) *StateWriterWithRedis {
	return &StateWriterWithRedis{
		underlying: underlying,
		redis:      GetRedisState(),
		blockNum:   blockNum,
		blockHash:  blockHash,
	}
}

// WrapStateWriter wraps a StateWriter with Redis integration if Redis is enabled
func WrapStateWriter(writer StateWriter, blockNum uint64, blockHash libcommon.Hash) StateWriter {
	redis := GetRedisState()
	if redis.Enabled() {
		return NewStateWriterWithRedis(writer, blockNum, blockHash)
	}
	return writer
}

// CreateRedisEnabledWriter creates a new WriterV4 with Redis integration if enabled
func CreateRedisEnabledWriter(tx kv.TemporalPutDel, blockNum uint64, blockHash libcommon.Hash) StateWriter {
	writer := NewWriterV4(tx)
	return WrapStateWriter(writer, blockNum, blockHash)
}

// UpdateAccountData updates account data and writes to Redis
// Redis errors are propagated to ensure synchronization
func (w *StateWriterWithRedis) UpdateAccountData(address libcommon.Address, original, account *accounts.Account) error {
	// First write to underlying storage
	if err := w.underlying.UpdateAccountData(address, original, account); err != nil {
		return err
	}

	// Then write to Redis with block hash for chain identity
	if err := w.redis.writeAccount(w.blockNum, w.blockHash, address, account); err != nil {
		return err
	}

	return nil
}

// DeleteAccount deletes an account and writes to Redis
// Redis errors are propagated to ensure synchronization
func (w *StateWriterWithRedis) DeleteAccount(address libcommon.Address, original *accounts.Account) error {
	// First delete from underlying storage
	if err := w.underlying.DeleteAccount(address, original); err != nil {
		return err
	}

	// Then mark as deleted in Redis with block hash for chain identity
	if err := w.redis.deleteAccount(w.blockNum, w.blockHash, address); err != nil {
		return err
	}

	return nil
}

// UpdateAccountCode updates account code and writes to Redis
// Redis errors are propagated to ensure synchronization
func (w *StateWriterWithRedis) UpdateAccountCode(address libcommon.Address, incarnation uint64, codeHash libcommon.Hash, code []byte) error {
	// First update in underlying storage
	if err := w.underlying.UpdateAccountCode(address, incarnation, codeHash, code); err != nil {
		return err
	}

	// Then write code to Redis
	if err := w.redis.writeCode(codeHash, code); err != nil {
		return err
	}

	return nil
}

// WriteAccountStorage writes account storage and updates Redis
// Redis errors are propagated to ensure synchronization
func (w *StateWriterWithRedis) WriteAccountStorage(address libcommon.Address, incarnation uint64, key *libcommon.Hash, original, value *uint256.Int) error {
	// First write to underlying storage
	if err := w.underlying.WriteAccountStorage(address, incarnation, key, original, value); err != nil {
		return err
	}

	// Then write storage to Redis with block hash for chain identity
	if err := w.redis.writeStorage(w.blockNum, w.blockHash, address, key, value); err != nil {
		return err
	}

	return nil
}

// CreateContract creates a contract and updates Redis
// Redis errors are propagated to ensure synchronization
func (w *StateWriterWithRedis) CreateContract(address libcommon.Address) error {
	// Pass through to underlying writer - account creation is handled by UpdateAccountData
	return w.underlying.CreateContract(address)
}
