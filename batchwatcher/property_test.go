package batchwatcher

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"pgregory.net/rapid"
)

// ===== StateStore Invariant Tests =====

// TestStateStoreInvariants_Property tests invariants that must always hold for
// the StateStore across any sequence of operations.
func TestStateStoreInvariants_Property(t *testing.T) {
	t.Run("invariant: batch in expiry index", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			store := NewStateStore()
			ops := genStateStoreOps(20).Draw(t, "ops")

			// Apply all operations.
			for _, op := range ops {
				op.Apply(store)
			}

			// INVARIANT: Every batch in batches map has entry in
			// expiryIndex at its expiry height.
			for batchID, state := range store.batches {
				found := false
				height := state.ExpiryHeight
				expiringBatches := store.expiryIndex[height]

				for _, id := range expiringBatches {
					if id == batchID {
						found = true

						break
					}
				}

				if !found {
					t.Fatalf("batch %s at %d not in "+
						"expiryIndex", batchID,
						state.ExpiryHeight)
				}
			}
		})
	})

	t.Run("invariant: expiry index references valid batches",
		func(t *testing.T) {
			rapid.Check(t, func(t *rapid.T) {
				store := NewStateStore()
				ops := genStateStoreOps(20).Draw(t, "ops")

				for _, op := range ops {
					op.Apply(store)
				}

				// INVARIANT: All IDs in expiryIndex exist in
				// batches map.
				for _, batchIDs := range store.expiryIndex {
					for _, bid := range batchIDs {
						_, ok := store.batches[bid]
						if !ok {
							t.Fatalf("batch %s in "+
								"expiry index "+
								"but not in "+
								"batches map",
								bid)
						}
					}
				}
			})
		})

	t.Run("invariant: unregister is idempotent", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			store := NewStateStore()

			// Register a batch.
			batchID := genBatchID().Draw(t, "batchID")
			expiry := genExpiryHeight().Draw(t, "expiry")
			state := NewBatchTreeState(batchID, nil, expiry)
			store.RegisterBatch(state)

			// Unregister twice.
			store.UnregisterBatch(batchID)
			beforeSecond := store.NumBatches()
			store.UnregisterBatch(batchID)
			afterSecond := store.NumBatches()

			// INVARIANT: Second unregister has no effect.
			if beforeSecond != afterSecond {
				t.Fatalf("second unregister changed count: "+
					"%d -> %d", beforeSecond, afterSecond)
			}
		})
	})

	t.Run("invariant: batch count consistency", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			store := NewStateStore()
			ops := genStateStoreOps(20).Draw(t, "ops")

			for _, op := range ops {
				op.Apply(store)
			}

			// INVARIANT: NumBatches equals len(batches).
			if store.NumBatches() != len(store.batches) {
				t.Fatalf("NumBatches() = %d but "+
					"len(batches) = %d",
					store.NumBatches(), len(store.batches))
			}
		})
	})
}

// ===== BatchTreeState Invariant Tests =====

// TestBatchTreeStateInvariants_Property tests invariants that must always hold
// for BatchTreeState across any sequence of operations.
func TestBatchTreeStateInvariants_Property(t *testing.T) {
	t.Run("invariant: VTXOsOnChain subset of ExistingOutputs",
		func(t *testing.T) {
			rapid.Check(t, func(t *rapid.T) {
				batchID := genBatchID().Draw(t, "batchID")
				state := NewBatchTreeState(batchID, nil, 1000)
				ops := genTreeStateOps(30).Draw(t, "ops")

				for _, op := range ops {
					op.Apply(state)

					// INVARIANT: VTXOsOnChain subset of
					// ExistingOutputs.
					for op := range state.VTXOsOnChain {
						ex := state.ExistingOutputs
						_, ok := ex[op]
						if !ok {
							t.Fatalf("VTXO %v not "+
								"in existing",
								op)
						}
					}
				}
			})
		})

	t.Run("invariant: removed output gone", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			batchID := genBatchID().Draw(t, "batchID")
			state := NewBatchTreeState(batchID, nil, 1000)

			// Add an output.
			output := genOutput().Draw(t, "output")
			state.AddExistingOutput(output)

			// Remove it.
			state.RemoveExistingOutput(output.Outpoint)

			// INVARIANT: Output not in ExistingOutputs.
			_, inExisting := state.ExistingOutputs[output.Outpoint]
			if inExisting {
				t.Fatalf("removed output still in existing")
			}

			// INVARIANT: Output not in VTXOsOnChain.
			_, inVTXOs := state.VTXOsOnChain[output.Outpoint]
			if inVTXOs {
				t.Fatalf("removed output still in VTXOsOnChain")
			}
		})
	})

	t.Run("invariant: VTXO flag consistency", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			batchID := genBatchID().Draw(t, "batchID")
			state := NewBatchTreeState(batchID, nil, 1000)
			ops := genTreeStateOps(30).Draw(t, "ops")

			for _, op := range ops {
				op.Apply(state)
			}

			// INVARIANT: Every output in VTXOsOnChain has
			// IsVTXO=true in ExistingOutputs.
			for op := range state.VTXOsOnChain {
				output, found := state.ExistingOutputs[op]
				if !found {
					// Covered by subset invariant.
					continue
				}

				if !output.IsVTXO {
					t.Fatalf("output %v in VTXOsOnChain "+
						"but IsVTXO=false", op)
				}
			}
		})
	})

	t.Run("invariant: watched set monotonic", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			batchID := genBatchID().Draw(t, "batchID")
			state := NewBatchTreeState(batchID, nil, 1000)

			// Generate sequence of mark watched operations.
			numOps := rapid.IntRange(1, 20).Draw(t, "numOps")
			previousSize := 0

			for i := 0; i < numOps; i++ {
				outpoint := genOutpoint().Draw(t, "outpoint")
				state.MarkWatched(outpoint)
				currentSize := len(state.WatchedOutpoints)

				// INVARIANT: WatchedOutpoints is monotonic.
				if currentSize < previousSize {
					t.Fatalf("WatchedOutpoints size "+
						"decreased: %d -> %d",
						previousSize, currentSize)
				}

				previousSize = currentSize
			}
		})
	})

	t.Run("invariant: spent nodes set only grows", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			batchID := genBatchID().Draw(t, "batchID")
			state := NewBatchTreeState(batchID, nil, 1000)

			numOps := rapid.IntRange(1, 20).Draw(t, "numOps")
			previousSize := 0

			for i := 0; i < numOps; i++ {
				txid := genChainhash().Draw(t, "txid")
				state.MarkNodeSpent(txid)
				currentSize := len(state.SpentNodes)

				// INVARIANT: SpentNodes size never decreases.
				if currentSize < previousSize {
					t.Fatalf("SpentNodes size decreased: "+
						"%d -> %d",
						previousSize, currentSize)
				}

				previousSize = currentSize
			}
		})
	})
}

// ===== Actor Invariant Tests =====

// TestActorInvariants_Property tests invariants that must hold for the
// BatchWatcherActor across any valid sequence of messages.
func TestActorInvariants_Property(t *testing.T) {
	t.Run("invariant: GetTreeState returns found only for registered",
		func(t *testing.T) {
			rapid.Check(t, func(rt *rapid.T) {
				h := newTestHarness(t)

				// Set up mock expectations.
				setupMockForPropertyTest(h)

				// Generate batch IDs.
				registeredID := genBatchID().Draw(rt,
					"registeredID")
				unregisteredID := genBatchID().Draw(rt,
					"unregisteredID")

				// Register one batch.
				testTree := h.createSimpleTree(t)
				treeState := NewBatchTreeState(
					registeredID, testTree, 1000,
				)
				h.actor.state.RegisterBatch(treeState)

				// Query registered batch.
				req1 := &GetTreeStateRequest{
					BatchID: registeredID,
				}
				result1 := h.actor.Receive(t.Context(), req1)

				if result1.IsErr() {
					rt.Fatalf("unexpected error: %v",
						result1.Err())
				}

				res1 := result1.UnwrapOr(nil)
				resp1, ok := res1.(*GetTreeStateResponse)
				if !ok {
					rt.Fatalf("unexpected response type")
				}

				if !resp1.Found {
					rt.Fatalf("registered batch not found")
				}

				// Query unregistered batch.
				req2 := &GetTreeStateRequest{
					BatchID: unregisteredID,
				}
				result2 := h.actor.Receive(t.Context(), req2)

				if result2.IsErr() {
					rt.Fatalf("unexpected error: %v",
						result2.Err())
				}

				res2 := result2.UnwrapOr(nil)
				resp2, ok := res2.(*GetTreeStateResponse)
				if !ok {
					rt.Fatalf("unexpected response type")
				}

				// INVARIANT: Unregistered batch not found.
				if resp2.Found {
					rt.Fatalf("unregistered batch should " +
						"not be found")
				}
			})
		})

	t.Run("invariant: expiry notification only at correct height",
		func(t *testing.T) {
			rapid.Check(t, func(rt *rapid.T) {
				h := newTestHarness(t)

				// Generate expiry height.
				expiryHeight := genExpiryHeight().Draw(rt,
					"expiry")

				// Register batch.
				batchID := genBatchID().Draw(rt, "batchID")
				treeState := NewBatchTreeState(
					batchID, nil, expiryHeight,
				)
				h.actor.state.RegisterBatch(treeState)

				// Send blocks at different heights.
				testHeights := []int32{
					int32(expiryHeight) - 100,
					int32(expiryHeight) - 1,
					int32(expiryHeight),
					int32(expiryHeight) + 1,
				}

				for _, height := range testHeights {
					h.mockBatchSweeper.receivedMsgs = nil
					msg := &NewBlockReceived{Height: height}
					_ = h.actor.Receive(t.Context(), msg)

					gotNotification := len(
						h.mockBatchSweeper.receivedMsgs,
					) > 0

					shouldNotify := height ==
						int32(expiryHeight)

					// INVARIANT: Notification only at
					// exact expiry height.
					if gotNotification != shouldNotify {
						rt.Fatalf("height %d: got "+
							"notification=%v, "+
							"want %v",
							height, gotNotification,
							shouldNotify)
					}
				}
			})
		})
}

// ===== Helper Functions =====

// setupMockForPropertyTest configures mocks for property tests.
func setupMockForPropertyTest(h *testHarness) {
	// Property tests don't need specific mock expectations since they
	// primarily test state management logic.
}

// Compile-time check that operations implement interfaces.
var (
	_ StateStoreOp = RegisterOp{}
	_ StateStoreOp = UnregisterOp{}
	_ TreeStateOp  = AddOutputOp{}
	_ TreeStateOp  = RemoveOutputOp{}
	_ TreeStateOp  = MarkWatchedOp{}
	_ TreeStateOp  = MarkSpentOp{}
)

// Prevent unused import errors.
var _ = wire.OutPoint{}
