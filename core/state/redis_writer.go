package state

import (
	libcommon "github.com/erigontech/erigon-lib/common"
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

// UpdateAccountData updates account data and writes to Redis
func (w *StateWriterWithRedis) UpdateAccountData(address libcommon.Address, original, account *accounts.Account) error {
	// Write to Redis with block hash for chain identity
	w.redis.writeAccount(w.blockNum, w.blockHash, address, account)

	// Pass through to underlying writer
	return w.underlying.UpdateAccountData(address, original, account)
}

// DeleteAccount deletes an account and writes to Redis
func (w *StateWriterWithRedis) DeleteAccount(address libcommon.Address, original *accounts.Account) error {
	// Mark as deleted in Redis with block hash for chain identity
	w.redis.deleteAccount(w.blockNum, w.blockHash, address)

	// Pass through to underlying writer
	return w.underlying.DeleteAccount(address, original)
}

// UpdateAccountCode updates account code and writes to Redis
func (w *StateWriterWithRedis) UpdateAccountCode(address libcommon.Address, incarnation uint64, codeHash libcommon.Hash, code []byte) error {
	// Write code to Redis - code is immutable and addressed by hash
	w.redis.writeCode(codeHash, code)

	// Pass through to underlying writer
	return w.underlying.UpdateAccountCode(address, incarnation, codeHash, code)
}

// WriteAccountStorage writes account storage and updates Redis
func (w *StateWriterWithRedis) WriteAccountStorage(address libcommon.Address, incarnation uint64, key *libcommon.Hash, original, value *uint256.Int) error {
	// Write storage to Redis with block hash for chain identity
	w.redis.writeStorage(w.blockNum, w.blockHash, address, key, value)

	// Pass through to underlying writer
	return w.underlying.WriteAccountStorage(address, incarnation, key, original, value)
}

// CreateContract creates a contract and updates Redis
func (w *StateWriterWithRedis) CreateContract(address libcommon.Address) error {
	// Pass through to underlying writer - account creation is handled by UpdateAccountData
	return w.underlying.CreateContract(address)
}
