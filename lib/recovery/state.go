package recovery

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
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
	FailedTxid *chainhash.Hash

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

	var failedTxid *chainhash.Hash
	if state.FailedTxid != nil {
		failed := *state.FailedTxid
		failedTxid = &failed
	}

	session := &Session{
		proof:          proof,
		txStates:       txStates,
		confirmHeights: confirmHeights,
		failedTxid:     failedTxid,
	}

	if state.LastError != "" {
		session.lastError = fmt.Errorf("%s", state.LastError)
	}

	return session, nil
}

// ExportState returns a durable snapshot of the session's caller-owned state.
func (s *Session) ExportState() *SessionState {
	txStates := make(map[chainhash.Hash]TxState, len(s.txStates))
	for txid, txState := range s.txStates {
		txStates[txid] = txState
	}

	confirmHeights := make(map[chainhash.Hash]int32,
		len(s.confirmHeights))
	for txid, height := range s.confirmHeights {
		confirmHeights[txid] = height
	}

	var failedTxid *chainhash.Hash
	if s.failedTxid != nil {
		failed := *s.failedTxid
		failedTxid = &failed
	}

	state := &SessionState{
		TxStates:       txStates,
		ConfirmHeights: confirmHeights,
		FailedTxid:     failedTxid,
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

	if (state.FailedTxid == nil) != (state.LastError == "") {
		return fmt.Errorf("failed txid and last error " +
			"must be set together")
	}

	if state.FailedTxid != nil {
		if _, ok := proof.Node(*state.FailedTxid); !ok {
			return fmt.Errorf("failed txid %s is not in proof",
				*state.FailedTxid)
		}
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

			_ = confirmHeight

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

	return nil
}
