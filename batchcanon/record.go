package batchcanon

import (
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// Record is the durable canonicality view of one batch (commitment)
// transaction, keyed by its txid. It bundles the interpreted State, the
// current confirmation observation, the recompute inputs for effective
// expiry, the consumed inputs and dependent VTXOs, and the reserved policy
// slot.
type Record struct {
	// BatchTxID is the commitment transaction id and the record's
	// identity. Identity is by txid, never by (txid, block hash): a reorg
	// that re-mines the same tx in a different block is the same batch.
	BatchTxID chainhash.Hash

	// State is the interpreted canonicality state.
	State State

	// ConfirmationHeight is the best-chain height at which the batch tx
	// is currently observed confirmed. None when the batch is not
	// currently confirmed (unseen or reorged out). A reorg clears it; a
	// reconfirmation sets it to the new height.
	ConfirmationHeight fn.Option[int32]

	// ConfirmationBlock is the hash of the block currently confirming the
	// batch tx. It is an observation attribute only and is NOT part of
	// the batch identity. None when the batch is not currently confirmed.
	ConfirmationBlock fn.Option[chainhash.Hash]

	// CSVExpiryDelta is the batch's CSV-relative expiry timeout, in
	// blocks. The effective (absolute) expiry height is derived from this
	// plus the current confirmation height, so it tracks reconfirmations
	// after a reorg instead of being frozen at first confirmation.
	CSVExpiryDelta int32

	// ConfirmationPkScript is the pkScript of the batch-tx output the
	// confirmation watch keys on. It is persisted so the manager can
	// re-register the watch after a restart, since light-client backends
	// (neutrino, Esplora) filter confirmation notifications by pkScript.
	// May be empty for records seeded by descriptor backfill, which has no
	// batch-output pkScript to derive.
	ConfirmationPkScript []byte

	// PolicyState is the reserved policy classification slot. See
	// PolicyState.
	PolicyState PolicyState

	// ConsumedInputs are the inputs this batch tx spends. They are tracked
	// so the manager can watch each one for a conflicting spend.
	ConsumedInputs []ConsumedInput

	// DependentVTXOs are the VTXO outpoints anchored by this batch. Their
	// derived availability follows this batch's canonicality.
	DependentVTXOs []wire.OutPoint
}

// ConsumedInput is one input a batch (commitment) tx spends, paired with the
// pkScript of the output being spent. The pkScript is required to register the
// reorg-aware spend watch: lnd's spend notifier filters by the output's script,
// so a bare outpoint is rejected ("an output script must be provided"). It is
// persisted alongside the outpoint so the watch can be re-armed after a
// restart.
type ConsumedInput struct {
	// Outpoint is the spent output.
	Outpoint wire.OutPoint

	// PkScript is the scriptPubKey of the spent output, used to register
	// the spend watch. May be empty only for legacy/backfilled rows that
	// predate script tracking; such inputs cannot be watched on
	// light-client backends.
	PkScript []byte

	// Conflicting is true while a conflicting spend of this input (a spend
	// by a transaction other than the batch itself) is observed and has not
	// been reorged out. Persisting it lets restart reconciliation rebuild
	// the per-input conflict view so live re-observation cannot transiently
	// downgrade a persisted conflict before the conflicting spend
	// re-arrives.
	Conflicting bool

	// ConflictFinal is true once a conflicting spend of this input has
	// matured past the reorg-safety depth. Persisted for the same
	// restart-reconciliation reason as Conflicting.
	ConflictFinal bool
}

// EffectiveExpiry derives the absolute expiry height from the current
// confirmation observation: ConfirmationHeight + CSVExpiryDelta. It returns
// None when the batch is not currently confirmed.
//
// Deriving expiry on demand (rather than persisting an absolute height) is
// what keeps expiry reorg-safe: a confirmation that is reorged out clears
// ConfirmationHeight and so erases the effective expiry, and a
// reconfirmation at a different height yields a fresh effective expiry.
// Expiry is therefore never a one-way terminal fact at this layer.
func (r *Record) EffectiveExpiry() fn.Option[int32] {
	return fn.MapOption(
		func(height int32) int32 {
			return height + r.CSVExpiryDelta
		})(r.ConfirmationHeight)
}

// ProvisionalConsumer records that a (locally relevant) VTXO has been
// provisionally consumed by a not-yet-canonical consumer batch. It is the
// reverse-dependency edge that lets a provisionally consumed VTXO be restored
// if the consumer batch never becomes canonical — for example a round-2
// forfeit whose commitment tx is reorged out, which must restore the round-1
// VTXO it consumed.
type ProvisionalConsumer struct {
	// ConsumedVTXO is the outpoint of the VTXO consumed by ConsumerBatch.
	ConsumedVTXO wire.OutPoint

	// ConsumerBatch is the batch tx that provisionally consumes
	// ConsumedVTXO.
	ConsumerBatch chainhash.Hash
}
