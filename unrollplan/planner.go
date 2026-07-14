package unrollplan

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// Planner evaluates unilateral-exit progress for one immutable recovery
// proof.
//
// The Planner is intentionally stateless: it wraps a *recovery.Proof and
// nothing else. All progress state lives in the caller-owned `State` passed
// to Plan. This shape makes the Planner trivially re-usable across restarts
// (build once, call Plan whenever state changes) and avoids the
// cache-invalidation bugs a stateful planner would invite when the durable
// state evolves on disk.
type Planner struct {
	proof *recovery.Proof
}

// NewPlanner creates a pure planner for one immutable recovery proof. The
// only failure mode is a nil proof, which indicates a caller bug; there is
// no filesystem or network I/O.
func NewPlanner(proof *recovery.Proof) (*Planner, error) {
	if proof == nil {
		return nil, fmt.Errorf("proof cannot be nil")
	}

	return &Planner{proof: proof}, nil
}

// Proof returns the immutable recovery proof the planner is evaluating.
// A nil receiver returns nil so that defensive callers do not need to
// separately check `planner == nil` before reaching through for the proof.
func (p *Planner) Proof() *recovery.Proof {
	if p == nil {
		return nil
	}

	return p.proof
}

// State captures the durable, caller-owned progress needed to resume planning.
// The canonical serialization is TLV (see state_codec.go); no JSON tags are
// provided because go's default JSON marshaler for chainhash.Hash is
// parser-differential (accepts short forms and legacy arrays) and would admit
// key-collision attacks against a persisted state file.
type State struct {
	// ConfirmedTxids lists proof txids the caller has observed
	// confirmed.
	ConfirmedTxids []chainhash.Hash

	// InFlightTxids lists proof txids currently being
	// materialized by the caller but not yet observed confirmed.
	InFlightTxids []chainhash.Hash

	// TargetConfirmHeight records the confirmation height of the target
	// tx. Some once the target has confirmed, None while it is still
	// pending.
	TargetConfirmHeight fn.Option[int32]

	// Sweep records the final sweep lifecycle state.
	Sweep SweepState
}

// SweepStatus describes the caller-observed state of the final sweep.
type SweepStatus int

const (
	// SweepStatusPending means the sweep has not been broadcast yet.
	SweepStatusPending SweepStatus = iota

	// SweepStatusBroadcasted means the sweep was broadcast and is awaiting
	// confirmation.
	SweepStatusBroadcasted

	// SweepStatusConfirmed means the sweep confirmed on-chain.
	SweepStatusConfirmed
)

// String returns the stable debug label for a SweepStatus.
func (s SweepStatus) String() string {
	switch s {
	case SweepStatusPending:
		return "pending"

	case SweepStatusBroadcasted:
		return "broadcasted"

	case SweepStatusConfirmed:
		return "confirmed"

	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// SweepState captures the durable state of the final sweep.
type SweepState struct {
	// Status records whether the sweep is pending, broadcasted,
	// or confirmed.
	Status SweepStatus

	// Txid records the sweep txid once broadcast. None while the sweep is
	// still pending.
	Txid fn.Option[chainhash.Hash]

	// ConfirmHeight records the sweep confirmation height once known.
	// None until the sweep confirms.
	ConfirmHeight fn.Option[int32]
}

// Validate checks that the durable state is internally consistent with the
// immutable proof graph.
//
// Ordered so the cheapest checks run first:
//
//  1. Non-nil proof + state (cheap)
//  2. ConfirmedTxids and InFlightTxids convert cleanly to fn.Sets
//     (catches duplicates inside each slice)
//  3. The two sets are disjoint (a tx cannot be both confirmed and
//     in-flight)
//  4. Every txid references a real proof node
//  5. TargetConfirmHeight presence symmetry: set only when the target is
//     confirmed, required when the target IS confirmed, non-negative.
//  6. Sweep state is internally consistent per its three-state lifecycle.
//  7. Topological invariant: every confirmed / in-flight node has all
//     in-proof parents confirmed. This is the expensive check.
//
// The order matters only for fast-fail under adversarial inputs; a
// well-formed state passes every step without any measurable cost even
// at realistic proof sizes.
func (s *State) Validate(proof *recovery.Proof) error {
	if proof == nil {
		return fmt.Errorf("proof cannot be nil")
	}

	if s == nil {
		return fmt.Errorf("state cannot be nil")
	}

	confirmed, err := hashSetFromSlice(s.ConfirmedTxids, "confirmed txids")
	if err != nil {
		return err
	}

	inflight, err := hashSetFromSlice(s.InFlightTxids, "in-flight txids")
	if err != nil {
		return err
	}

	if err := ensureDisjoint(confirmed, inflight); err != nil {
		return err
	}

	for txid := range confirmed {
		if _, ok := proof.Node(txid); !ok {
			return fmt.Errorf("confirmed txid %s is not in proof",
				txid)
		}
	}

	for txid := range inflight {
		if _, ok := proof.Node(txid); !ok {
			return fmt.Errorf("in-flight txid %s is not in proof",
				txid)
		}
	}

	targetConfirmed := confirmedTxidSetContains(
		confirmed, proof.TargetOutpoint().Hash,
	)
	if s.TargetConfirmHeight.IsSome() && !targetConfirmed {
		return fmt.Errorf("target confirm height set without " +
			"confirmed target")
	}
	if targetConfirmed && s.TargetConfirmHeight.IsNone() {
		return fmt.Errorf("target confirmed without target confirm " +
			"height")
	}
	confirmHeightErr := fn.MapOptionZ(s.TargetConfirmHeight,
		func(h int32) error {
			if h < 0 {
				return fmt.Errorf("target confirm height %d "+
					"is negative", h)
			}

			return nil
		},
	)
	if confirmHeightErr != nil {
		return confirmHeightErr
	}

	err = validateSweepState(
		s.Sweep, confirmed, proof, s.TargetConfirmHeight,
	)
	if err != nil {
		return err
	}

	for txid := range confirmed {
		err := ensureParentsConfirmed(
			proof, confirmed, txid,
		)
		if err != nil {
			return err
		}
	}

	for txid := range inflight {
		err := ensureParentsConfirmed(
			proof, confirmed, txid,
		)
		if err != nil {
			return fmt.Errorf("in-flight tx %s: %w", txid, err)
		}
	}

	return nil
}

// Snapshot is the caller-facing planning view at one block height.
type Snapshot struct {
	// Ready are transactions the caller should try to materialize next.
	Ready []TxFrontier

	// InFlight are transactions already handed to the broadcaster and still
	// awaiting confirmation.
	InFlight []TxFrontier

	// Blocked are transactions that still have unmet in-proof dependencies.
	Blocked []BlockedTx

	// TargetConfirmed is true once the target transaction itself
	// is confirmed.
	TargetConfirmed bool

	// TargetConfirmHeight is populated once the target confirms.
	TargetConfirmHeight fn.Option[int32]

	// CSV is populated once the target confirms.
	CSV fn.Option[CSVInfo]

	// AllProofConfirmed is true once every proof node is confirmed.
	AllProofConfirmed bool

	// NeedSweep is true when the target is CSV-mature and the sweep has not
	// yet been broadcast.
	NeedSweep bool

	// Done is true once the final sweep confirms.
	Done bool

	// Sweep carries the durable sweep state for the target.
	Sweep SweepState
}

// CSVInfo describes the target's CSV maturity state.
type CSVInfo struct {
	// TargetConfirmHeight is the block height at which the target
	// confirmed.
	TargetConfirmHeight int32

	// MaturityHeight is the block height at which the target becomes
	// timeout-spendable.
	MaturityHeight int32

	// BlocksRemaining is how many blocks remain until maturity.
	BlocksRemaining int32

	// Ready is true once the current height is at or past maturity.
	Ready bool
}

// TxFrontier describes one proof transaction and the parents it depends on.
type TxFrontier struct {
	// Txid is the proof transaction hash.
	Txid chainhash.Hash

	// Node is the immutable proof node for this transaction.
	Node *recovery.Node

	// Layer is the topological layer index within the proof graph.
	Layer int

	// ParentTxids are the in-proof parents that must already be confirmed.
	ParentTxids []chainhash.Hash
}

// BlockedTx describes one proof transaction that is not yet ready.
type BlockedTx struct {
	// TxFrontier contains the immutable proof node details.
	TxFrontier

	// MissingParents are the in-proof parents that are still unconfirmed.
	MissingParents []chainhash.Hash
}

// Plan evaluates the proof at a given height and returns the current
// frontier.
//
// The algorithm walks the proof's precomputed topological layers in order
// (roots first, target last) and classifies each non-confirmed txid into
// exactly one bucket:
//
//   - confirmed: skipped (nothing for the caller to do)
//   - in flight: the caller told us it was handed to the broadcaster; we
//     still surface it in the snapshot so the caller can track it to
//     confirmation.
//   - ready:     every in-proof parent is confirmed — safe to broadcast.
//   - blocked:   has at least one unconfirmed parent; we include the
//     list of missing parents so the caller knows what to wait for.
//
// After the layer walk we derive the post-materialization state: is the
// target confirmed, is the CSV delay mature, does the caller need to
// broadcast the final sweep, or is the sweep already done.
//
// Validation runs unconditionally on every call — the planner re-derives
// everything from first principles and refuses to operate on inconsistent
// state. This is slightly more expensive than caching a validation flag
// but makes crash recovery trivially correct.
func (p *Planner) Plan(height int32, state *State) (*Snapshot, error) {
	if p == nil || p.proof == nil {
		return nil, fmt.Errorf("planner proof cannot be nil")
	}

	if state == nil {
		return nil, fmt.Errorf("state cannot be nil")
	}

	// Validate first. The planner never plans against a state that
	// violates an invariant, because any downstream broadcast decisions
	// would be incorrect by construction.
	if err := state.Validate(p.proof); err != nil {
		return nil, err
	}

	confirmed, err := hashSetFromSlice(
		state.ConfirmedTxids, "confirmed txids",
	)
	if err != nil {
		return nil, err
	}

	inflight, err := hashSetFromSlice(
		state.InFlightTxids, "in-flight txids",
	)
	if err != nil {
		return nil, err
	}

	snapshot := &Snapshot{
		Sweep:               copySweepState(state.Sweep),
		TargetConfirmHeight: state.TargetConfirmHeight,
	}

	layers := p.proof.Layers()
	for layerIndex, layer := range layers {
		for _, txid := range layer {
			node, _ := p.proof.Node(txid)
			parentTxids, err := p.proof.ParentTxids(txid)
			if err != nil {
				return nil, err
			}

			if confirmed.Contains(txid) {
				continue
			}

			if inflight.Contains(txid) {
				snapshot.InFlight = append(
					snapshot.InFlight, TxFrontier{
						Txid:        txid,
						Node:        node,
						Layer:       layerIndex,
						ParentTxids: parentTxids,
					},
				)

				continue
			}

			missingParents, err := missingParentsFromSet(
				p.proof, confirmed, txid,
			)
			if err != nil {
				return nil, err
			}

			if len(missingParents) == 0 {
				snapshot.Ready = append(
					snapshot.Ready, TxFrontier{
						Txid:        txid,
						Node:        node,
						Layer:       layerIndex,
						ParentTxids: parentTxids,
					},
				)

				continue
			}

			snapshot.Blocked = append(
				snapshot.Blocked, BlockedTx{
					TxFrontier: TxFrontier{
						Txid:        txid,
						Node:        node,
						Layer:       layerIndex,
						ParentTxids: parentTxids,
					},
					MissingParents: missingParents,
				},
			)
		}
	}

	snapshot.TargetConfirmed = confirmedTxidSetContains(
		confirmed, p.proof.TargetOutpoint().Hash,
	)
	snapshot.AllProofConfirmed = allProofTxidsConfirmed(
		p.proof, confirmed,
	)

	if snapshot.TargetConfirmed {
		csv, err := csvInfoAt(
			p.proof, height, state.TargetConfirmHeight,
		)
		if err != nil {
			return nil, err
		}

		snapshot.CSV = fn.Some(csv)
		snapshot.NeedSweep = csv.Ready &&
			snapshot.Sweep.Status == SweepStatusPending
	}

	snapshot.Done = snapshot.Sweep.Status == SweepStatusConfirmed

	sortFrontier(snapshot.Ready)
	sortFrontier(snapshot.InFlight)
	sortBlocked(snapshot.Blocked)

	return snapshot, nil
}

// csvInfoAt derives the target's CSV maturity view at the given block
// height. Uses int64 math (via recovery.ComputeMaturityHeight) to guarantee
// no silent int32 overflow even if the proof or state somehow carries
// values outside the usual recovery-package bounds.
//
// See recovery.ComputeMaturityHeight for the overflow reasoning; it is
// worth keeping the single overflow-safe implementation shared between
// packages so a fix to one side automatically benefits the other.
func csvInfoAt(proof *recovery.Proof, height int32,
	targetConfirmHeight fn.Option[int32]) (CSVInfo, error) {

	confirmHeight, err := targetConfirmHeight.UnwrapOrErr(
		fmt.Errorf("target confirm height cannot be nil"),
	)
	if err != nil {
		return CSVInfo{}, err
	}

	maturityHeight, err := recovery.ComputeMaturityHeight(
		confirmHeight, proof.CSVDelay(),
	)
	if err != nil {
		return CSVInfo{}, err
	}

	blocksRemaining := maturityHeight - height
	if blocksRemaining < 0 {
		blocksRemaining = 0
	}

	return CSVInfo{
		TargetConfirmHeight: confirmHeight,
		MaturityHeight:      maturityHeight,
		BlocksRemaining:     blocksRemaining,
		Ready:               height >= maturityHeight,
	}, nil
}

// allProofTxidsConfirmed returns true iff every node in the proof graph is in
// the confirmed set. The planner walks the layers-ordered traversal rather
// than iterating the nodes map so the check short-circuits at the topmost
// unconfirmed layer, which is usually cheaper than a full scan.
func allProofTxidsConfirmed(proof *recovery.Proof,
	confirmed fn.Set[chainhash.Hash]) bool {

	for _, layer := range proof.Layers() {
		for _, txid := range layer {
			if confirmed.Contains(txid) {
				continue
			}

			return false
		}
	}

	return true
}

// missingParentsFromSet lists the parents of txid that are NOT yet confirmed.
// An empty return value means the tx is ready to broadcast. Results are
// sorted by raw byte order so Snapshot output is deterministic.
func missingParentsFromSet(proof *recovery.Proof,
	confirmed fn.Set[chainhash.Hash],
	txid chainhash.Hash) ([]chainhash.Hash, error) {

	parentTxids, err := proof.ParentTxids(txid)
	if err != nil {
		return nil, err
	}

	missing := make([]chainhash.Hash, 0, len(parentTxids))
	for _, parent := range parentTxids {
		if confirmed.Contains(parent) {
			continue
		}

		missing = append(missing, parent)
	}

	sortHashes(missing)

	return missing, nil
}

// ensureParentsConfirmed errors if txid has any unconfirmed parent. It is the
// topological invariant enforced by Validate: a "confirmed" or "in-flight"
// tx in the persisted state must not depend on a still-pending parent.
func ensureParentsConfirmed(proof *recovery.Proof,
	confirmed fn.Set[chainhash.Hash], txid chainhash.Hash) error {

	missing, err := missingParentsFromSet(proof, confirmed, txid)
	if err != nil {
		return err
	}

	if len(missing) == 0 {
		return nil
	}

	return fmt.Errorf("tx %s has unconfirmed parents %v", txid, missing)
}

// validateSweepState checks that the durable sweep state is internally
// consistent with the confirmed set and the proof graph.
//
// # Sweep lifecycle model
//
// The sweep is the final transaction the caller broadcasts to convert
// the target outpoint into an address they control. Its three-state
// lifecycle is:
//
//	Pending      : nothing broadcast yet. Neither Txid nor ConfirmHeight
//	               is set. This is the only state where the planner will
//	               flip NeedSweep=true (once CSV matures).
//	Broadcasted  : the sweep was broadcast. Txid is set but
//	               ConfirmHeight is not. The target MUST be confirmed at
//	               this point; otherwise the sweep tx couldn't have been
//	               valid (it spends the target via a CSV-timelocked
//	               path).
//	Confirmed    : the sweep confirmed on-chain. Both Txid and
//	               ConfirmHeight are set. The target must be confirmed
//	               AND the sweep's confirm height must be at or past
//	               target_confirm_height + csv_delay.
//
// A Txid that collides with any proof node txid is rejected up front,
// regardless of status: a collision would leave the planner treating the
// same hash as both a confirmed proof node AND a sweep step, which is
// logically incoherent and masks genuine progress.
func validateSweepState(sweep SweepState, confirmed fn.Set[chainhash.Hash],
	proof *recovery.Proof, targetConfirmHeight fn.Option[int32]) error {

	// A sweep Txid that collides with a proof node would leave the planner
	// treating the same hash as both a confirmed proof node and a sweep
	// step. Reject eagerly regardless of status.
	collideErr := fn.MapOptionZ(sweep.Txid,
		func(txid chainhash.Hash) error {
			if _, collides := proof.Node(txid); collides {
				return fmt.Errorf("sweep txid %s collides "+
					"with proof node", txid)
			}

			return nil
		},
	)
	if collideErr != nil {
		return collideErr
	}

	switch sweep.Status {
	case SweepStatusPending:
		if sweep.Txid.IsSome() {
			return fmt.Errorf("pending sweep must not have a txid")
		}
		if sweep.ConfirmHeight.IsSome() {
			return fmt.Errorf("pending sweep must not have a " +
				"confirm height")
		}

	case SweepStatusBroadcasted:
		if sweep.Txid.IsNone() {
			return fmt.Errorf("broadcasted sweep must have a txid")
		}
		if sweep.ConfirmHeight.IsSome() {
			return fmt.Errorf("broadcasted sweep must not have a " +
				"confirm height")
		}

		// A broadcasted sweep is only reachable from a confirmed
		// target: the sweep tx input is the target outpoint with a CSV
		// delay, so the target must have matured before any valid
		// broadcast could land in the mempool. Rejecting this state
		// also prevents a tampered file from suppressing a legitimate
		// sweep-ready signal.
		if !confirmed.Contains(proof.TargetOutpoint().Hash) {
			return fmt.Errorf("broadcasted sweep requires " +
				"confirmed target")
		}

	case SweepStatusConfirmed:
		if sweep.Txid.IsNone() {
			return fmt.Errorf("confirmed sweep must have a txid")
		}
		confirmHeight, err := sweep.ConfirmHeight.UnwrapOrErr(
			fmt.Errorf("confirmed sweep must have a confirm " +
				"height"),
		)
		if err != nil {
			return err
		}
		if confirmHeight < 0 {
			return fmt.Errorf("sweep confirm height %d is negative",
				confirmHeight)
		}

		targetTxid := proof.TargetOutpoint().Hash
		if !confirmed.Contains(targetTxid) {
			return fmt.Errorf("confirmed sweep requires " +
				"confirmed target")
		}

		// A confirmed sweep tx could not have been mined before the
		// target's CSV maturity: the sweep spends the target via a
		// relative-timelocked path. If the persisted state claims
		// otherwise, the file is either tampered or logically
		// incoherent.
		targetHeight, err := targetConfirmHeight.UnwrapOrErr(
			fmt.Errorf("confirmed sweep requires target confirm " +
				"height"),
		)
		if err != nil {
			return err
		}
		maturityHeight, err := recovery.ComputeMaturityHeight(
			targetHeight, proof.CSVDelay(),
		)
		if err != nil {
			return fmt.Errorf("confirmed sweep: %w", err)
		}
		if confirmHeight < maturityHeight {
			return fmt.Errorf("sweep confirmed at height %d "+
				"before csv maturity %d", confirmHeight,
				maturityHeight)
		}

	default:
		return fmt.Errorf("unknown sweep status %d", sweep.Status)
	}

	return nil
}

// copySweepState returns a value-copy of the input. With fn.Option-valued
// fields the copy is already deep by virtue of the Option type wrapping the
// inner value, so the helper is now just a readability alias for the
// canonical "don't mutate the caller's SweepState" intent at call sites.
func copySweepState(s SweepState) SweepState {
	return SweepState{
		Status:        s.Status,
		Txid:          s.Txid,
		ConfirmHeight: s.ConfirmHeight,
	}
}

// hashSetFromSlice copies a slice of txids into an fn.Set, rejecting any
// duplicate entry. Using fn.Set rather than a bare map[chainhash.Hash]struct{}
// lets the rest of the planner use idiomatic Contains/Diff/Intersect calls
// without reinventing those helpers per-caller.
func hashSetFromSlice(values []chainhash.Hash,
	label string) (fn.Set[chainhash.Hash], error) {

	set := fn.NewSet[chainhash.Hash]()
	for _, value := range values {
		if set.Contains(value) {
			return nil, fmt.Errorf("duplicate %s entry %s", label,
				value)
		}

		set.Add(value)
	}

	return set, nil
}

// ensureDisjoint verifies no txid appears in both the confirmed and the
// in-flight sets. The two states are mutually exclusive by definition: a tx
// is either confirmed on-chain or still in the broadcaster's queue, never
// both. A state that claims otherwise is either tampered or the caller has a
// state-machine bug.
func ensureDisjoint(a, b fn.Set[chainhash.Hash]) error {
	overlap := a.Intersect(b)
	if overlap.IsEmpty() {
		return nil
	}

	// Pick any offender for the error message; iteration order is not
	// deterministic but the first collision is enough to identify the bug.
	for txid := range overlap {
		return fmt.Errorf("txid %s cannot be both confirmed and "+
			"in-flight", txid)
	}

	return nil
}

// confirmedTxidSetContains is preserved as a named helper (rather than an
// inline set.Contains call) because the planner reads much better when the
// "is this the confirmed target?" intent is spelled out at the call site.
func confirmedTxidSetContains(set fn.Set[chainhash.Hash],
	txid chainhash.Hash) bool {

	return set.Contains(txid)
}

// sortFrontier sorts transactions deterministically: ascending layer first,
// then by raw txid byte order within a layer. Raw-byte comparison avoids the
// per-comparison hex encoding that chainhash.Hash.String() performs.
func sortFrontier(frontier []TxFrontier) {
	sort.Slice(frontier, func(i, j int) bool {
		if frontier[i].Layer != frontier[j].Layer {
			return frontier[i].Layer < frontier[j].Layer
		}

		return bytes.Compare(
			frontier[i].Txid[:], frontier[j].Txid[:],
		) < 0
	})
}

// sortBlocked sorts blocked transactions deterministically using the same
// (layer, txid) ordering as sortFrontier.
func sortBlocked(frontier []BlockedTx) {
	sort.Slice(frontier, func(i, j int) bool {
		if frontier[i].Layer != frontier[j].Layer {
			return frontier[i].Layer < frontier[j].Layer
		}

		return bytes.Compare(
			frontier[i].Txid[:], frontier[j].Txid[:],
		) < 0
	})
}

// sortHashes sorts hashes deterministically by raw byte order.
func sortHashes(hashes []chainhash.Hash) {
	sort.Slice(hashes, func(i, j int) bool {
		return bytes.Compare(hashes[i][:], hashes[j][:]) < 0
	})
}
