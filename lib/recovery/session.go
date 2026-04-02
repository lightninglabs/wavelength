package recovery

import (
	"fmt"

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
type Session struct {
	proof          *Proof
	txStates       map[chainhash.Hash]TxState
	confirmHeights map[chainhash.Hash]int32
	failedTxid     *chainhash.Hash
	lastError      error
}

// NewSession constructs a session for one immutable recovery proof.
func NewSession(proof *Proof) *Session {
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
	}
}

// Proof returns the immutable proof this session is driving.
func (s *Session) Proof() *Proof {
	return s.proof
}

// MarkBroadcasted records that the caller broadcast a ready transaction.
func (s *Session) MarkBroadcasted(txid chainhash.Hash) error {
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
func (s *Session) MarkConfirmed(txid chainhash.Hash, height int32) error {
	if s.lastError != nil {
		return fmt.Errorf("session is failed: %w", s.lastError)
	}

	node, ok := s.proof.Node(txid)
	if !ok || node == nil {
		return fmt.Errorf("unknown txid %s", txid)
	}

	s.txStates[txid] = TxStateConfirmed
	s.confirmHeights[txid] = height

	return nil
}

// MarkFailed records a terminal failure reported by the caller.
func (s *Session) MarkFailed(txid chainhash.Hash, err error) error {
	if err == nil {
		return fmt.Errorf("failure error cannot be nil")
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
	ready := make([]BroadcastAction, 0)
	awaiting := make([]chainhash.Hash, 0)
	blocked := make([]BlockedAction, 0)

	for _, layer := range s.proof.layers {
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

				layerIndex, err := s.proof.Layer(txid)
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

		return snapshot, nil
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

	maturityHeight := targetConfirmHeight + int32(s.proof.CSVDelay())
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
