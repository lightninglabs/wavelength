package recovery

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// SessionState is the durable caller-owned state for one recovery session.
//
// The proof graph itself is immutable and stored separately. This struct
// only captures caller observations that must survive restart.
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
// previously exported caller-owned state snapshot.
func NewSessionFromState(proof *Proof, state *SessionState) (*Session,
	error) {

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

	confirmHeights := make(map[chainhash.Hash]int32,
		len(state.ConfirmHeights))
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

// validateSessionState checks that a durable session state is consistent with
// the immutable proof graph it claims to execute.
func validateSessionState(proof *Proof, state *SessionState) error {
	if state.TxStates == nil {
		return fmt.Errorf("tx states cannot be nil")
	}

	if state.ConfirmHeights == nil {
		return fmt.Errorf("confirm heights cannot be nil")
	}

	if state.FailedTxid.IsNone() != (state.LastError == "") {
		return fmt.Errorf("failed txid and last error " +
			"must be set together")
	}

	var failedErr error
	state.FailedTxid.WhenSome(func(txid chainhash.Hash) {
		if _, ok := proof.Node(txid); !ok {
			failedErr = fmt.Errorf("failed txid %s is not "+
				"in proof", txid)
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
			return fmt.Errorf("unknown tx state %d for %s",
				txState, txid)
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
