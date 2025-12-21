package oor

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
)

// Session groups a running OOR transfer FSM with its stable identifier.
type Session struct {
	// ID is the stable v0 session identifier (Ark txid).
	ID SessionID

	// FSM is the running state machine for this session.
	FSM *StateMachine
}

// NewSession builds a submit package and creates a new OOR transfer session
// FSM that is ready to send the submit package to the server.
//
// This helper exists to ensure the FSM environment name is stable and derived
// from the Ark txid, which is only known after building the Ark PSBT.
//
// The returned outbox contains the submit request and should be treated as the
// only place where the caller performs I/O (transport, signing, timers). The
// caller is expected to:
//  1. execute outbox requests and turn results into follow-up events; and
//  2. feed those events back into the session FSM.
func NewSession(ctx context.Context, policy scripts.CheckpointPolicy,
	inputs []oortx.CheckpointInput,
	outputs []oortx.RecipientOutput) (*Session, []OutboxEvent, error) {

	inputOutpoints := make([]wire.OutPoint, 0, len(inputs))
	for i := range inputs {
		inputOutpoints = append(inputOutpoints, inputs[i].Outpoint)
	}

	ark, checkpoints, err := buildSubmitPackage(policy, inputs, outputs)
	if err != nil {
		return nil, nil, err
	}

	sessionID, err := sessionIDFromArk(ark)
	if err != nil {
		return nil, nil, err
	}

	env := &Environment{SessionID: sessionID}

	fsmCfg := StateMachineCfg{
		Logger:        log.WithPrefix(sessionID.LogPrefix()),
		ErrorReporter: newContextErrorReporter(ctx, sessionID.LogPrefix()),
		InitialState: &AwaitingSubmitAccepted{
			InputOutpoints:  inputOutpoints,
			ArkPSBT:         ark,
			CheckpointPSBTs: checkpoints,
		},
		Env: env,
	}

	sm := protofsm.NewStateMachine(fsmCfg)
	sm.Start(ctx)

	outbox := []OutboxEvent{
		&SendSubmitPackageRequest{
			ArkPSBT:         ark,
			CheckpointPSBTs: checkpoints,
		},
	}

	return &Session{
		ID:  sessionID,
		FSM: &sm,
	}, outbox, nil
}

// sessionIDFromArk derives the v0 session identifier from an Ark PSBT.
func sessionIDFromArk(ark *psbt.Packet) (SessionID, error) {
	if ark == nil || ark.UnsignedTx == nil {
		return SessionID{}, fmt.Errorf("ark psbt must be provided")
	}

	txid := ark.UnsignedTx.TxHash()

	return SessionID(txid), nil
}
