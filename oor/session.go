package oor

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
)

// Session groups a running OOR transfer FSM with its stable identifier.
type Session struct {
	// ID is the stable v0 session identifier (Ark txid).
	ID SessionID

	// FSM is the running state machine for this session.
	FSM *StateMachine
}

// NewSession creates a new outgoing OOR transfer session and returns the first
// outbox request produced by the FSM.
//
// This helper lets the FSM build the submit package itself, then derives the
// stable session ID from the resulting Ark txid before returning control to the
// caller.
//
// The returned outbox contains the submit request and should be treated as the
// only place where the caller performs I/O (transport, signing, timers). The
// caller is expected to:
//  1. execute outbox requests and turn results into follow-up events; and
//  2. feed those events back into the session FSM.
func NewSession(ctx context.Context, policy arkscript.CheckpointPolicy,
	inputs []TransferInput, outputs []oortx.RecipientOutput) (*Session,
	[]OutboxEvent, error) {

	return NewSessionWithIdempotencyKey(
		ctx, policy, inputs, outputs, "", EnvConfig{},
	)
}

// NewSessionWithIdempotencyKey creates a new outgoing OOR transfer session
// tagged with a caller-provided idempotency key. envCfg injects the
// deterministic clock and the bounded transient submit-reject retry budget into
// the FSM Environment.
func NewSessionWithIdempotencyKey(ctx context.Context,
	policy arkscript.CheckpointPolicy, inputs []TransferInput,
	outputs []oortx.RecipientOutput, idempotencyKey string,
	envCfg EnvConfig) (*Session, []OutboxEvent, error) {

	logger(ctx).DebugS(ctx, "Creating new OOR session",
		slog.Int("num_inputs", len(inputs)),
		slog.Int("num_outputs", len(outputs)),
	)

	startupID := SessionID{}
	env := envCfg.newEnvironment(startupID)

	baseLogger := logger(ctx)

	fsmCfg := StateMachineCfg{
		Logger: baseLogger.WithPrefix(startupID.LogPrefix()),
		ErrorReporter: newContextErrorReporter(
			ctx, startupID.LogPrefix(),
		),
		InitialState: &Idle{},
		Env:          env,
	}

	sm := protofsm.NewStateMachine(fsmCfg)
	sm.Start(ctx)

	fut := sm.AskEvent(ctx, &StartTransferEvent{
		VTXOInputs:       inputs,
		RecipientOutputs: outputs,
		Policy:           policy,
		IdempotencyKey:   idempotencyKey,
	})
	result := fut.Await(ctx)
	if result.IsErr() {
		return nil, nil, result.Err()
	}

	currentState, err := sm.CurrentState()
	if err != nil {
		return nil, nil, err
	}

	var arkPSBT *psbt.Packet
	switch s := currentState.(type) {
	case *AwaitingArkSignatures:
		arkPSBT = s.ArkPSBT

	case *AwaitingSubmitAccepted:
		arkPSBT = s.ArkPSBT

	default:
		return nil, nil, fmt.Errorf("unexpected start state: %T",
			currentState)
	}

	sessionID, err := sessionIDFromArk(arkPSBT)
	if err != nil {
		return nil, nil, err
	}

	// Bind the FSM environment to the stable session identifier only after
	// StartTransfer has deterministically built the package.
	env.SessionID = sessionID

	logger(ctx).InfoS(ctx, "OOR session created with stable ID",
		slog.String("session_id", sessionID.String()),
	)

	return &Session{
		ID:  sessionID,
		FSM: &sm,
	}, result.UnwrapOr(nil), nil
}

// sessionIDFromArk derives the v0 session identifier from an Ark PSBT.
func sessionIDFromArk(ark *psbt.Packet) (SessionID, error) {
	if ark == nil || ark.UnsignedTx == nil {
		return SessionID{}, fmt.Errorf("ark psbt must be provided")
	}

	txid := ark.UnsignedTx.TxHash()

	return SessionID(txid), nil
}
