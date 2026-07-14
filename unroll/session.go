package unroll

import (
	"context"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/unrollplan"
)

// Session groups a running unroll FSM with the immutable proof it executes.
type Session struct {
	// Proof is the immutable recovery proof this session executes.
	Proof *recovery.Proof

	// Planner is the pure planner bound to the immutable proof.
	Planner *unrollplan.Planner

	// FSM is the running protofsm instance.
	FSM *StateMachine
}

// SessionOption customizes one unroll FSM session.
type SessionOption func(*Environment)

// NewSession creates a new unroll FSM session with the provided initial state.
// fraudCheckpointSafetyMargin overrides the recipient backstop margin (in
// blocks) baked into the FSM Environment; zero falls back to
// defaultFraudCheckpointSafetyMargin.
func NewSession(ctx context.Context, proof *recovery.Proof,
	planner *unrollplan.Planner, initial State, logger btclog.Logger,
	fraudCheckpointSafetyMargin int32,
	opts ...SessionOption) (*Session, error) {

	if proof == nil {
		return nil, fmt.Errorf("proof must be provided")
	}

	if planner == nil {
		return nil, fmt.Errorf("planner must be provided")
	}

	if initial == nil {
		return nil, fmt.Errorf("initial state must be provided")
	}

	if logger == nil {
		logger = btclog.Disabled
	}

	env := &Environment{
		Proof:                       proof,
		Planner:                     planner,
		FraudCheckpointSafetyMargin: fraudCheckpointSafetyMargin,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(env)
		}
	}

	fsmCfg := protofsm.StateMachineCfg[Event, OutboxEvent, *Environment]{
		Logger: logger.WithPrefix(proof.TargetOutpoint().String()),
		ErrorReporter: newContextErrorReporter(
			ctx, logger, proof.TargetOutpoint().String(),
		),
		InitialState: initial,
		Env:          env,
	}

	sm := protofsm.NewStateMachine(fsmCfg)
	sm.Start(ctx)

	return &Session{
		Proof:   proof,
		Planner: planner,
		FSM:     &sm,
	}, nil
}
