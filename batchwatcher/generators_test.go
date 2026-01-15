package batchwatcher

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/google/uuid"
	"pgregory.net/rapid"
)

// genBatchID generates a random BatchID for property tests.
func genBatchID() *rapid.Generator[BatchID] {
	return rapid.Custom(func(t *rapid.T) BatchID {
		// Generate a UUID from random bytes using rapid's bitstream.
		var uuidBytes [16]byte
		for i := range uuidBytes {
			uuidBytes[i] = rapid.Byte().Draw(t, "uuid_byte")
		}

		// Set version (4) and variant (RFC 4122) bits.
		uuidBytes[6] = (uuidBytes[6] & 0x0f) | 0x40 // Version 4
		uuidBytes[8] = (uuidBytes[8] & 0x3f) | 0x80 // Variant RFC 4122

		id, err := uuid.FromBytes(uuidBytes[:])
		if err != nil {
			// Should never happen with valid bytes.
			id = uuid.UUID{}
		}

		return BatchID(id)
	})
}

// genExpiryHeight generates a valid expiry height.
func genExpiryHeight() *rapid.Generator[uint32] {
	return rapid.Uint32Range(1, 1_000_000)
}

// genOutpoint generates a random wire.OutPoint.
func genOutpoint() *rapid.Generator[wire.OutPoint] {
	return rapid.Custom(func(t *rapid.T) wire.OutPoint {
		var hash chainhash.Hash
		hashBytes := rapid.SliceOfN(
			rapid.Byte(), 32, 32,
		).Draw(t, "hash")
		copy(hash[:], hashBytes)

		return wire.OutPoint{
			Hash:  hash,
			Index: rapid.Uint32Range(0, 10).Draw(t, "index"),
		}
	})
}

// genTxOut generates a random wire.TxOut.
func genTxOut() *rapid.Generator[*wire.TxOut] {
	return rapid.Custom(func(t *rapid.T) *wire.TxOut {
		value := rapid.Int64Range(
			0, 2_100_000_000_000_000,
		).Draw(t, "value")
		pkScript := rapid.SliceOfN(
			rapid.Byte(), 20, 40,
		).Draw(t, "pkScript")

		return wire.NewTxOut(value, pkScript)
	})
}

// genOutput generates a random Output struct.
func genOutput() *rapid.Generator[*Output] {
	return rapid.Custom(func(t *rapid.T) *Output {
		outputIdx := rapid.Uint32Range(0, 10).Draw(t, "outputIndex")

		return &Output{
			Outpoint: genOutpoint().Draw(t, "outpoint"),
			TxOut:    genTxOut().Draw(t, "txout"),
			IsVTXO:   rapid.Bool().Draw(t, "isVTXO"),
			// TreeNode requires tree construction.
			TreeNode:    nil,
			OutputIndex: outputIdx,
		}
	})
}

// genChainhash generates a random chainhash.Hash.
func genChainhash() *rapid.Generator[chainhash.Hash] {
	return rapid.Custom(func(t *rapid.T) chainhash.Hash {
		var hash chainhash.Hash
		hashBytes := rapid.SliceOfN(
			rapid.Byte(), 32, 32,
		).Draw(t, "hash")
		copy(hash[:], hashBytes)

		return hash
	})
}

// ===== Operation Generators for State Machines =====

// StateStoreOp represents an operation on StateStore.
type StateStoreOp interface {
	Apply(store *StateStore)
}

// RegisterOp represents a RegisterBatch operation.
type RegisterOp struct {
	BatchID      BatchID
	ExpiryHeight uint32
}

// Apply executes the register operation.
func (op RegisterOp) Apply(store *StateStore) {
	state := NewBatchTreeState(op.BatchID, nil, op.ExpiryHeight)
	store.RegisterBatch(state)
}

// UnregisterOp represents an UnregisterBatch operation.
type UnregisterOp struct {
	BatchID BatchID
}

// Apply executes the unregister operation.
func (op UnregisterOp) Apply(store *StateStore) {
	store.UnregisterBatch(op.BatchID)
}

// genRegisterOp generates a random RegisterOp.
func genRegisterOp() *rapid.Generator[RegisterOp] {
	return rapid.Custom(func(t *rapid.T) RegisterOp {
		return RegisterOp{
			BatchID:      genBatchID().Draw(t, "batchID"),
			ExpiryHeight: genExpiryHeight().Draw(t, "expiry"),
		}
	})
}

// genStateStoreOps generates a sequence of StateStore operations.
// This generates a mix of register and unregister operations.
func genStateStoreOps(maxOps int) *rapid.Generator[[]StateStoreOp] {
	return rapid.Custom(func(t *rapid.T) []StateStoreOp {
		numOps := rapid.IntRange(1, maxOps).Draw(t, "numOps")
		ops := make([]StateStoreOp, numOps)
		knownBatches := make([]BatchID, 0)

		for i := 0; i < numOps; i++ {
			opType := rapid.IntRange(0, 1).Draw(t, "opType")

			switch opType {
			case 0:
				// Register new batch.
				regOp := genRegisterOp().Draw(t, "regOp")
				ops[i] = regOp
				knownBatches = append(
					knownBatches, regOp.BatchID,
				)

			case 1:
				// Unregister batch.
				if len(knownBatches) > 0 {
					idx := rapid.IntRange(
						0, len(knownBatches)-1,
					).Draw(t, "idx")
					ops[i] = UnregisterOp{
						BatchID: knownBatches[idx],
					}
				} else {
					// No known batches, register instead.
					op := genRegisterOp().Draw(t, "reg")
					ops[i] = op
					knownBatches = append(
						knownBatches, op.BatchID,
					)
				}
			}
		}

		return ops
	})
}

// ===== TreeState Operation Generators =====

// TreeStateOp represents an operation on BatchTreeState.
type TreeStateOp interface {
	Apply(state *BatchTreeState)
}

// AddOutputOp adds an output to the state.
type AddOutputOp struct {
	Output *Output
}

// Apply executes the add output operation.
func (op AddOutputOp) Apply(state *BatchTreeState) {
	state.AddExistingOutput(op.Output)
}

// RemoveOutputOp removes an output from the state.
type RemoveOutputOp struct {
	Outpoint wire.OutPoint
}

// Apply executes the remove output operation.
func (op RemoveOutputOp) Apply(state *BatchTreeState) {
	state.RemoveExistingOutput(op.Outpoint)
}

// MarkWatchedOp marks an outpoint as watched.
type MarkWatchedOp struct {
	Outpoint wire.OutPoint
}

// Apply executes the mark watched operation.
func (op MarkWatchedOp) Apply(state *BatchTreeState) {
	state.MarkWatched(op.Outpoint)
}

// MarkSpentOp marks a node as spent.
type MarkSpentOp struct {
	TxID chainhash.Hash
}

// Apply executes the mark spent operation.
func (op MarkSpentOp) Apply(state *BatchTreeState) {
	state.MarkNodeSpent(op.TxID)
}

// genTreeStateOps generates a sequence of BatchTreeState operations.
func genTreeStateOps(maxOps int) *rapid.Generator[[]TreeStateOp] {
	return rapid.Custom(func(t *rapid.T) []TreeStateOp {
		numOps := rapid.IntRange(1, maxOps).Draw(t, "numOps")
		ops := make([]TreeStateOp, numOps)
		knownOutpoints := make([]wire.OutPoint, 0)

		for i := 0; i < numOps; i++ {
			opType := rapid.IntRange(0, 3).Draw(t, "opType")

			switch opType {
			case 0:
				// Add output.
				output := genOutput().Draw(t, "output")
				ops[i] = AddOutputOp{Output: output}
				knownOutpoints = append(
					knownOutpoints, output.Outpoint,
				)

			case 1:
				// Remove output.
				if len(knownOutpoints) > 0 {
					idx := rapid.IntRange(
						0, len(knownOutpoints)-1,
					).Draw(t, "idx")
					ops[i] = RemoveOutputOp{
						Outpoint: knownOutpoints[idx],
					}
				} else {
					op := genOutpoint().Draw(t, "outpoint")
					ops[i] = RemoveOutputOp{Outpoint: op}
				}

			case 2:
				// Mark watched.
				outpoint := genOutpoint().Draw(t, "outpoint")
				ops[i] = MarkWatchedOp{Outpoint: outpoint}

			case 3:
				// Mark spent.
				ops[i] = MarkSpentOp{
					TxID: genChainhash().Draw(t, "txid"),
				}
			}
		}

		return ops
	})
}
