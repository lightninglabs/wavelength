package recovery

import (
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// SessionState is the durable caller-owned state for one recovery session.
//
// # Persistence philosophy
//
// The proof graph itself is immutable and stored separately (encoded via
// proof_codec.go). Only caller observations that must survive restart live
// here. Restart recovery is thus a two-step rehydrate: decode the Proof,
// decode the SessionState, and call NewSessionFromState. The separation
// means the (expensive) graph validation only runs once, when the Proof is
// first built and persisted; subsequent restarts only pay for state
// validation against an already-validated graph.
//
// # Invariants (mirrored by validateSessionState)
//
//   - TxStates has an entry for every node in the proof.
//   - A node with TxStateConfirmed has a matching ConfirmHeights entry and
//     that height is non-negative.
//   - A node with TxStatePending or TxStateBroadcasted has NO
//     ConfirmHeights entry.
//   - A confirmed node's in-proof parents are all confirmed too (no
//     "dangling child" states).
//   - FailedTxid and LastError are either both set or both empty, and the
//     failed txid exists in the proof.
//
// These invariants are exactly the ones the Session state machine would
// have enforced at runtime; validating them on load means a caller cannot
// "sneak in" an inconsistent state by editing the blob directly.
type SessionState struct {
	// TxStates records the caller-observed state for each proof node.
	TxStates map[chainhash.Hash]TxState

	// ConfirmHeights records the confirmation height for confirmed nodes.
	ConfirmHeights map[chainhash.Hash]int32

	// FailedTxid is the node associated with the terminal failure, if any.
	FailedTxid fn.Option[chainhash.Hash]

	// LastError carries the terminal failure string, if any.
	LastError string
}

// NewSessionFromState constructs a session from immutable proof data and a
// previously exported caller-owned state snapshot. Validation runs before
// any copy so an invalid state can never produce a partially-constructed
// Session (which would be a landmine for any caller that inspected it).
//
// The LastError string is wrapped back into a sentinel `error` value via
// fmt.Errorf("%s", ...) — we deliberately lose the original error type,
// because the type is not serialized by ExportState and any claim to
// preserve it would be misleading.
func NewSessionFromState(proof *Proof, state *SessionState) (*Session, error) {
	if proof == nil {
		return nil, fmt.Errorf("proof cannot be nil")
	}

	if state == nil {
		return nil, fmt.Errorf("session state cannot be nil")
	}

	if err := validateSessionState(proof, state); err != nil {
		return nil, err
	}

	txStates := make(map[chainhash.Hash]TxState, len(state.TxStates))
	for txid, txState := range state.TxStates {
		txStates[txid] = txState
	}

	confirmHeights := make(
		map[chainhash.Hash]int32, len(state.ConfirmHeights),
	)
	for txid, height := range state.ConfirmHeights {
		confirmHeights[txid] = height
	}

	session := &Session{
		proof:          proof,
		txStates:       txStates,
		confirmHeights: confirmHeights,
		failedTxid:     state.FailedTxid,
	}

	if state.LastError != "" {
		session.lastError = fmt.Errorf("%s", state.LastError)
	}

	return session, nil
}

// ExportState returns a durable snapshot of the session's caller-owned state.
func (s *Session) ExportState() *SessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	txStates := make(map[chainhash.Hash]TxState, len(s.txStates))
	for txid, txState := range s.txStates {
		txStates[txid] = txState
	}

	confirmHeights := make(map[chainhash.Hash]int32,
		len(s.confirmHeights))
	for txid, height := range s.confirmHeights {
		confirmHeights[txid] = height
	}

	state := &SessionState{
		TxStates:       txStates,
		ConfirmHeights: confirmHeights,
		FailedTxid:     s.failedTxid,
	}

	if s.lastError != nil {
		state.LastError = s.lastError.Error()
	}

	return state
}

// validateSessionState checks that a durable session state is consistent
// with the immutable proof graph it claims to execute.
//
// # Why mirror the Session state machine here
//
// The Session state machine enforces every invariant at each transition
// (MarkBroadcasted, MarkConfirmed, MarkFailed). But a persisted state can
// also be produced by: (a) an earlier version of this code, (b) a caller
// editing the blob directly, (c) a bug in the TLV codec. So before we
// hydrate a SessionState into a Session we re-run every invariant that the
// state machine would have enforced. A state that passes Validate is
// guaranteed to behave the same whether it was reached via a series of
// Mark* calls or loaded from disk.
//
// The checks are ordered so the cheapest failures (nil maps, missing
// per-node entries) surface first, and the per-node topological invariant
// (confirmed child requires confirmed parent) runs last since it is the
// most expensive.
func validateSessionState(proof *Proof, state *SessionState) error {
	if state.TxStates == nil {
		return fmt.Errorf("tx states cannot be nil")
	}

	if state.ConfirmHeights == nil {
		return fmt.Errorf("confirm heights cannot be nil")
	}

	if state.FailedTxid.IsNone() != (state.LastError == "") {
		return fmt.Errorf("failed txid and last error must be set " +
			"together")
	}

	var failedErr error
	state.FailedTxid.WhenSome(func(txid chainhash.Hash) {
		if _, ok := proof.Node(txid); !ok {
			failedErr = fmt.Errorf("failed txid %s is not in proof",
				txid)
		}
	})
	if failedErr != nil {
		return failedErr
	}

	for txid := range proof.nodes {
		txState, ok := state.TxStates[txid]
		if !ok {
			return fmt.Errorf("missing tx state for %s", txid)
		}

		confirmHeight, hasConfirmHeight := state.ConfirmHeights[txid]

		switch txState {
		case TxStatePending, TxStateBroadcasted:
			if hasConfirmHeight {
				return fmt.Errorf("tx %s has unexpected "+
					"confirmation height", txid)
			}

		case TxStateConfirmed:
			if !hasConfirmHeight {
				return fmt.Errorf("confirmed tx %s missing "+
					"confirmation height", txid)
			}

			// A negative confirm height is never valid and, absent
			// this guard, overflows csv maturity arithmetic into
			// a small positive number that reports Ready=true.
			if confirmHeight < 0 {
				return fmt.Errorf("confirmed tx %s has "+
					"negative height %d", txid,
					confirmHeight)
			}

		default:
			return fmt.Errorf("unknown tx state %d for %s", txState,
				txid)
		}
	}

	for txid := range state.TxStates {
		if _, ok := proof.Node(txid); ok {
			continue
		}

		return fmt.Errorf("tx state contains unknown txid %s", txid)
	}

	for txid := range state.ConfirmHeights {
		if _, ok := proof.Node(txid); ok {
			continue
		}

		return fmt.Errorf("confirm heights contains unknown txid %s",
			txid)
	}

	// A confirmed node may not have any unconfirmed parent. This is the
	// topological invariant that the Session state machine enforces in
	// MarkConfirmed; we mirror it here so a persisted state that bypassed
	// the state machine (e.g. direct JSON/TLV surgery, or a coding bug)
	// cannot pass validation.
	for txid, txState := range state.TxStates {
		if txState != TxStateConfirmed {
			continue
		}

		parents, err := proof.ParentTxids(txid)
		if err != nil {
			return err
		}

		for _, parent := range parents {
			if state.TxStates[parent] == TxStateConfirmed {
				continue
			}

			return fmt.Errorf("tx %s confirmed with unconfirmed "+
				"parent %s", txid, parent)
		}
	}

	return nil
}
