package oor

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
)

// ReceiveSession groups a running incoming-transfer FSM with its stable
// identifier.
type ReceiveSession struct {
	ID SessionID

	FSM *StateMachine
}

// NewReceiveSession creates a new incoming-transfer FSM session for the given
// Ark PSBT.
//
// The caller should drive the returned FSM by sending an IncomingTransferEvent
// (or by using DriveIncomingTransfer).
//
// Ownership checks are intentionally deferred to the incoming materialization
// boundary (LocalPersistenceOutboxHandler + resolver callbacks), where wallet
// key ownership is available.
func NewReceiveSession(ctx context.Context, ark *psbt.Packet,
	sessionID SessionID) (*ReceiveSession, error) {

	if ark == nil || ark.UnsignedTx == nil {
		return nil, fmt.Errorf("ark psbt must be provided")
	}

	if sessionID == (SessionID{}) {
		return nil, fmt.Errorf("session id must be provided")
	}

	logger(ctx).InfoS(ctx, "Creating receive session",
		slog.String("session_id", sessionID.String()),
	)

	return newReceiveSessionWithState(
		ctx, sessionID, &ReceiveIdle{}, EnvConfig{},
	)
}

// newReceiveSessionWithState creates an incoming-transfer FSM session with
// the provided initial receive state. envCfg injects the deterministic clock
// and retry budget into the FSM Environment; the incoming FSM does not read the
// retry budget today, but the clock is threaded for consistency with the
// outgoing path and to keep transitions deterministic.
func newReceiveSessionWithState(ctx context.Context, sessionID SessionID,
	state SessionState, envCfg EnvConfig) (*ReceiveSession, error) {

	if sessionID == (SessionID{}) {
		return nil, fmt.Errorf("session id must be provided")
	}

	if state == nil {
		return nil, fmt.Errorf("state must be provided")
	}

	env := envCfg.newEnvironment(sessionID)

	baseLogger := logger(ctx)

	fsmCfg := StateMachineCfg{
		Logger: baseLogger.WithPrefix(sessionID.LogPrefix()),
		ErrorReporter: newContextErrorReporter(
			ctx, sessionID.LogPrefix(),
		),
		InitialState: state,
		Env:          env,
	}

	sm := protofsm.NewStateMachine(fsmCfg)
	sm.Start(ctx)

	return &ReceiveSession{
		ID:  sessionID,
		FSM: &sm,
	}, nil
}

// DriveIncomingTransfer is a small helper that constructs a receive session
// and feeds it the incoming-transfer event.
//
// This is intended for tests and early harnesses. In an app, the incoming
// event would typically be delivered to an already-running durable actor.
func DriveIncomingTransfer(ctx context.Context, sessionID SessionID,
	ark *psbt.Packet) (*ReceiveSession, []OutboxEvent, error) {

	return DriveIncomingTransferWithCheckpoints(
		ctx, sessionID, ark, nil, nil,
	)
}

// DriveIncomingTransferWithCheckpoints is DriveIncomingTransfer with optional
// finalized checkpoints attached to the incoming event.
func DriveIncomingTransferWithCheckpoints(ctx context.Context,
	sessionID SessionID, ark *psbt.Packet, finalCheckpoints []*psbt.Packet,
	ancestorPackages []PackageArtifact) (*ReceiveSession, []OutboxEvent,
	error) {

	sess, err := NewReceiveSession(ctx, ark, sessionID)
	if err != nil {
		return nil, nil, err
	}

	fut := sess.FSM.AskEvent(ctx, &IncomingTransferEvent{
		SessionID:            sessionID,
		ArkPSBT:              ark,
		FinalCheckpointPSBTs: finalCheckpoints,
		AncestorPackages:     ancestorPackages,
	})

	result := fut.Await(ctx)
	if result.IsErr() {
		return nil, nil, result.Err()
	}

	outbox := result.UnwrapOr(nil)

	return sess, outbox, nil
}
