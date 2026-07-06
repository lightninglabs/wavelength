package recovery

import (
	"fmt"
	"math"
	"sync"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// TxState describes the caller-observed state of one recovery transaction.
//
// The three states form a strict progression — Pending → Broadcasted →
// Confirmed — and the Session state machine enforces the order: a tx cannot
// be confirmed without first being broadcast. "Broadcasted" means the
// caller handed the tx to the mempool / broadcaster; it is not a chain-level
// observation, so callers may legitimately observe a tx confirmed even if
// they never called MarkBroadcasted themselves (for example, after a
// restart with stale state). This is why NewSessionFromState accepts states
// where TxStates[txid] is already Confirmed without requiring an earlier
// Broadcasted transition — the session was hydrated from persistence, not
// built step-by-step.
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

// SessionStatus is the high-level state of a recovery session. It is a
// convenience summary derived at snapshot time from the per-node TxStates
// plus any terminal error; callers should never persist the status as their
// source of truth — persist the per-node states and let Snapshot derive the
// status.
//
// Transition diagram:
//
//	Materializing ─┬─▶ AwaitingCSV ──▶ SweepReady
//	               │
//	               └───────────────▶ Failed (any time, caller-reported)
//
// The lifecycle is monotonic on the happy path: once every proof node is
// confirmed we're AwaitingCSV; once the chain passes the maturity height
// we're SweepReady. There is no "DoneSweeping" status here — the sweep
// itself is tracked in the `unrollplan` package. This package's scope ends
// once the target outpoint is timeout-spendable.
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
	CSV fn.Option[CSVStatus]

	// FailedTxid is the txid associated with a terminal error, if any.
	FailedTxid fn.Option[chainhash.Hash]

	// LastError is the terminal error reported by the caller, if any.
	LastError error
}

// Session is a pure planning object driven by caller-reported observations.
//
// The model is explicitly caller-driven. The Session does not subscribe to
// the chain, does not start goroutines, and does not produce side effects.
// Callers feed it three kinds of observations (broadcast, confirm, fail)
// and ask it for a Snapshot whenever they need the current plan. This
// inversion of control keeps the session portable across different
// broadcaster / mempool / watchtower implementations and makes its behavior
// trivially deterministic under tests.
//
// # State machine
//
// Per-tx: Pending ─▶ Broadcasted ─▶ Confirmed (parent-confirmed guarded).
// Per-session: terminal `lastError` latches on first MarkFailed and is
// never overwritten; subsequent MarkFailed calls fail so the root cause
// survives a restart.
//
// # Concurrency
//
// Session methods are safe for concurrent use. An RWMutex guards the
// mutable maps; readers (SnapshotAt, ExportState) hold an RLock and writers
// (MarkBroadcasted, MarkConfirmed, MarkFailed) hold the write lock for the
// duration of the call. Internal helpers (isReady, missingParents,
// materializationComplete, csvStatusAt) assume the caller already holds a
// lock — they do NOT acquire one themselves, because the Go RWMutex does
// not support re-entrancy.
type Session struct {
	mu sync.RWMutex

	proof          *Proof
	txStates       map[chainhash.Hash]TxState
	confirmHeights map[chainhash.Hash]int32
	failedTxid     fn.Option[chainhash.Hash]
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
// It enforces the three preconditions that make "broadcast" meaningful:
//
//  1. The session is not in a terminal failure state.
//  2. The tx belongs to this proof graph.
//  3. Every in-proof parent is confirmed.
//
// Idempotency is deliberately NOT granted here: a second call for the same
// txid returns an "already broadcasted" error rather than a silent no-op.
// This surfaces caller bugs (e.g. double-scheduling the same tx in a
// broadcast queue) quickly instead of masking them.
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

	// Parents must all be confirmed before we pay the fee to broadcast
	// this tx. If we allowed out-of-order broadcast, the mempool would
	// reject this tx as "missing inputs" and we'd lose visibility into
	// which parent actually needs to go first.
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
//
// # Why each guard exists
//
//   - "session is failed": once a terminal failure has been reported, we
//     stop advancing the session so the caller's failure-handling code
//     path runs to completion before any new chain observations replace
//     the root-cause error.
//   - "cannot confirm before broadcast": child-confirmed-without-parent is
//     the exact shape that enables the int32-overflow / instant-sweep
//     class of bugs described in the C-findings on the PR review. The
//     state machine refuses to produce it.
//   - "cannot confirm with unconfirmed parents": even if the caller
//     bypassed MarkBroadcasted (e.g. via a fake backend), a child cannot
//     confirm before its parent on a canonical chain. A state claiming
//     otherwise is either tampered or caused by a bug in the caller's
//     chain reorg handling, and we refuse to proceed.
//   - "cannot reconfirm at different height": if the caller observes the
//     same tx at two different heights, they are either watching two
//     chains or there was a reorg; either way they should use a reorg
//     API to revert the old height before reconfirming. Silently
//     overwriting would invite drift between Session state and the chain.
//   - Idempotency at the same height is granted because redundant chain
//     notifications (e.g. on connection re-establishment) are common and
//     should not surface as user-visible errors.
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
		return fmt.Errorf("tx %s cannot confirm before broadcast", txid)

	case TxStateBroadcasted:
		// Fall through to the post-switch block where we verify
		// parents are confirmed before applying the transition.

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
		return fmt.Errorf("tx %s cannot confirm with "+
			"unconfirmed parents", txid)
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

	s.failedTxid = fn.Some(txid)
	s.lastError = err

	return nil
}

// SnapshotAt returns the current planning view at the given block height.
//
// The walk visits the proof's layers in topological order (roots first) and
// classifies each pending tx into one of three buckets:
//
//   - "ready": every in-proof parent is confirmed — the caller can
//     broadcast this tx next.
//   - "blocked": at least one parent is still unconfirmed — the snapshot
//     lists the missing parents so the caller knows what to wait for.
//   - "awaiting confirmation": the tx was already broadcast; we're waiting
//     for the chain.
//
// Confirmed txs are intentionally omitted from the snapshot — there is
// nothing to do about them. Once every node is confirmed we derive the
// target's CSV maturity and flip the session status accordingly.
//
// The walk is O(N) over the graph on every call. At expected recovery
// sizes (hundreds of nodes at most) this is fine and it keeps the logic
// straightforward. For larger proofs a caller could cache the last
// snapshot and invalidate on each Mark* transition, but that optimization
// is not needed today.
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
		return snapshot, nil
	}

	if !s.materializationComplete() {
		return snapshot, nil
	}

	csvStatus, err := s.csvStatusAt(height)
	if err != nil {
		return nil, err
	}

	snapshot.CSV = fn.Some(csvStatus)
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
func (s *Session) csvStatusAt(height int32) (CSVStatus, error) {
	targetTxid := s.proof.TargetOutpoint().Hash
	targetConfirmHeight, ok := s.confirmHeights[targetTxid]
	if !ok {
		return CSVStatus{}, fmt.Errorf("target %s is not confirmed",
			targetTxid)
	}

	maturityHeight, err := ComputeMaturityHeight(
		targetConfirmHeight, s.proof.CSVDelay(),
	)
	if err != nil {
		return CSVStatus{}, err
	}

	blocksRemaining := maturityHeight - height
	if blocksRemaining < 0 {
		blocksRemaining = 0
	}

	return CSVStatus{
		TargetConfirmHeight: targetConfirmHeight,
		MaturityHeight:      maturityHeight,
		BlocksRemaining:     blocksRemaining,
		Ready:               height >= maturityHeight,
	}, nil
}

// ComputeMaturityHeight returns targetConfirmHeight + csvDelay using int64
// arithmetic, rejecting any overflow past int32 range.
//
// # Why this is its own function
//
// The naive expression `targetConfirmHeight + int32(csvDelay)` has two
// classes of bugs:
//
//  1. Signed overflow: a targetConfirmHeight close to MaxInt32 plus a
//     non-trivial csvDelay wraps into a NEGATIVE number, and downstream
//     code that compares "current >= maturity" reads Ready=true
//     indefinitely.
//  2. Unsigned-to-signed overflow: `int32(uint32)` where the uint32 has
//     its high bit set flips sign. `int32(MaxUint32) == -1`, so
//     `targetConfirmHeight + (-1) == targetConfirmHeight - 1`, and a
//     tampered csvDelay reports Ready=true about 136 years early.
//
// NewProof already caps csvDelay at MaxCSVDelay and
// validateSessionState / unrollplan.State.Validate reject negative
// targetConfirmHeight, so overflow is only reachable if a caller bypasses
// both guards. But this function is exported to let unrollplan reuse the
// same overflow-safe path and to keep the "belt and braces" property that
// even a buggy caller cannot construct an instant-sweep maturity height.
//
// This is exported so the unrollplan package and any other consumer
// reuses the single overflow-safe path.
func ComputeMaturityHeight(targetConfirmHeight int32,
	csvDelay uint32) (int32, error) {

	if targetConfirmHeight < 0 {
		return 0, fmt.Errorf("target confirm height %d is negative",
			targetConfirmHeight)
	}

	if csvDelay > MaxCSVDelay {
		return 0, fmt.Errorf("csv delay %d exceeds max %d", csvDelay,
			MaxCSVDelay)
	}

	maturity := int64(targetConfirmHeight) + int64(csvDelay)
	if maturity > math.MaxInt32 {
		return 0, fmt.Errorf("csv maturity %d overflows int32",
			maturity)
	}

	return int32(maturity), nil
}
