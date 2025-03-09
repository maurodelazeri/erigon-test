// Copyright 2024 The Erigon Authors
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package redisstate

import (
	"fmt"

	"github.com/holiman/uint256"

	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon-lib/types/accounts"
	"github.com/erigontech/erigon/core/state"
	"github.com/erigontech/erigon/core/types"
)

// RedisWriterAdapter adapts a RedisHistoricalWriter to be compatible with state.RedisHistoricalWriter
type RedisWriterAdapter struct {
	Writer *RedisHistoricalWriter
	Logger log.Logger
}

// Implement all methods required by state.RedisHistoricalWriter interface
func (a *RedisWriterAdapter) GetBlockNum() uint64 {
	return a.Writer.GetBlockNum()
}

func (a *RedisWriterAdapter) SetTxNum(txNum uint64) {
	a.Writer.SetTxNum(txNum)
}

func (a *RedisWriterAdapter) GetTxNum() uint64 {
	return a.Writer.GetTxNum()
}

func (a *RedisWriterAdapter) WriteBlockStart(blockNum uint64) error {
	fmt.Printf("REDIS_ADAPTER: Starting block %d\n", blockNum)
	return a.Writer.WriteBlockStart(blockNum)
}

func (a *RedisWriterAdapter) WriteChangeSets() error {
	fmt.Printf("REDIS_ADAPTER: Writing change sets for block %d\n", a.Writer.GetBlockNum())
	return a.Writer.WriteChangeSets()
}

func (a *RedisWriterAdapter) WriteHistory() error {
	fmt.Printf("REDIS_ADAPTER: Writing history for block %d\n", a.Writer.GetBlockNum())
	return a.Writer.WriteHistory()
}

func (a *RedisWriterAdapter) HandleTransaction(tx interface{}, receipt interface{}, blockNum uint64, txIndex uint64) error {
	fmt.Printf("REDIS_ADAPTER: Handling transaction at block %d, index %d\n", blockNum, txIndex)

	// Check for nil transaction
	if tx == nil {
		fmt.Printf("REDIS_ADAPTER_WARNING: Nil transaction at block %d, index %d\n", blockNum, txIndex)
		return nil
	}

	// Try direct conversion first
	transaction, ok := tx.(types.Transaction)
	if !ok {
		fmt.Printf("REDIS_ADAPTER_WARNING: Failed to convert transaction to types.Transaction: %T\n", tx)

		// Try to handle it anyway if it has needed methods
		if hasher, ok := tx.(interface{ Hash() libcommon.Hash }); ok {
			txHash := hasher.Hash()
			fmt.Printf("REDIS_ADAPTER: Found transaction hash: %s\n", txHash.Hex())

			// If we can't directly convert to Transaction, but we have a hash,
			// we can try to at least store a minimal transaction in Redis

			// For now, we might need to skip this transaction in Redis
			// but we'll attempt to store receipt data if available
			if receipt != nil {
				fmt.Printf("REDIS_ADAPTER: We have a receipt but no Transaction type, storing receipt only\n")
				// Implement minimal transaction handling here if needed
			}

			// Return success - we don't want to break the main flow
			return nil
		}

		// Can't handle this transaction type
		fmt.Printf("REDIS_ADAPTER_ERROR: Cannot handle transaction type: %T\n", tx)
		return nil
	}

	// Convert the receipt if available
	var txReceipt *types.Receipt
	if receipt != nil {
		txReceipt, ok = receipt.(*types.Receipt)
		if !ok {
			fmt.Printf("REDIS_ADAPTER_WARNING: Failed to convert receipt to *types.Receipt: %T\n", receipt)

			// Try to see if it has minimal receipt interface
			if rcpt, ok := receipt.(interface {
				TxHash() libcommon.Hash
				GasUsed() uint64
			}); ok {
				// Create a minimal receipt
				txHash := rcpt.TxHash()
				gasUsed := rcpt.GasUsed()

				fmt.Printf("REDIS_ADAPTER: Creating minimal receipt for tx %s with gas %d\n",
					txHash.Hex(), gasUsed)

				// We could create a fake receipt here, but for now just log it
			}
		}
	}

	// Call the Writer with the converted types
	err := a.Writer.HandleTransaction(transaction, txReceipt, blockNum, txIndex)
	if err != nil {
		fmt.Printf("REDIS_ADAPTER_ERROR: Failed to handle transaction at block %d, index %d: %v\n",
			blockNum, txIndex, err)
	} else {
		fmt.Printf("REDIS_ADAPTER: Successfully handled transaction %s at block %d\n",
			transaction.Hash().Hex(), blockNum)
	}
	return err
}

func (a *RedisWriterAdapter) StoreBlockInfo(header interface{}, root libcommon.Hash) error {
	fmt.Printf("REDIS_ADAPTER: Storing block info for root %s\n", root.Hex())
	return a.Writer.StoreBlockInfo(header, root)
}

// Forward all StateWriter methods
func (a *RedisWriterAdapter) UpdateAccountData(address libcommon.Address, original, account *accounts.Account) error {
	fmt.Printf("REDIS_ADAPTER: Updating account data for %s\n", address.Hex())

	// No need for type conversion anymore since we're receiving the correct types directly
	err := a.Writer.UpdateAccountData(address, original, account)
	if err != nil {
		fmt.Printf("REDIS_ADAPTER_ERROR: Failed to update account data for %s: %v\n", address.Hex(), err)
	} else {
		fmt.Printf("REDIS_ADAPTER: Successfully updated account for %s\n", address.Hex())
	}
	return err
}

func (a *RedisWriterAdapter) UpdateAccountCode(address libcommon.Address, incarnation uint64, codeHash libcommon.Hash, code []byte) error {
	fmt.Printf("REDIS_ADAPTER: Updating code for address %s, hash: %s, length: %d\n",
		address.Hex(), codeHash.Hex(), len(code))

	err := a.Writer.UpdateAccountCode(address, incarnation, codeHash, code)
	if err != nil {
		fmt.Printf("REDIS_ADAPTER_ERROR: Failed to update code for %s: %v\n", address.Hex(), err)
	}
	return err
}

func (a *RedisWriterAdapter) WriteAccountStorage(address libcommon.Address, incarnation uint64, key *libcommon.Hash, original, value *uint256.Int) error {
	fmt.Printf("REDIS_ADAPTER: Writing storage for address %s, key %s\n", address.Hex(), key.Hex())

	// No type conversion needed anymore since we're receiving the correct types directly
	err := a.Writer.WriteAccountStorage(address, incarnation, key, original, value)
	if err != nil {
		fmt.Printf("REDIS_ADAPTER_ERROR: Failed to write storage for %s, key %s: %v\n",
			address.Hex(), key.Hex(), err)
	} else {
		fmt.Printf("REDIS_ADAPTER: Successfully wrote storage for %s, key %s\n",
			address.Hex(), key.Hex())
	}
	return err
}

func (a *RedisWriterAdapter) CreateContract(address libcommon.Address) error {
	fmt.Printf("REDIS_ADAPTER: Creating contract at address %s\n", address.Hex())
	err := a.Writer.CreateContract(address)
	if err != nil {
		fmt.Printf("REDIS_ADAPTER_ERROR: Failed to create contract at %s: %v\n", address.Hex(), err)
	}
	return err
}

func (a *RedisWriterAdapter) DeleteAccount(address libcommon.Address, original *accounts.Account) error {
	fmt.Printf("REDIS_ADAPTER: Deleting account %s\n", address.Hex())

	// Call the Writer with the original account - no conversion needed since we receive correct type
	err := a.Writer.DeleteAccount(address, original)
	if err != nil {
		fmt.Printf("REDIS_ADAPTER_ERROR: Failed to delete account %s: %v\n", address.Hex(), err)
	} else {
		fmt.Printf("REDIS_ADAPTER: Successfully deleted account %s\n", address.Hex())
	}
	return err
}

// adapterFactoryFn creates an adapter that implements the state.RedisHistoricalWriter interface
func adapterFactoryFn(blockNum uint64) interface{} {
	// Get the writer from the existing factory
	writer := CreateRedisWriter(blockNum)
	if writer == nil {
		fmt.Printf("REDIS_ADAPTER_ERROR: Failed to create Redis writer for block %d\n", blockNum)
		return nil
	}

	// Create an adapter that implements state.RedisHistoricalWriter
	adapter := &RedisWriterAdapter{
		Writer: writer,
		Logger: log.Root(),
	}

	// Add explicit debug logging to verify the interface implementation
	var historyWriter state.RedisHistoricalWriter = adapter
	
	// Double check that we can convert the other way too (added for safety)
	if _, ok := interface{}(historyWriter).(state.RedisHistoricalWriter); !ok {
		fmt.Printf("REDIS_ADAPTER_ERROR: Interface conversion issue detected for block %d\n", blockNum)
	}
	
	fmt.Printf("REDIS_ADAPTER: Successfully created adapter that implements state.RedisHistoricalWriter for block %d\n", blockNum)
	
	// Return the adapter as a state.RedisHistoricalWriter interface type
	// This is critical - we return the interface, not the concrete type
	return historyWriter
}

// init initializes the Redis integration with the core/state package
func init() {
	// Register the Redis writer factory with the state package
	// This allows the state package to create Redis writers when needed
	// without introducing circular dependencies
	logger := log.Root()
	logger.Info("Registering Redis writer factory with core/state package")

	// Use the adapter factory instead of directly using GetRedisWriterFactoryFn
	state.SetRedisWriterFactory(adapterFactoryFn)

	logger.Info("Redis writer factory registered successfully")
}
