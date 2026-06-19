package txconfirm

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
)

// feeBumpStateMachine is the protofsm instance that tracks the lifecycle of
// the fee-input fanout. There is a single instance per FeeBumpInputController:
// at most one fanout transaction is ever in flight, so the FSM models that
// fanout's life from broadcast through confirmation (or rejection) and back to
// idle.
type feeBumpStateMachine = protofsm.StateMachine[
	feeBumpEvent, feeBumpOutboxEvent, *feeBumpEnvironment,
]

// feeBumpStateTransition is the fanout protofsm transition type.
type feeBumpStateTransition = protofsm.StateTransition[
	feeBumpEvent, feeBumpOutboxEvent, *feeBumpEnvironment,
]

// feeBumpState is the sealed protofsm state interface for the fanout
// lifecycle.
type feeBumpState interface {
	protofsm.State[
		feeBumpEvent, feeBumpOutboxEvent, *feeBumpEnvironment,
	]

	feeBumpStateSealed()
}

// feeBumpEvent is the sealed event surface accepted by the fanout FSM.
type feeBumpEvent interface {
	feeBumpEventSealed()
}

// feeBumpOutboxEvent is the fanout outbox event surface.
//
// Like the tracked-tx FSM, the fanout FSM is intentionally pure and does not
// emit outbox events. The controller and actor drive all IO (wallet funding,
// broadcast, conf-watch registration) around the FSM, querying its state and
// feeding it events. The sealed interface still exists so the package follows
// the same protofsm shape as the rest of the codebase.
type feeBumpOutboxEvent interface {
	feeBumpOutboxEventSealed()
}

// feeBumpEnvironment carries immutable execution context for the fanout FSM
// instance.
type feeBumpEnvironment struct {
}

// pendingFeeInputFanout captures everything about a fanout transaction that is
// broadcast and awaiting confirmation. It is the payload carried by the
// fanout-pending FSM state.
type pendingFeeInputFanout struct {
	// txid is the fanout transaction hash used to match the confirmation
	// callback and the rebroadcast response.
	txid chainhash.Hash

	// tx is the fully funded fanout transaction kept for rebroadcasting on
	// the fee-bump interval.
	tx *wire.MsgTx

	// watchScript is the output script the actor registers a confirmation
	// watch on.
	watchScript []byte

	// assignments maps each blocked parent txid to the fanout outputs
	// reserved for it as predicted fee inputs.
	assignments map[chainhash.Hash][]wire.OutPoint

	// lastBroadcastHeight is the chain height of the most recent broadcast
	// attempt, used to pace rebroadcasts against the fee-bump interval.
	lastBroadcastHeight int32
}

// feeBumpFanoutBroadcast records that a fresh fanout transaction has been
// funded and broadcast and is now awaiting confirmation.
type feeBumpFanoutBroadcast struct {
	// pending is the newly broadcast fanout state to carry forward.
	pending *pendingFeeInputFanout
}

// feeBumpEventSealed marks feeBumpFanoutBroadcast as a fanout event.
func (e *feeBumpFanoutBroadcast) feeBumpEventSealed() {}

// feeBumpFanoutRebroadcast records that the in-flight fanout transaction was
// rebroadcast at a new height. It refreshes the pending state's last-broadcast
// height without otherwise disturbing the in-flight fanout.
type feeBumpFanoutRebroadcast struct {
	// height is the chain height at which the rebroadcast completed.
	height int32
}

// feeBumpEventSealed marks feeBumpFanoutRebroadcast as a fanout event.
func (e *feeBumpFanoutRebroadcast) feeBumpEventSealed() {}

// feeBumpFanoutConfirmed records that the in-flight fanout transaction has
// confirmed. The controller has already promoted the predicted outputs to
// used fee inputs before sending this event, so the FSM simply returns to
// idle.
type feeBumpFanoutConfirmed struct {
	// txid identifies the confirmed fanout transaction.
	txid chainhash.Hash
}

// feeBumpEventSealed marks feeBumpFanoutConfirmed as a fanout event.
func (e *feeBumpFanoutConfirmed) feeBumpEventSealed() {}

// feeBumpFanoutCleared records that the in-flight fanout has been abandoned:
// either the rebroadcast was rejected, or every parent it served was evicted.
// The controller releases the predicted outputs and wallet leases before
// sending this event, so the FSM simply returns to idle.
type feeBumpFanoutCleared struct{}

// feeBumpEventSealed marks feeBumpFanoutCleared as a fanout event.
func (e *feeBumpFanoutCleared) feeBumpEventSealed() {}

// feeBumpErrorReporter reports fanout FSM errors through the package logger.
type feeBumpErrorReporter struct {
	log btclog.Logger
}

// ReportError logs a fanout FSM execution error.
func (r *feeBumpErrorReporter) ReportError(err error) {
	r.log.Error("Fee-input fanout FSM error", err)
}

// newFeeBumpStateMachine creates a new protofsm state machine for the fanout
// lifecycle, starting in the idle state with no fanout in flight.
func newFeeBumpStateMachine(log btclog.Logger) *feeBumpStateMachine {
	cfg := protofsm.StateMachineCfg[
		feeBumpEvent, feeBumpOutboxEvent, *feeBumpEnvironment,
	]{
		Logger: log,
		ErrorReporter: &feeBumpErrorReporter{
			log: log,
		},
		InitialState: &feeBumpStateIdle{},
		Env:          &feeBumpEnvironment{},
	}

	fsm := protofsm.NewStateMachine(cfg)

	return &fsm
}

// feeBumpPendingFanout returns the in-flight fanout carried by the supplied
// state, or nil if the FSM is idle.
func feeBumpPendingFanout(state feeBumpState) *pendingFeeInputFanout {
	pending, ok := state.(*feeBumpStateFanoutPending)
	if !ok {
		return nil
	}

	return pending.pending
}
