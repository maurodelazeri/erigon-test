package stagedsync

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	chaos_monkey "github.com/erigontech/erigon/tests/chaos-monkey"

	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/log/v3"
	state2 "github.com/erigontech/erigon-lib/state"
	"github.com/erigontech/erigon/consensus"
	"github.com/erigontech/erigon/core"
	"github.com/erigontech/erigon/core/rawdb/rawtemporaldb"
	"github.com/erigontech/erigon/core/state"
	"github.com/erigontech/erigon/core/types"
)

type serialExecutor struct {
	txExecutor
	skipPostEvaluation bool
	// outputs
	txCount     uint64
	usedGas     uint64
	blobGasUsed uint64
}

func (se *serialExecutor) wait() error {
	return nil
}

func (se *serialExecutor) status(ctx context.Context, commitThreshold uint64) error {
	return nil
}

func (se *serialExecutor) execute(ctx context.Context, tasks []*state.TxTask) (cont bool, err error) {
	for _, txTask := range tasks {
		if txTask.Error != nil {
			return false, nil
		}

		// Initialize Redis state for this block if Redis is enabled
		// This is a critical fix to ensure block context is set in Redis
		if state.IsRedisEnabled() && txTask.BlockNum > 0 {
			fmt.Printf("REDIS_EXEC: Redis is enabled, initializing block %d\n", txTask.BlockNum)
			blockNum := txTask.BlockNum
			redisWriter := state.GetRedisStateWriter(blockNum)
			if redisWriter != nil {
				fmt.Printf("REDIS_EXEC: Got Redis writer for block %d\n", blockNum)
				// Check interfaces based on method existence, not type assertion
				// This is a more reliable way to handle cross-package interfaces
				writerType := reflect.TypeOf(redisWriter)
				
				// Create a manual proxy for the RedisHistoricalWriter interface
				// This is a workaround for the Go type system's handling of interface identity
				writeBlockStartMethod := reflect.ValueOf(redisWriter).MethodByName("WriteBlockStart")
				
				// Check if the necessary method exists
				useReflection := writeBlockStartMethod.IsValid()
				
				if useReflection {
					fmt.Printf("REDIS_EXEC: Found WriteBlockStart method via reflection on type %s\n", 
					    writerType.String())
					
					// Use reflection to call WriteBlockStart
					args := []reflect.Value{reflect.ValueOf(blockNum)}
					results := writeBlockStartMethod.Call(args)
					
					if len(results) > 0 && !results[0].IsNil() {
						// Check for error
						if errVal, errOk := results[0].Interface().(error); errOk {
							fmt.Printf("REDIS_EXEC_ERROR: Failed to initialize Redis block %d via reflection: %v\n", blockNum, errVal)
							se.logger.Warn("Failed to initialize Redis block", "block", blockNum, "err", errVal)
						}
					} else {
						fmt.Printf("REDIS_EXEC: Successfully initialized Redis block %d via reflection\n", blockNum)
						se.logger.Debug("Successfully initialized Redis block", "block", blockNum)
					}
				} else {
					// Fall back to direct type assertion as a last resort
					redisHistoryWriter, ok := redisWriter.(state.RedisHistoricalWriter)
					if ok {
						fmt.Printf("REDIS_EXEC: Got RedisHistoricalWriter, calling WriteBlockStart for block %d\n", blockNum)
						if err := redisHistoryWriter.WriteBlockStart(blockNum); err != nil {
							fmt.Printf("REDIS_EXEC_ERROR: Failed to initialize Redis block %d: %v\n", blockNum, err)
							se.logger.Warn("Failed to initialize Redis block", "block", blockNum, "err", err)
						} else {
							fmt.Printf("REDIS_EXEC: Successfully initialized Redis block %d\n", blockNum)
							se.logger.Debug("Successfully initialized Redis block", "block", blockNum)
						}
					} else {
						fmt.Printf("REDIS_EXEC_ERROR: Failed to cast writer to RedisHistoricalWriter for block %d\n", blockNum)
						fmt.Printf("REDIS_EXEC_DEBUG: Writer type is %T\n", redisWriter)
						
						// Try to determine if it's a pointer issue
						if writerType.Kind() == reflect.Ptr {
							elemType := writerType.Elem()
							fmt.Printf("REDIS_EXEC_DEBUG: Writer is a pointer to %s\n", elemType.String())
						}
					}
				}
			} else {
				fmt.Printf("REDIS_EXEC_ERROR: Failed to get Redis writer for block %d\n", blockNum)
			}
		} else {
			if !state.IsRedisEnabled() {
				fmt.Printf("REDIS_EXEC: Redis is not enabled\n")
			}
		}

		se.applyWorker.RunTxTaskNoLock(txTask, se.isMining)
		if err := func() error {
			if errors.Is(txTask.Error, context.Canceled) {
				return txTask.Error
			}
			if txTask.Error != nil {
				return fmt.Errorf("%w, txnIdx=%d, %v", consensus.ErrInvalidBlock, txTask.TxIndex, txTask.Error) //same as in stage_exec.go
			}

			se.txCount++
			se.usedGas += txTask.UsedGas
			mxExecGas.Add(float64(txTask.UsedGas))
			mxExecTransactions.Add(1)

			if txTask.Tx != nil {
				se.blobGasUsed += txTask.Tx.GetBlobGas()
			}

			txTask.CreateReceipt(se.applyTx)

			if txTask.Final {
				if !se.isMining && !se.inMemExec && !se.skipPostEvaluation && !se.execStage.CurrentSyncCycle.IsInitialCycle {
					// note this assumes the bloach reciepts is a fixed array shared by
					// all tasks - if that changes this will need to change - robably need to
					// add this to the executor
					se.cfg.notifications.RecentLogs.Add(txTask.BlockReceipts)
				}
				checkReceipts := !se.cfg.vmConfig.StatelessExec && se.cfg.chainConfig.IsByzantium(txTask.BlockNum) && !se.cfg.vmConfig.NoReceipts && !se.isMining
				if txTask.BlockNum > 0 && !se.skipPostEvaluation { //Disable check for genesis. Maybe need somehow improve it in future - to satisfy TestExecutionSpec
					if err := core.BlockPostValidation(se.usedGas, se.blobGasUsed, checkReceipts, txTask.BlockReceipts, txTask.Header, se.isMining, txTask.Txs, se.cfg.chainConfig, se.logger); err != nil {
						return fmt.Errorf("%w, txnIdx=%d, %v", consensus.ErrInvalidBlock, txTask.TxIndex, err) //same as in stage_exec.go
					}
				}

				se.outputBlockNum.SetUint64(txTask.BlockNum)
			}
			if se.cfg.syncCfg.ChaosMonkey {
				chaosErr := chaos_monkey.ThrowRandomConsensusError(se.execStage.CurrentSyncCycle.IsInitialCycle, txTask.TxIndex, se.cfg.badBlockHalt, txTask.Error)
				if chaosErr != nil {
					log.Warn("Monkey in a consensus")
					return chaosErr
				}
			}
			return nil
		}(); err != nil {
			if errors.Is(err, context.Canceled) {
				return false, err
			}
			se.logger.Warn(fmt.Sprintf("[%s] Execution failed", se.execStage.LogPrefix()),
				"block", txTask.BlockNum, "txNum", txTask.TxNum, "hash", txTask.Header.Hash().String(), "err", err, "inMem", se.inMemExec)
			if se.cfg.hd != nil && se.cfg.hd.POSSync() && errors.Is(err, consensus.ErrInvalidBlock) {
				se.cfg.hd.ReportBadHeaderPoS(txTask.Header.Hash(), txTask.Header.ParentHash)
			}
			if se.cfg.badBlockHalt {
				return false, err
			}
			if errors.Is(err, consensus.ErrInvalidBlock) {
				if se.u != nil {
					if err := se.u.UnwindTo(txTask.BlockNum-1, BadBlock(txTask.Header.Hash(), err), se.applyTx); err != nil {
						return false, err
					}
				}
			} else {
				if se.u != nil {
					if err := se.u.UnwindTo(txTask.BlockNum-1, ExecUnwind, se.applyTx); err != nil {
						return false, err
					}
				}
			}
			return false, nil
		}

		if !txTask.Final {
			var receipt *types.Receipt
			if txTask.TxIndex >= 0 {
				receipt = txTask.BlockReceipts[txTask.TxIndex]
			}
			if err := rawtemporaldb.AppendReceipt(se.doms, receipt, se.blobGasUsed); err != nil {
				return false, err
			}
		}

		// MA applystate
		if err := se.rs.ApplyState4(ctx, txTask); err != nil {
			return false, err
		}

		se.outputTxNum.Add(1)
	}

	return true, nil
}

func (se *serialExecutor) commit(ctx context.Context, txNum uint64, blockNum uint64, useExternalTx bool) (t2 time.Duration, err error) {
	// Finalize Redis writes for this block if Redis is enabled
	if state.IsRedisEnabled() && blockNum > 0 {
		fmt.Printf("REDIS_COMMIT: Redis is enabled, finalizing block %d\n", blockNum)
		se.logger.Debug("Finalizing Redis block data", "block", blockNum)
		if redisWriter := state.GetRedisStateWriter(blockNum); redisWriter != nil {
			fmt.Printf("REDIS_COMMIT: Got Redis writer for block %d\n", blockNum)
			// Check interfaces based on method existence, not type assertion
			// This is a more reliable way to handle cross-package interfaces
			writerType := reflect.TypeOf(redisWriter)
			
			// Create a manual proxy for the RedisHistoricalWriter interface
			// This is a workaround for the Go type system's handling of interface identity
			writeChangeSetsMethod := reflect.ValueOf(redisWriter).MethodByName("WriteChangeSets")
			writeHistoryMethod := reflect.ValueOf(redisWriter).MethodByName("WriteHistory")
			storeBlockInfoMethod := reflect.ValueOf(redisWriter).MethodByName("StoreBlockInfo")
			
			// Check if the necessary methods exist
			useReflection := writeChangeSetsMethod.IsValid() && 
			    writeHistoryMethod.IsValid() && 
			    storeBlockInfoMethod.IsValid()
			
			if useReflection {
				fmt.Printf("REDIS_COMMIT: Found all required RedisHistoricalWriter methods via reflection on type %s\n", 
				    writerType.String())
				
				// Get the header for this block
				header := se.getHeader(ctx, libcommon.Hash{}, blockNum)
				if header != nil {
					fmt.Printf("REDIS_COMMIT: Got header for block %d, storing block info via reflection\n", blockNum)
					
					// Store block info using reflection
					storeArgs := []reflect.Value{
						reflect.ValueOf(header),
						reflect.ValueOf(header.Root),
					}
					storeResults := storeBlockInfoMethod.Call(storeArgs)
					
					if len(storeResults) > 0 && !storeResults[0].IsNil() {
						if errVal, errOk := storeResults[0].Interface().(error); errOk {
							fmt.Printf("REDIS_COMMIT_ERROR: Failed to store block info in Redis for block %d: %v\n", blockNum, errVal)
							se.logger.Warn("Failed to store block info in Redis", "block", blockNum, "err", errVal)
						}
					} else {
						fmt.Printf("REDIS_COMMIT: Successfully stored block info for block %d via reflection\n", blockNum)
					}
				}
				
				// Write change sets using reflection
				fmt.Printf("REDIS_COMMIT: Writing change sets for block %d via reflection\n", blockNum)
				changeSetResults := writeChangeSetsMethod.Call([]reflect.Value{})
				if len(changeSetResults) > 0 && !changeSetResults[0].IsNil() {
					if errVal, errOk := changeSetResults[0].Interface().(error); errOk {
						fmt.Printf("REDIS_COMMIT_ERROR: Failed to write change sets to Redis for block %d: %v\n", blockNum, errVal)
						se.logger.Warn("Failed to write change sets to Redis", "block", blockNum, "err", errVal)
					}
				} else {
					fmt.Printf("REDIS_COMMIT: Successfully wrote change sets for block %d via reflection\n", blockNum)
				}
				
				// Write history using reflection
				fmt.Printf("REDIS_COMMIT: Writing history for block %d via reflection\n", blockNum)
				historyResults := writeHistoryMethod.Call([]reflect.Value{})
				if len(historyResults) > 0 && !historyResults[0].IsNil() {
					if errVal, errOk := historyResults[0].Interface().(error); errOk {
						fmt.Printf("REDIS_COMMIT_ERROR: Failed to write history to Redis for block %d: %v\n", blockNum, errVal)
						se.logger.Warn("Failed to write history to Redis", "block", blockNum, "err", errVal)
					}
				} else {
					fmt.Printf("REDIS_COMMIT: Successfully wrote history for block %d via reflection\n", blockNum)
				}
			} else {
				// Fall back to direct type assertion (for backwards compatibility)
				redisHistoryWriter, ok := redisWriter.(state.RedisHistoricalWriter)
				if ok {
					fmt.Printf("REDIS_COMMIT: Got RedisHistoricalWriter for block %d\n", blockNum)
					// Store block header and state root with proper method
					if header := se.getHeader(ctx, libcommon.Hash{}, blockNum); header != nil {
						fmt.Printf("REDIS_COMMIT: Got header for block %d, storing block info\n", blockNum)
						if err := redisHistoryWriter.StoreBlockInfo(header, header.Root); err != nil {
							fmt.Printf("REDIS_COMMIT_ERROR: Failed to store block info in Redis for block %d: %v\n", blockNum, err)
							se.logger.Warn("Failed to store block info in Redis", "block", blockNum, "err", err)
						} else {
							fmt.Printf("REDIS_COMMIT: Successfully stored block info for block %d\n", blockNum)
							se.logger.Debug("Successfully stored block info in Redis", "block", blockNum)
						}
					}

					// Write change sets
					fmt.Printf("REDIS_COMMIT: Writing change sets for block %d\n", blockNum)
					if err := redisHistoryWriter.WriteChangeSets(); err != nil {
						fmt.Printf("REDIS_COMMIT_ERROR: Failed to write change sets to Redis for block %d: %v\n", blockNum, err)
						se.logger.Warn("Failed to write change sets to Redis", "block", blockNum, "err", err)
					} else {
						fmt.Printf("REDIS_COMMIT: Successfully wrote change sets for block %d\n", blockNum)
					}

					// Write history
					fmt.Printf("REDIS_COMMIT: Writing history for block %d\n", blockNum)
					if err := redisHistoryWriter.WriteHistory(); err != nil {
						fmt.Printf("REDIS_COMMIT_ERROR: Failed to write history to Redis for block %d: %v\n", blockNum, err)
						se.logger.Warn("Failed to write history to Redis", "block", blockNum, "err", err)
					} else {
						fmt.Printf("REDIS_COMMIT: Successfully wrote history for block %d\n", blockNum)
					}
				} else {
					fmt.Printf("REDIS_COMMIT_ERROR: Failed to cast writer to RedisHistoricalWriter for block %d\n", blockNum)
				}
			}
		}
	}

	se.doms.Close()
	if err = se.execStage.Update(se.applyTx, blockNum); err != nil {
		return 0, err
	}

	se.applyTx.CollectMetrics()

	if !useExternalTx {
		tt := time.Now()
		if err = se.applyTx.Commit(); err != nil {
			return 0, err
		}

		t2 = time.Since(tt)
		se.agg.BuildFilesInBackground(se.outputTxNum.Load())

		se.applyTx, err = se.cfg.db.BeginRw(context.Background()) //nolint
		if err != nil {
			return t2, err
		}
	}
	se.doms, err = state2.NewSharedDomains(se.applyTx, se.logger)
	if err != nil {
		return t2, err
	}
	se.doms.SetTxNum(txNum)
	se.rs = state.NewStateV3(se.doms, se.logger)

	se.applyWorker.ResetTx(se.applyTx)
	se.applyWorker.ResetState(se.rs, se.accumulator)

	return t2, nil
}