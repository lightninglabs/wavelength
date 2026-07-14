package txconfirm

import (
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
)

// feeBumpStateMachine is the protofsm instance that tracks and drives the
// lifecycle of the fee-input fanout. There is a single instance per
// TxBroadcasterActor: at most one fanout transaction is ever in flight, so the
// FSM models that fanout's life from broadcast through confirmation (or
// rejection) and back to idle.
//
// Unlike most protofsm instances in this codebase, the fanout FSM owns all of
// the fanout logic and performs its own IO directly inside transitions (wallet
// funding, broadcast, conf-watch decisions) via its feeBumpEnvironment. This is
// safe because the txconfirm actor serializes every event fed to this FSM, so
// at most one transition (and therefore at most one blocking Ask) is ever in
// flight: blocking in a transition has the same profile as a synchronous helper
// call from the actor. The FSM only emits outbox events for the handful of
// effects that must be applied by the actor itself (confirmation-watch
// register/unregister, and retrying stuck parents).
type feeBumpStateMachine = protofsm.StateMachine[
	feeBumpEvent, feeBumpOutboxEvent, *feeBumpEnvironment,
]

// feeBumpStateTransition is the fanout protofsm transition type.
type feeBumpStateTransition = protofsm.StateTransition[
	feeBumpEvent, feeBumpOutboxEvent, *feeBumpEnvironment,
]

// feeBumpEmittedEvent is the fanout protofsm emitted-event type carrying the
// outbox events a transition hands back to the actor.
type feeBumpEmittedEvent = protofsm.EmittedEvent[
	feeBumpEvent, feeBumpOutboxEvent,
]

// feeBumpState is the sealed protofsm state interface for the fanout
// lifecycle.
type feeBumpState interface {
	protofsm.State[
		feeBumpEvent, feeBumpOutboxEvent, *feeBumpEnvironment,
	]

	feeBumpStateSealed()
}

// feeBumpEvent is the sealed event surface the actor feeds into the fanout FSM.
type feeBumpEvent interface {
	feeBumpEventSealed()
}

// feeBumpOutboxEvent is the sealed outbox surface the fanout FSM hands back to
// the actor. The FSM does its own wallet/broadcast IO inside transitions, but
// effects that touch actor-owned resources — the chainsource confirmation watch
// and the tracked-tx retry loop — cannot be done from inside a transition, so
// they are returned as outbox events for the actor to apply.
type feeBumpOutboxEvent interface {
	feeBumpOutboxEventSealed()
}

// feeBumpEnvironment carries the execution context the fanout transitions use
// to reach the wallet, chainsource, and the shared per-parent reservation map.
// It holds a reference to the broadcaster so predicted/used fee-input
// accounting stays consistent with the CPFP-child build path, exactly as the
// old controller did.
type feeBumpEnvironment struct {
	// broadcaster provides the shared parentStates reservation map plus the
	// wallet/chainsource helpers the fanout build path reuses
	// (deriveChangePkScript, selectReservedFeeInput, releaseWalletLease,
	// parentState).
	broadcaster *CPFPBroadcaster

	// log is the package logger used for the fanout lifecycle log lines.
	log btclog.Logger

	// lastErr records the most recent operational error a transition hit
	// (a failed wallet fund, a rejected broadcast, a rewritten output).
	//
	// Transitions must NOT return these errors from ProcessEvent: protofsm
	// tears the whole state machine down on any transition error, and the
	// fanout FSM is a single long-lived instance that must survive a
	// transient fanout failure and stay ready for the next demand. Instead
	// a failing transition stashes the error here (staying in a safe state)
	// and the actor / test seam reads it back via takeLastErr after the
	// AskEvent completes. A genuinely impossible event (default branch) is
	// still returned as an error, since that is a programming bug worth
	// crashing on.
	lastErr error
}

// takeLastErr returns and clears the most recent operational error stashed by a
// transition, so each AskEvent surfaces only the error from its own turn.
func (env *feeBumpEnvironment) takeLastErr() error {
	err := env.lastErr
	env.lastErr = nil

	return err
}

// feeInputDemand records that one anchor parent needs a confirmed wallet fee
// input of at least minAmount before its CPFP child can be built. The actor
// computes the demand set from its tracked txids and hands it to the FSM; the
// FSM owns the supply decision.
type feeInputDemand struct {
	// parentTxid is the anchor parent that needs a fee input.
	parentTxid chainhash.Hash

	// minAmount is the smallest fee input that would unblock the parent.
	minAmount btcutil.Amount
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

// feeBumpDemandsObserved is fed to the FSM whenever a CPFP child broadcast hit
// ErrCPFPFeeInputUnavailable and the actor has computed the set of parents that
// currently need a confirmed fee input. The idle state turns this into a fresh
// fanout broadcast; the fanout-pending state turns it into a liveness check
// (rebroadcast) of the in-flight fanout.
type feeBumpDemandsObserved struct {
	// demands is the set of anchor parents that need a confirmed fee input.
	demands []feeInputDemand

	// feeRate is the shared fee rate (sat/vbyte) the fanout should size
	// against.
	feeRate int64

	// height is the actor's current best height, used to pace rebroadcasts.
	height int32

	// retryInterval is the fee-bump interval (in blocks) the rebroadcast
	// cadence is paced against.
	retryInterval int32
}

// feeBumpEventSealed marks feeBumpDemandsObserved as a fanout event.
func (e *feeBumpDemandsObserved) feeBumpEventSealed() {}

// feeBumpFanoutConfirmedEvent is fed to the FSM when a confirmation callback is
// observed. If the txid matches the in-flight fanout, the fanout-pending state
// promotes its predicted outputs into used fee inputs and returns to idle;
// otherwise it is a no-op self-loop.
type feeBumpFanoutConfirmedEvent struct {
	// txid identifies the transaction whose confirmation was observed.
	txid chainhash.Hash
}

// feeBumpEventSealed marks feeBumpFanoutConfirmedEvent as a fanout event.
func (e *feeBumpFanoutConfirmedEvent) feeBumpEventSealed() {}

// feeBumpParentEvicted is fed to the FSM when a tracked parent reaches a
// terminal state or is otherwise evicted. The fanout-pending state drops that
// parent's assignments; when no parents remain the whole fanout is released.
type feeBumpParentEvicted struct {
	// parentTxid is the parent that was evicted.
	parentTxid chainhash.Hash
}

// feeBumpEventSealed marks feeBumpParentEvicted as a fanout event.
func (e *feeBumpParentEvicted) feeBumpEventSealed() {}

// feeBumpWatchFanout instructs the actor to register a chainsource confirmation
// watch on the freshly broadcast fanout. It is emitted by the idle state when a
// new fanout is put on the wire.
type feeBumpWatchFanout struct {
	// txid is the fanout transaction to watch for confirmation.
	txid chainhash.Hash

	// watchScript is the output script the watch is keyed on.
	watchScript []byte
}

// feeBumpOutboxEventSealed marks feeBumpWatchFanout as a fanout outbox event.
func (e *feeBumpWatchFanout) feeBumpOutboxEventSealed() {}

// feeBumpUnwatchFanout instructs the actor to tear down the confirmation watch
// armed for a fanout that has now confirmed or been abandoned. It carries the
// same txid + script the watch was registered with so chainsource's
// txid+script-keyed lookup resolves the exact watch to cancel.
type feeBumpUnwatchFanout struct {
	// txid is the fanout transaction whose watch should be removed.
	txid chainhash.Hash

	// watchScript is the output script the watch was keyed on.
	watchScript []byte
}

// feeBumpOutboxEventSealed marks feeBumpUnwatchFanout as a fanout outbox event.
func (e *feeBumpUnwatchFanout) feeBumpOutboxEventSealed() {}

// feeBumpRetryParents instructs the actor to re-attempt every tracked tx still
// stuck in the Broadcasting state. It is emitted once a fanout confirms and
// fresh fee inputs become available, so the parents that were waiting on supply
// can finally build their CPFP children.
type feeBumpRetryParents struct{}

// feeBumpOutboxEventSealed marks feeBumpRetryParents as a fanout outbox event.
func (e *feeBumpRetryParents) feeBumpOutboxEventSealed() {}

// feeBumpErrorReporter reports fanout FSM errors through the package logger.
type feeBumpErrorReporter struct {
	log btclog.Logger
}

// ReportError logs a fanout FSM execution error.
func (r *feeBumpErrorReporter) ReportError(err error) {
	r.log.Error("Fee-input fanout FSM error", err)
}

// newFeeBumpStateMachine creates a new protofsm state machine for the fanout
// lifecycle, starting in the idle state with no fanout in flight. The supplied
// broadcaster provides the shared reservation map and the wallet/chainsource
// helpers the transitions use to do their own IO.
//
// The environment is returned alongside the machine so the actor can read back
// the per-turn operational error (env.takeLastErr) that transitions stash
// rather than return.
func newFeeBumpStateMachine(broadcaster *CPFPBroadcaster,
	log btclog.Logger) (*feeBumpStateMachine, *feeBumpEnvironment) {

	env := &feeBumpEnvironment{
		broadcaster: broadcaster,
		log:         log,
	}

	cfg := protofsm.StateMachineCfg[
		feeBumpEvent, feeBumpOutboxEvent, *feeBumpEnvironment,
	]{
		Logger: log,
		ErrorReporter: &feeBumpErrorReporter{
			log: log,
		},
		InitialState: &feeBumpStateIdle{},
		Env:          env,
	}

	fsm := protofsm.NewStateMachine(cfg)

	return &fsm, env
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
