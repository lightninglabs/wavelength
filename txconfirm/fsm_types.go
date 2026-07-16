package txconfirm

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// trackedTxStateMachine is the protofsm instance used for one tracked txid.
type trackedTxStateMachine = protofsm.StateMachine[
	trackedTxEvent, trackedTxOutboxEvent, *trackedTxEnvironment,
]

// trackedTxStateTransition is the tracked-tx protofsm transition type.
type trackedTxStateTransition = protofsm.StateTransition[
	trackedTxEvent, trackedTxOutboxEvent, *trackedTxEnvironment,
]

// trackedTxState is the sealed protofsm state interface for one tracked txid.
type trackedTxState interface {
	protofsm.State[
		trackedTxEvent, trackedTxOutboxEvent, *trackedTxEnvironment,
	]

	trackedTxStateSealed()
}

// trackedTxEvent is the sealed event surface accepted by the tracked-tx FSM.
type trackedTxEvent interface {
	trackedTxEventSealed()
}

// trackedTxOutboxEvent is the tracked-tx outbox event surface.
//
// The tracked-tx FSM is intentionally pure and does not currently emit outbox
// events. The sealed interface still exists so the package follows the same
// protofsm shape as the rest of the codebase.
type trackedTxOutboxEvent interface {
	trackedTxOutboxEventSealed()
}

// trackedTxEnvironment carries immutable execution context for one tracked-tx
// FSM instance.
type trackedTxEnvironment struct {
	// Txid identifies the tracked transaction for logs and debugging.
	Txid chainhash.Hash
}

// trackedTxData is the immutable request data for one tracked txid.
type trackedTxData struct {
	// Tx is the fully signed transaction that should be confirmed.
	Tx *wire.MsgTx

	// Txid is the transaction hash used for deduplication.
	Txid chainhash.Hash

	// ConfirmationPkScript is the output script used for the confirmation
	// watch.
	ConfirmationPkScript []byte

	// Label is the optional human-readable broadcast label.
	Label string

	// HeightHint is the earliest height the transaction could confirm at.
	HeightHint uint32

	// TargetConfs is the required confirmation count.
	TargetConfs uint32

	// ParentFee is the absolute miner fee, in satoshis, that the tracked
	// transaction already pays on its own. It is used only for a funded-
	// anchor parent, where the CPFP child subtracts it so a fee bump lands
	// the combined parent+child fee on the target rate instead of
	// overshooting by the parent's own fee. Zero for zero-fee ephemeral
	// parents and for callers that do not supply it.
	ParentFee btcutil.Amount
}

// trackedTxProgress is the mutable per-broadcast progress carried by FSM
// states after the transaction has been submitted at least once.
type trackedTxProgress struct {
	// LastBroadcastHeight is the chain height at which the last submission
	// attempt completed. It is None until the tx has been submitted at
	// least once, so the retry/fee-bump interval check can distinguish "no
	// attempt yet" from a genuine attempt at height zero (e.g. on a fresh
	// chain or during early sync) rather than overloading a zero height as
	// the sentinel.
	LastBroadcastHeight fn.Option[int32]

	// CurrentFeeRate is the fee rate used by the latest submission attempt.
	CurrentFeeRate int64

	// BumpCount counts successful fee-bump rebroadcasts after the initial
	// submission.
	BumpCount int

	// BroadcastFailures counts consecutive initial-broadcast attempts that
	// reached no mempool at all (both the CPFP child sign and the
	// direct-parent fallback failed). It is reset to zero once a broadcast
	// succeeds and the tx advances to AwaitingConfirmation. The actor uses
	// it to escalate to the operator after repeated deterministic failures
	// without ever giving up on a tx that must still confirm.
	BroadcastFailures int

	// ChildTxid is the latest CPFP child txid when an anchor package was
	// built.
	ChildTxid *chainhash.Hash
}

// trackedTxBroadcastStarted records the start of the initial broadcast
// attempt.
type trackedTxBroadcastStarted struct{}

// trackedTxEventSealed marks trackedTxBroadcastStarted as a tracked-tx event.
func (e *trackedTxBroadcastStarted) trackedTxEventSealed() {}

// trackedTxBroadcastAccepted records that the current broadcast attempt
// completed successfully and the tx is now waiting for confirmation.
type trackedTxBroadcastAccepted struct {
	// Progress captures the latest broadcast metadata.
	Progress trackedTxProgress
}

// trackedTxEventSealed marks trackedTxBroadcastAccepted as a tracked-tx event.
func (e *trackedTxBroadcastAccepted) trackedTxEventSealed() {}

// trackedTxBroadcastFailed records that an initial broadcast attempt reached
// no mempool at all. It keeps the tx in the Broadcasting state (rather than
// falsely advancing to AwaitingConfirmation) so it is re-attempted on the
// next interval, and carries the updated progress including the incremented
// consecutive-failure counter.
type trackedTxBroadcastFailed struct {
	// Progress captures the broadcast metadata for the failed attempt,
	// including the bumped BroadcastFailures counter.
	Progress trackedTxProgress
}

// trackedTxEventSealed marks trackedTxBroadcastFailed as a tracked-tx event.
func (e *trackedTxBroadcastFailed) trackedTxEventSealed() {}

// trackedTxFeeBumpStarted records the start of a fee-bump rebroadcast
// attempt.
type trackedTxFeeBumpStarted struct{}

// trackedTxEventSealed marks trackedTxFeeBumpStarted as a tracked-tx event.
func (e *trackedTxFeeBumpStarted) trackedTxEventSealed() {}

// trackedTxConfirmed records terminal confirmation of the tracked txid.
type trackedTxConfirmed struct {
	// BlockHeight is the block height where the tx confirmed.
	BlockHeight int32
}

// trackedTxEventSealed marks trackedTxConfirmed as a tracked-tx event.
func (e *trackedTxConfirmed) trackedTxEventSealed() {}

// trackedTxFailed records a terminal failure for the tracked txid.
type trackedTxFailed struct {
	// Reason is the stable human-readable failure reason.
	Reason string
}

// trackedTxEventSealed marks trackedTxFailed as a tracked-tx event.
func (e *trackedTxFailed) trackedTxEventSealed() {}

// trackedTxReorged records that a previously delivered confirmation was
// reorged out of the canonical chain. Only valid from
// trackedTxStateConfirmed.
type trackedTxReorged struct{}

// trackedTxEventSealed marks trackedTxReorged as a tracked-tx event.
func (e *trackedTxReorged) trackedTxEventSealed() {}

// trackedTxFinalized records that a confirmation is past the backend's
// reorg-safety depth. Only valid from trackedTxStateConfirmed.
type trackedTxFinalized struct{}

// trackedTxEventSealed marks trackedTxFinalized as a tracked-tx event.
func (e *trackedTxFinalized) trackedTxEventSealed() {}

// trackedTxErrorReporter reports tracked-tx FSM errors through the package
// logger.
type trackedTxErrorReporter struct {
	log  btclog.Logger
	txid chainhash.Hash
}

// ReportError logs a tracked-tx FSM execution error.
func (r *trackedTxErrorReporter) ReportError(err error) {
	r.log.Error("Tracked tx FSM error", btclog.Hex("txid", r.txid[:]), err)
}

// newTrackedTxStateMachine creates a new protofsm state machine for one
// tracked txid.
func newTrackedTxStateMachine(log btclog.Logger,
	data trackedTxData) *trackedTxStateMachine {

	cfg := protofsm.StateMachineCfg[
		trackedTxEvent, trackedTxOutboxEvent, *trackedTxEnvironment,
	]{
		Logger: log,
		ErrorReporter: &trackedTxErrorReporter{
			log:  log,
			txid: data.Txid,
		},
		InitialState: &trackedTxStateNew{
			trackedTxData: data,
		},
		Env: &trackedTxEnvironment{
			Txid: data.Txid,
		},
	}

	fsm := protofsm.NewStateMachine(cfg)

	return &fsm
}

// txStateFromTrackedState projects an internal protofsm state into the public
// TxState status enum returned by the actor API.
func txStateFromTrackedState(state trackedTxState) TxState {
	switch state.(type) {
	case *trackedTxStateNew:
		return TxStateNew

	case *trackedTxStateBroadcasting:
		return TxStateBroadcasting

	case *trackedTxStateAwaitingConfirmation:
		return TxStateAwaitingConfirmation

	case *trackedTxStateFeeBumping:
		return TxStateFeeBumping

	case *trackedTxStateConfirmed:
		return TxStateConfirmed

	case *trackedTxStateFinalized:
		return TxStateFinalized

	case *trackedTxStateFailed:
		return TxStateFailed

	default:
		return TxStateFailed
	}
}

// currentTxState returns the tracked tx's projected public state.
func (t *trackedTx) currentTxState() (TxState, error) {
	state, err := t.currentFSMState()
	if err != nil {
		return TxStateFailed, err
	}

	return txStateFromTrackedState(state), nil
}

// currentFSMState returns the current protofsm state for one tracked tx.
func (t *trackedTx) currentFSMState() (trackedTxState, error) {
	if t.fsm == nil {
		return nil, fmt.Errorf("tracked tx fsm not initialized")
	}

	rawState, err := t.fsm.CurrentState()
	if err != nil {
		return nil, err
	}

	state, ok := rawState.(trackedTxState)
	if !ok {
		return nil, fmt.Errorf("unexpected tracked tx state %T",
			rawState)
	}

	return state, nil
}

// trackedTxLastBroadcastHeight returns the state's latest broadcast height, or
// None if the tx has not been submitted yet.
func trackedTxLastBroadcastHeight(state trackedTxState) fn.Option[int32] {
	switch s := state.(type) {
	case *trackedTxStateBroadcasting:
		return s.LastBroadcastHeight

	case *trackedTxStateAwaitingConfirmation:
		return s.LastBroadcastHeight

	case *trackedTxStateFeeBumping:
		return s.LastBroadcastHeight

	case *trackedTxStateConfirmed:
		return s.LastBroadcastHeight

	case *trackedTxStateFinalized:
		return s.LastBroadcastHeight

	case *trackedTxStateFailed:
		return s.LastBroadcastHeight

	default:
		return fn.None[int32]()
	}
}

// trackedTxBroadcastFailures returns the state's consecutive failed-broadcast
// count. Only the Broadcasting state tracks failures; every other state has
// either not started broadcasting or already reached a mempool, so the count
// is zero there.
func trackedTxBroadcastFailures(state trackedTxState) int {
	if s, ok := state.(*trackedTxStateBroadcasting); ok {
		return s.BroadcastFailures
	}

	return 0
}

// trackedTxConfirmHeight returns the state's confirmation height if the
// transaction has already confirmed.
func trackedTxConfirmHeight(state trackedTxState) (int32, bool) {
	switch s := state.(type) {
	case *trackedTxStateConfirmed:
		return s.ConfirmHeight, true

	case *trackedTxStateFinalized:
		return s.ConfirmHeight, true

	default:
		return 0, false
	}
}

// trackedTxFailureReason returns the state's terminal failure reason when
// available.
func trackedTxFailureReason(state trackedTxState) (string, bool) {
	failed, ok := state.(*trackedTxStateFailed)
	if !ok {
		return "", false
	}

	return failed.Reason, true
}
