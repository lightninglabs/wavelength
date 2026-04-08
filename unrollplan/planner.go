package unrollplan

import (
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/lib/recovery"
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
type State struct {
	// ConfirmedTxids lists proof txids the caller has observed
	// confirmed.
	ConfirmedTxids []chainhash.Hash `json:"confirmed_txids"`

	// InFlightTxids lists proof txids currently being
	// materialized by the caller but not yet observed confirmed.
	InFlightTxids []chainhash.Hash `json:"in_flight_txids"`

	// TargetConfirmHeight records the confirmation height of the
	// target tx.
	TargetConfirmHeight *int32 `json:"target_confirm_height"`

	// Sweep records the final sweep lifecycle state.
	Sweep SweepState `json:"sweep"`
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
	Status SweepStatus `json:"status"`

	// Txid records the sweep txid once broadcast.
	Txid *chainhash.Hash `json:"txid"`

	// ConfirmHeight records the sweep confirmation height once
	// known.
	ConfirmHeight *int32 `json:"confirm_height"`
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

	if s.TargetConfirmHeight != nil && !confirmedTxidSetContains(
		confirmed, proof.TargetOutpoint().Hash,
	) {

		return fmt.Errorf("target confirm height set " +
			"without confirmed target")
	}

	if err := validateSweepState(s.Sweep, confirmed, proof); err != nil {
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
	TargetConfirmHeight *int32

	// CSV is populated once the target confirms.
	CSV *CSVInfo

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
		Sweep: copySweepState(state.Sweep),
		TargetConfirmHeight: copyInt32Ptr(
			state.TargetConfirmHeight,
		),
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

		snapshot.CSV = csv
		snapshot.NeedSweep = csv.Ready &&
			snapshot.Sweep.Status == SweepStatusPending
	}

	snapshot.Done = snapshot.Sweep.Status == SweepStatusConfirmed

	sortFrontier(snapshot.Ready)
	sortFrontier(snapshot.InFlight)
	sortBlocked(snapshot.Blocked)

	return snapshot, nil
}

func csvInfoAt(proof *recovery.Proof, height int32,
	targetConfirmHeight *int32) (*CSVInfo, error) {

	if targetConfirmHeight == nil {
		return nil, fmt.Errorf("target confirm height cannot be nil")
	}

	maturityHeight := *targetConfirmHeight + int32(proof.CSVDelay())
	blocksRemaining := maturityHeight - height
	if blocksRemaining < 0 {
		blocksRemaining = 0
	}

	return &CSVInfo{
		TargetConfirmHeight: *targetConfirmHeight,
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

func validateSweepState(sweep SweepState, confirmed map[chainhash.Hash]struct{},
	proof *recovery.Proof) error {

	switch sweep.Status {
	case SweepStatusPending:
		if sweep.Txid != nil {
			return fmt.Errorf("pending sweep must not have a txid")
		}
		if sweep.ConfirmHeight != nil {
			return fmt.Errorf("pending sweep must not " +
				"have a confirm height")
		}

	case SweepStatusBroadcasted:
		if sweep.Txid == nil {
			return fmt.Errorf("broadcasted sweep must have a txid")
		}
		if sweep.ConfirmHeight != nil {
			return fmt.Errorf("broadcasted sweep must " +
				"not have a confirm height")
		}

	case SweepStatusConfirmed:
		if sweep.Txid == nil {
			return fmt.Errorf("confirmed sweep must have a txid")
		}
		if sweep.ConfirmHeight == nil {
			return fmt.Errorf("confirmed sweep must " +
				"have a confirm height")
		}

		targetTxid := proof.TargetOutpoint().Hash
		if _, ok := confirmed[targetTxid]; !ok {
			return fmt.Errorf("confirmed sweep requires " +
				"confirmed target")
		}

	default:
		return fmt.Errorf("unknown sweep status %d", sweep.Status)
	}

	return nil
}

func copySweepState(s SweepState) SweepState {
	next := SweepState{Status: s.Status}
	if s.Txid != nil {
		txid := *s.Txid
		next.Txid = &txid
	}
	if s.ConfirmHeight != nil {
		height := *s.ConfirmHeight
		next.ConfirmHeight = &height
	}

	return next
}

func copyInt32Ptr(v *int32) *int32 {
	if v == nil {
		return nil
	}

	next := *v

	return &next
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

func sortFrontier(frontier []TxFrontier) {
	sort.Slice(frontier, func(i, j int) bool {
		if frontier[i].Layer != frontier[j].Layer {
			return frontier[i].Layer < frontier[j].Layer
		}

		return frontier[i].Txid.String() < frontier[j].Txid.String()
	})
}

func sortBlocked(frontier []BlockedTx) {
	sort.Slice(frontier, func(i, j int) bool {
		if frontier[i].Layer != frontier[j].Layer {
			return frontier[i].Layer < frontier[j].Layer
		}

		return frontier[i].Txid.String() < frontier[j].Txid.String()
	})
}

func sortHashes(hashes []chainhash.Hash) {
	sort.Slice(hashes, func(i, j int) bool {
		return hashes[i].String() < hashes[j].String()
	})
}
