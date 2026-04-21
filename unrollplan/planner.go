package unrollplan

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/lib/recovery"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// Planner evaluates unilateral-exit progress for one immutable recovery proof.
type Planner struct {
	proof *recovery.Proof
}

// NewPlanner creates a pure planner for one immutable recovery proof.
func NewPlanner(proof *recovery.Proof) (*Planner, error) {
	if proof == nil {
		return nil, fmt.Errorf("proof cannot be nil")
	}

	return &Planner{proof: proof}, nil
}

// Proof returns the immutable recovery proof the planner is evaluating.
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
			return fmt.Errorf("confirmed txid %s is not "+
				"in proof", txid)
		}
	}

	for txid := range inflight {
		if _, ok := proof.Node(txid); !ok {
			return fmt.Errorf("in-flight txid %s is not"+
				" in proof", txid)
		}
	}

	targetConfirmed := confirmedTxidSetContains(
		confirmed, proof.TargetOutpoint().Hash,
	)
	if s.TargetConfirmHeight.IsSome() && !targetConfirmed {
		return fmt.Errorf("target confirm height set " +
			"without confirmed target")
	}
	if targetConfirmed && s.TargetConfirmHeight.IsNone() {
		return fmt.Errorf("target confirmed without " +
			"target confirm height")
	}
	confirmHeightErr := fn.MapOptionZ(s.TargetConfirmHeight,
		func(h int32) error {
			if h < 0 {
				return fmt.Errorf("target confirm height "+
					"%d is negative", h)
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
			return fmt.Errorf(
				"in-flight tx %s: %w", txid, err,
			)
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

// Plan evaluates the proof at a given height and returns the current frontier.
func (p *Planner) Plan(height int32, state *State) (*Snapshot, error) {
	if p == nil || p.proof == nil {
		return nil, fmt.Errorf("planner proof cannot be nil")
	}

	if state == nil {
		return nil, fmt.Errorf("state cannot be nil")
	}

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

			if _, ok := confirmed[txid]; ok {
				continue
			}

			if _, ok := inflight[txid]; ok {
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

// csvInfoAt derives the target's CSV maturity view at the given block height.
// Uses int64 math to guarantee no silent int32 overflow even if the proof or
// state somehow carries values outside the usual recovery-package bounds.
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

func allProofTxidsConfirmed(proof *recovery.Proof,
	confirmed map[chainhash.Hash]struct{}) bool {

	for _, layer := range proof.Layers() {
		for _, txid := range layer {
			if _, ok := confirmed[txid]; ok {
				continue
			}

			return false
		}
	}

	return true
}

func missingParentsFromSet(proof *recovery.Proof,
	confirmed map[chainhash.Hash]struct{}, txid chainhash.Hash) (
	[]chainhash.Hash, error) {

	parentTxids, err := proof.ParentTxids(txid)
	if err != nil {
		return nil, err
	}

	missing := make([]chainhash.Hash, 0, len(parentTxids))
	for _, parent := range parentTxids {
		if _, ok := confirmed[parent]; ok {
			continue
		}

		missing = append(missing, parent)
	}

	sortHashes(missing)

	return missing, nil
}

func ensureParentsConfirmed(proof *recovery.Proof,
	confirmed map[chainhash.Hash]struct{}, txid chainhash.Hash) error {

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
// consistent with the confirmed set and the proof graph. It additionally
// rejects a sweep Txid that collides with any proof node txid and, for
// confirmed sweeps, requires the sweep confirm height to be at or past the
// target's CSV maturity.
func validateSweepState(sweep SweepState, confirmed map[chainhash.Hash]struct{},
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
			return fmt.Errorf("pending sweep must not " +
				"have a confirm height")
		}

	case SweepStatusBroadcasted:
		if sweep.Txid.IsNone() {
			return fmt.Errorf("broadcasted sweep must have a txid")
		}
		if sweep.ConfirmHeight.IsSome() {
			return fmt.Errorf("broadcasted sweep must " +
				"not have a confirm height")
		}

		// A broadcasted sweep is only reachable from a confirmed
		// target: the sweep tx input is the target outpoint with a CSV
		// delay, so the target must have matured before any valid
		// broadcast could land in the mempool. Rejecting this state
		// also prevents a tampered file from suppressing a legitimate
		// sweep-ready signal.
		if _, ok := confirmed[proof.TargetOutpoint().Hash]; !ok {
			return fmt.Errorf("broadcasted sweep requires " +
				"confirmed target")
		}

	case SweepStatusConfirmed:
		if sweep.Txid.IsNone() {
			return fmt.Errorf("confirmed sweep must have a txid")
		}
		confirmHeight, err := sweep.ConfirmHeight.UnwrapOrErr(
			fmt.Errorf("confirmed sweep must have a " +
				"confirm height"),
		)
		if err != nil {
			return err
		}
		if confirmHeight < 0 {
			return fmt.Errorf("sweep confirm height %d is "+
				"negative", confirmHeight)
		}

		targetTxid := proof.TargetOutpoint().Hash
		if _, ok := confirmed[targetTxid]; !ok {
			return fmt.Errorf("confirmed sweep requires " +
				"confirmed target")
		}

		// A confirmed sweep tx could not have been mined before the
		// target's CSV maturity: the sweep spends the target via a
		// relative-timelocked path. If the persisted state claims
		// otherwise, the file is either tampered or logically
		// incoherent.
		targetHeight, err := targetConfirmHeight.UnwrapOrErr(
			fmt.Errorf("confirmed sweep requires target " +
				"confirm height"),
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
				"before csv maturity %d",
				confirmHeight, maturityHeight)
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

func hashSetFromSlice(values []chainhash.Hash, label string) (
	map[chainhash.Hash]struct{}, error) {

	set := make(map[chainhash.Hash]struct{}, len(values))
	for _, value := range values {
		if _, ok := set[value]; ok {
			return nil, fmt.Errorf("duplicate %s entry %s", label,
				value)
		}

		set[value] = struct{}{}
	}

	return set, nil
}

func ensureDisjoint(a, b map[chainhash.Hash]struct{}) error {
	for txid := range a {
		if _, ok := b[txid]; ok {
			return fmt.Errorf("txid %s cannot be both "+
				"confirmed and in-flight",
				txid)
		}
	}

	return nil
}

func confirmedTxidSetContains(set map[chainhash.Hash]struct{},
	txid chainhash.Hash) bool {

	_, ok := set[txid]
	return ok
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
