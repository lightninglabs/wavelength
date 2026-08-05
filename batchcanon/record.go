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

	// BatchTx is the serialized commitment transaction whose hash is
	// BatchTxID. It makes the registered consumed-input set independently
	// checkable rather than a caller assertion.
	BatchTx []byte

	// BatchOutputIndex selects the transaction output whose script is used
	// for the confirmation watch.
	BatchOutputIndex uint32

	// RegistrationStage is the crash-safe evidence/readiness lifecycle.
	// Only RegistrationComplete can be admitted.
	RegistrationStage RegistrationStage

	// ObservationGeneration identifies the current reconciliation attempt.
	// Restart increments it before any watch is armed.
	ObservationGeneration uint64

	// ReadyGeneration is set only after every registered subject has
	// supplied a current observation for ObservationGeneration.
	ReadyGeneration fn.Option[uint64]

	// Revision increments whenever readiness or semantic availability can
	// change. Admission tokens bind to this value.
	Revision uint64

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

	// WatchHeightHint is the earliest height from which confirmation and
	// spend watches must scan. It is captured before the batch can confirm
	// and retained across restarts so delayed watch installation cannot
	// miss already-mined evidence.
	WatchHeightHint uint32

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

// Ready reports whether the record has complete evidence and a fully installed
// snapshot for its current observation generation.
func (r *Record) Ready() bool {
	return r.EvidenceComplete() &&
		r.RegistrationStage == RegistrationComplete &&
		r.ReadyGeneration.IsSome() &&
		r.ReadyGeneration.UnwrapOr(0) == r.ObservationGeneration
}

// EvidenceComplete reports whether the durable row carries the minimum
// immutable subjects required to reproduce every chain watch. Registration
// performs the stronger serialized-transaction cross-check before a row can
// reach Ready; this cheap predicate also keeps upgrade placeholders and
// corrupt partial rows fail-closed on every read.
func (r *Record) EvidenceComplete() bool {
	if len(r.BatchTx) == 0 || len(r.ConfirmationPkScript) == 0 ||
		len(r.ConsumedInputs) == 0 {
		return false
	}

	for _, input := range r.ConsumedInputs {
		if input.Value < 0 || len(input.PkScript) == 0 {
			return false
		}
	}

	return true
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

	// Value is the authenticated previous-output value in satoshis. It is
	// stored with PkScript so later lineage-proof validation binds the full
	// prevout rather than only an outpoint label.
	Value int64

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

// InputObservation is the mutable conflict view of one immutable consumed
// input. ObservationSnapshot writes every input together so chain evidence
// and its derived availability cannot tear across transactions.
type InputObservation struct {
	Outpoint      wire.OutPoint
	Conflicting   bool
	ConflictFinal bool
}

// ObservationSnapshot is one generation-tagged, fully derived chain view.
// The store applies the confirmation, every input conflict flag, State,
// readiness, and revision atomically.
type ObservationSnapshot struct {
	BatchTxID          chainhash.Hash
	Generation         uint64
	State              State
	ConfirmationHeight fn.Option[int32]
	ConfirmationBlock  fn.Option[chainhash.Hash]
	Inputs             []InputObservation
	Ready              bool
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

// ConsumerEdge records that a locally relevant VTXO was consumed by a batch.
// Its creator lineage and expected business revision are immutable evidence
// used by the terminal restore compare-and-swap.
type ConsumerEdge struct {
	// ConsumedVTXO is the outpoint of the VTXO consumed by ConsumerBatch.
	ConsumedVTXO wire.OutPoint

	// ConsumerBatch is the batch tx that provisionally consumes
	// ConsumedVTXO.
	ConsumerBatch chainhash.Hash

	// ExpectedRevision is the VTXO business revision installed by the exact
	// ForfeitedBy(ConsumerBatch) transition.
	ExpectedRevision uint64

	// CreatorLineage is the complete distinct commitment lineage that makes
	// ConsumedVTXO exist. Restore requires this lineage to be ready and
	// usable.
	CreatorLineage []chainhash.Hash
}

// ConsumerEdgeResolution is the durable outcome of resolving one terminal
// reverse edge.
type ConsumerEdgeResolution int

const (
	// ConsumerEdgeDeferred leaves the edge pending because a
	// compare-and-swap predicate is not currently satisfied.
	ConsumerEdgeDeferred ConsumerEdgeResolution = iota

	// ConsumerEdgeRestored means the exact forfeiture marker was changed to
	// Live and the edge completed in the same transaction.
	ConsumerEdgeRestored

	// ConsumerEdgeCompleted means the edge completed without restoring, for
	// example because the candidate's own creator lineage is invalidated.
	ConsumerEdgeCompleted
)
