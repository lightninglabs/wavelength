package batchcanon

import "fmt"

// RegistrationStage describes whether complete durable evidence and a
// generation-consistent chain snapshot are installed for a batch. Semantic
// State is never admissible unless the stage is RegistrationComplete and the
// ready generation matches the observation generation.
type RegistrationStage int

const (
	// RegistrationRegistering means evidence is durably staged but the
	// complete chain watch/snapshot generation has not been installed.
	RegistrationRegistering RegistrationStage = iota

	// RegistrationReconciling means restart reconciliation is rebuilding a
	// fresh observation generation. Admission remains closed.
	RegistrationReconciling

	// RegistrationComplete means complete immutable evidence is present and
	// ReadyGeneration identifies the installed observation generation.
	RegistrationComplete

	// RegistrationQuarantined means a repeated registration contradicted
	// immutable durable evidence. Admission remains closed until an
	// explicit repair proves which evidence is authoritative.
	RegistrationQuarantined
)

// String returns a stable lower-snake-case registration stage.
func (s RegistrationStage) String() string {
	switch s {
	case RegistrationRegistering:
		return "registering"

	case RegistrationReconciling:
		return "reconciling"

	case RegistrationComplete:
		return "complete"

	case RegistrationQuarantined:
		return "quarantined"

	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// State is the canonicality state of a batch (commitment) transaction as
// interpreted from raw chain observation. Provisional states are reversible
// until the configured policy-finality boundary. Finalized states are terminal
// within the basic-v1 safety claim because their watches are released;
// post-finality deep-reorg detection is explicitly outside that claim.
//
// State is persisted as a typed INTEGER column. Values are append-only and
// MUST NOT be renumbered, because persisted rows reference them directly.
type State int

const (
	// StateUnseen indicates the batch tx has not been observed confirmed
	// on the best chain. This is the zero value: a freshly recorded
	// batch with no confirmation observation is unseen.
	StateUnseen State = iota

	// StateProvisional indicates the batch tx is confirmed but has not
	// yet matured past the configured finality depth, so its
	// confirmation may still be reorged out.
	StateProvisional

	// StateFinalized indicates the batch tx confirmation has matured past
	// the configured finality depth. This is policy finality at the
	// configured depth, not a claim of absolute Bitcoin finality.
	StateFinalized

	// StateReorgedOut indicates a previously observed confirmation left
	// the best chain and no consumed input has been seen double-spent.
	// The batch may reconfirm, so dependent VTXOs enter limbo rather than
	// being invalidated.
	StateReorgedOut

	// StateConflictProvisional indicates a consumed batch input was
	// double-spent by a conflicting transaction on the best chain, and
	// that conflicting spend has not yet matured past the finality depth.
	StateConflictProvisional

	// StateConflictFinalized indicates a consumed-input conflict has
	// matured past the finality depth. It is a terminal invalidation within
	// the configured policy claim and may trigger conditional restoration
	// of a logically consumed ancestor.
	StateConflictFinalized
)

// String returns a stable lower-snake-case name for the state, matching the
// vocabulary used in darepo#454 and the persisted-column documentation.
func (s State) String() string {
	switch s {
	case StateUnseen:
		return "unseen"

	case StateProvisional:
		return "provisional"

	case StateFinalized:
		return "finalized"

	case StateReorgedOut:
		return "reorged_out"

	case StateConflictProvisional:
		return "conflict_provisional"

	case StateConflictFinalized:
		return "conflict_finalized"

	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// PolicyState is a durable, reorg-independent policy classification slot for a
// batch. darepo#454 reserves this field in the data model; this layer
// persists and round-trips it but assigns no business meaning yet. The
// BatchCanonicalityManager and the admission gates in later tasks own its
// interpretation.
//
// Like State, it is persisted as an append-only typed INTEGER column.
type PolicyState int

const (
	// PolicyStateDefault is the zero value and the only policy state
	// defined at the data-model layer.
	PolicyStateDefault PolicyState = iota
)

// String returns a stable lower-snake-case name for the policy state.
func (p PolicyState) String() string {
	switch p {
	case PolicyStateDefault:
		return "default"

	default:
		return fmt.Sprintf("unknown(%d)", int(p))
	}
}
