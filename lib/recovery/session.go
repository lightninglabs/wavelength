package recovery

import (
	"fmt"
	"math"
	"sync"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// TxState describes the caller-observed state of one recovery transaction.
type TxState int

const (
	// TxStatePending means the caller has not yet broadcast or confirmed
	// the transaction.
	TxStatePending TxState = iota

	// TxStateBroadcasted means the caller broadcast the transaction and is
	// now waiting for confirmation.
	TxStateBroadcasted

	// TxStateConfirmed means the caller observed this transaction
	// confirmed on-chain.
	TxStateConfirmed
)

// String returns the stable debug label for a TxState.
func (s TxState) String() string {
	switch s {
	case TxStatePending:
		return "pending"

	case TxStateBroadcasted:
		return "broadcasted"

	case TxStateConfirmed:
		return "confirmed"

	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// SessionStatus is the high-level state of a recovery session.
type SessionStatus int

const (
	// SessionStatusMaterializing means some proof nodes still need
	// broadcasting or confirmation.
	SessionStatusMaterializing SessionStatus = iota

	// SessionStatusAwaitingCSV means the target confirmed but its CSV delay
	// has not yet matured.
	SessionStatusAwaitingCSV

	// SessionStatusSweepReady means the target is now spendable by timeout.
	SessionStatusSweepReady

	// SessionStatusFailed means the caller reported a terminal error.
	SessionStatusFailed
)

// String returns the stable debug label for a SessionStatus.
func (s SessionStatus) String() string {
	switch s {
	case SessionStatusMaterializing:
		return "materializing"

	case SessionStatusAwaitingCSV:
		return "awaiting_csv"

	case SessionStatusSweepReady:
		return "sweep_ready"

	case SessionStatusFailed:
		return "failed"

	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// BroadcastAction describes one transaction the caller can broadcast now.
type BroadcastAction struct {
	// Txid is the transaction hash of this action.
	Txid chainhash.Hash

	// Node is the recovery node to materialize.
	Node *Node

	// Layer is the topological layer of this tx within the proof graph.
	Layer int

	// ParentTxids are the in-proof parents that must already be confirmed.
	ParentTxids []chainhash.Hash
}

// BlockedAction describes one pending transaction that still has unmet
// dependencies.
type BlockedAction struct {
	// Txid is the blocked transaction hash.
	Txid chainhash.Hash

	// Layer is the topological layer of this tx within the proof graph.
	Layer int

	// MissingParents are the parent txids that are not yet confirmed.
	MissingParents []chainhash.Hash
}

// CSVStatus describes the target's CSV maturity state.
type CSVStatus struct {
	// TargetConfirmHeight is the block height at which the target
	// transaction confirmed.
	TargetConfirmHeight int32

	// MaturityHeight is the block height at which the target becomes
	// timeout-spendable.
	MaturityHeight int32

	// BlocksRemaining is how many blocks remain until maturity.
	BlocksRemaining int32

	// Ready is true once the current height is at or past maturity.
	Ready bool
}

// Snapshot is the caller-facing view of a session at one block height.
type Snapshot struct {
	// Status is the high-level session status.
	Status SessionStatus

	// ReadyToBroadcast are the transactions the caller can materialize now.
	ReadyToBroadcast []BroadcastAction

	// AwaitingConfirmation are already broadcast transactions still waiting
	// for confirmation.
	AwaitingConfirmation []chainhash.Hash

	// Blocked are pending transactions with unmet in-proof dependencies.
	Blocked []BlockedAction

	// CSV is populated once the target has confirmed.
	CSV *CSVStatus

	// FailedTxid is the txid associated with a terminal error, if any.
	FailedTxid *chainhash.Hash

	// LastError is the terminal error reported by the caller, if any.
	LastError error
}

// Session is a pure planning object driven by caller-reported observations.
//
// Session methods are safe for concurrent use. An RWMutex guards the mutable
// maps; readers (SnapshotAt, ExportState) hold an RLock and writers
// (MarkBroadcasted, MarkConfirmed, MarkFailed) hold the write lock for the
// duration of the call.
type Session struct {
	mu sync.RWMutex

	proof          *Proof
	txStates       map[chainhash.Hash]TxState
	confirmHeights map[chainhash.Hash]int32
	failedTxid     *chainhash.Hash
	lastError      error
}

// NewSession constructs a session for one immutable recovery proof. The
// constructor validates only that the proof is non-nil; per-node state is
// initialized to TxStatePending.
func NewSession(proof *Proof) (*Session, error) {
	if proof == nil {
		return nil, fmt.Errorf("proof cannot be nil")
	}

	txStates := make(map[chainhash.Hash]TxState, len(proof.nodes))
	for txid := range proof.nodes {
		txStates[txid] = TxStatePending
	}

	return &Session{
		proof:    proof,
		txStates: txStates,
		confirmHeights: make(
			map[chainhash.Hash]int32, len(proof.nodes),
		),
	}, nil
}

// Proof returns the immutable proof this session is driving.
func (s *Session) Proof() *Proof {
	return s.proof
}

// MarkBroadcasted records that the caller broadcast a ready transaction.
func (s *Session) MarkBroadcasted(txid chainhash.Hash) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.lastError != nil {
		return fmt.Errorf("session is failed: %w", s.lastError)
	}

	node, ok := s.proof.Node(txid)
	if !ok || node == nil {
		return fmt.Errorf("unknown txid %s", txid)
	}

	state := s.txStates[txid]
	if state == TxStateConfirmed {
		return fmt.Errorf("tx %s already confirmed", txid)
	}

	if state == TxStateBroadcasted {
		return fmt.Errorf("tx %s already broadcasted", txid)
	}

	ready, err := s.isReady(txid)
	if err != nil {
		return err
	}

	if !ready {
		return fmt.Errorf("tx %s is not ready to broadcast", txid)
	}

	s.txStates[txid] = TxStateBroadcasted

	return nil
}

// MarkConfirmed records that the caller observed a tx confirmed on-chain.
// The transition is rejected if the session is failed, if the tx was never
// broadcast, if any in-proof parent is still unconfirmed, if the height is
// negative, or if the caller attempts to re-confirm at a different height.
// A repeat call at the original height is idempotent so that redundant chain
// notifications do not surface as errors.
func (s *Session) MarkConfirmed(txid chainhash.Hash, height int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.lastError != nil {
		return fmt.Errorf("session is failed: %w", s.lastError)
	}

	node, ok := s.proof.Node(txid)
	if !ok || node == nil {
		return fmt.Errorf("unknown txid %s", txid)
	}

	if height < 0 {
		return fmt.Errorf("confirm height %d is negative", height)
	}

	switch s.txStates[txid] {
	case TxStatePending:
		return fmt.Errorf("tx %s cannot confirm before broadcast",
			txid)

	case TxStateConfirmed:
		// Idempotent at the original height; otherwise a caller bug.
		existing := s.confirmHeights[txid]
		if existing == height {
			return nil
		}

		return fmt.Errorf("tx %s already confirmed at height %d, "+
			"cannot reconfirm at %d", txid, existing, height)
	}

	ready, err := s.isReady(txid)
	if err != nil {
		return err
	}

	if !ready {
		return fmt.Errorf("tx %s cannot confirm with unconfirmed "+
			"parents", txid)
	}

	s.txStates[txid] = TxStateConfirmed
	s.confirmHeights[txid] = height

	return nil
}

// MarkFailed records a terminal failure reported by the caller. A session
// that has already been marked failed rejects subsequent MarkFailed calls so
// that a downstream symptom cannot overwrite and hide the root cause across a
// restart.
func (s *Session) MarkFailed(txid chainhash.Hash, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err == nil {
		return fmt.Errorf("failure error cannot be nil")
	}

	if s.lastError != nil {
		return fmt.Errorf("session already failed: %w", s.lastError)
	}

	node, ok := s.proof.Node(txid)
	if !ok || node == nil {
		return fmt.Errorf("unknown txid %s", txid)
	}

	failedTxid := txid
	s.failedTxid = &failedTxid
	s.lastError = err

	return nil
}

// SnapshotAt returns the current planning view at the given block height.
func (s *Session) SnapshotAt(height int32) (*Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ready := make([]BroadcastAction, 0)
	awaiting := make([]chainhash.Hash, 0)
	blocked := make([]BlockedAction, 0)

	for layerIndex, layer := range s.proof.layers {
		for _, txid := range layer {
			state := s.txStates[txid]

			switch state {
			case TxStateBroadcasted:
				awaiting = append(awaiting, txid)

			case TxStateConfirmed:
				continue

			case TxStatePending:
				missingParents, err := s.missingParents(txid)
				if err != nil {
					return nil, err
				}

				node, _ := s.proof.Node(txid)
				if len(missingParents) == 0 {
					parentTxids, err := s.proof.ParentTxids(
						txid,
					)
					if err != nil {
						return nil, err
					}

					ready = append(ready, BroadcastAction{
						Txid:        txid,
						Node:        node,
						Layer:       layerIndex,
						ParentTxids: parentTxids,
					})

					continue
				}

				blocked = append(blocked, BlockedAction{
					Txid:           txid,
					Layer:          layerIndex,
					MissingParents: missingParents,
				})
			}
		}
	}

	sortHashes(awaiting)

	snapshot := &Snapshot{
		Status:               SessionStatusMaterializing,
		ReadyToBroadcast:     ready,
		AwaitingConfirmation: awaiting,
		Blocked:              blocked,
		LastError:            s.lastError,
		FailedTxid:           s.failedTxid,
	}

	if s.lastError != nil {
		snapshot.Status = SessionStatusFailed
		snapshot.ReadyToBroadcast = nil
		snapshot.AwaitingConfirmation = nil
		snapshot.Blocked = nil

		// A terminal failure is an externally-reported state, not an
		// internal planning error. The failure is surfaced via
		// snapshot.LastError and Status=Failed so the caller can see
		// it without losing access to the snapshot.
		return snapshot, nil //nolint:nilerr
	}

	if !s.materializationComplete() {
		return snapshot, nil
	}

	csvStatus, err := s.csvStatusAt(height)
	if err != nil {
		return nil, err
	}

	snapshot.CSV = csvStatus
	if csvStatus.Ready {
		snapshot.Status = SessionStatusSweepReady
	} else {
		snapshot.Status = SessionStatusAwaitingCSV
	}

	return snapshot, nil
}

// isReady returns true once every in-proof parent is confirmed.
func (s *Session) isReady(txid chainhash.Hash) (bool, error) {
	missingParents, err := s.missingParents(txid)
	if err != nil {
		return false, err
	}

	return len(missingParents) == 0, nil
}

// missingParents returns the parent txids that are not yet confirmed.
func (s *Session) missingParents(txid chainhash.Hash) ([]chainhash.Hash,
	error) {

	parentTxids, err := s.proof.ParentTxids(txid)
	if err != nil {
		return nil, err
	}

	missing := make([]chainhash.Hash, 0, len(parentTxids))
	for _, parentTxid := range parentTxids {
		if s.txStates[parentTxid] == TxStateConfirmed {
			continue
		}

		missing = append(missing, parentTxid)
	}

	sortHashes(missing)

	return missing, nil
}

// materializationComplete returns true once every proof node is confirmed.
func (s *Session) materializationComplete() bool {
	for txid := range s.proof.nodes {
		if s.txStates[txid] != TxStateConfirmed {
			return false
		}
	}

	return true
}

// csvStatusAt derives the target's CSV maturity state at one block height.
func (s *Session) csvStatusAt(height int32) (*CSVStatus, error) {
	targetTxid := s.proof.TargetOutpoint().Hash
	targetConfirmHeight, ok := s.confirmHeights[targetTxid]
	if !ok {
		return nil, fmt.Errorf("target %s is not confirmed", targetTxid)
	}

	maturityHeight, err := ComputeMaturityHeight(
		targetConfirmHeight, s.proof.CSVDelay(),
	)
	if err != nil {
		return nil, err
	}

	blocksRemaining := maturityHeight - height
	if blocksRemaining < 0 {
		blocksRemaining = 0
	}

	return &CSVStatus{
		TargetConfirmHeight: targetConfirmHeight,
		MaturityHeight:      maturityHeight,
		BlocksRemaining:     blocksRemaining,
		Ready:               height >= maturityHeight,
	}, nil
}

// ComputeMaturityHeight returns targetConfirmHeight + csvDelay using int64
// arithmetic, rejecting any overflow past int32 range. This is a belt-and-
// braces guard: NewProof already caps csvDelay at MaxCSVDelay, and Proof
// constructors reject negative confirm heights, so overflow only becomes
// possible if a caller bypasses those guards. Exported so the unrollplan
// package and any other consumer reuses the single overflow-safe path.
func ComputeMaturityHeight(targetConfirmHeight int32, csvDelay uint32) (int32,
	error) {

	if targetConfirmHeight < 0 {
		return 0, fmt.Errorf("target confirm height %d is negative",
			targetConfirmHeight)
	}

	if csvDelay > MaxCSVDelay {
		return 0, fmt.Errorf("csv delay %d exceeds max %d",
			csvDelay, MaxCSVDelay)
	}

	maturity := int64(targetConfirmHeight) + int64(csvDelay)
	if maturity > math.MaxInt32 {
		return 0, fmt.Errorf("csv maturity %d overflows int32",
			maturity)
	}

	return int32(maturity), nil
}
