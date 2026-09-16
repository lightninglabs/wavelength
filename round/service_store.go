package round

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
)

// DeferredServiceOperation is the minimal cold-restart ownership record. It
// intentionally contains no signer sessions or request reconstruction keys.
type DeferredServiceOperation struct {
	Service  types.ServiceRequest
	RoundID  RoundID
	Deadline time.Time
	Inputs   []wire.OutPoint
	Forfeits []wire.OutPoint
}

// ServiceOperationStore retains input ownership without resuming a signing
// ceremony. Save must reject overlapping inputs owned by another operation.
type ServiceOperationStore interface {
	SaveServiceOperation(context.Context, DeferredServiceOperation) error

	BindServiceOperation(context.Context, [32]byte, RoundID,
		time.Time) error

	ListDeferredServiceOperations(context.Context) (
		[]DeferredServiceOperation, error)

	CompleteServiceOperation(context.Context, [32]byte) error
}

// deferredServiceOperation extracts only ownership metadata, never signing
// keys.
func deferredServiceOperation(intents Intents) DeferredServiceOperation {
	op := DeferredServiceOperation{Service: *intents.Service}
	for _, input := range intents.Boarding {
		op.Inputs = append(op.Inputs, input.Outpoint)
	}
	for _, input := range intents.Forfeits {
		if input.VTXOOutpoint != nil {
			op.Inputs = append(op.Inputs, *input.VTXOOutpoint)
			op.Forfeits = append(op.Forfeits, *input.VTXOOutpoint)
		}
	}

	return op
}

// restoreDeferredServiceOperations restores visibility and reservation
// ownership while leaving cold signing sessions dormant. It does not replay a
// request.
func (a *RoundClientActor) restoreDeferredServiceOperations(
	ctx context.Context) error {

	if a.cfg.ServiceStore == nil {
		return nil
	}
	operations, err := a.cfg.ServiceStore.ListDeferredServiceOperations(ctx)
	if err != nil {
		return err
	}
	for _, operation := range operations {
		var key RoundKey = operation.RoundID
		if operation.RoundID == (RoundID{}) {
			key, err = NewTempRoundKey()
			if err != nil {
				return err
			}
		}
		keyString := RoundKeyStr(key.KeyString())
		if _, present := a.rounds[keyString]; present {
			continue
		}
		env := *a.env
		env.RoundKey = keyString
		state := &ServiceReconcileState{
			Intents: Intents{
				Service: &operation.Service,
			},
			RoundID: operation.RoundID, Cold: true,
			Probes: 0,
		}
		for _, input := range operation.Forfeits {
			outpoint := input
			state.Intents.Forfeits = append(
				state.Intents.Forfeits, types.ForfeitRequest{
					VTXOOutpoint: &outpoint,
				},
			)
		}
		fsm := protofsm.NewInlineStateMachine(ClientStateMachineCfg{
			Logger: a.log, ErrorReporter: newLoggerErrorReporter(
				a.log,
			),
			InitialState: state, Env: &env,
		})
		a.startRoundFSM(ctx, &fsm)
		a.rounds[keyString] = &RoundFSM{
			env: &env,
			FSM: &fsm, Key: key, RoundID: operation.RoundID,
			Operation: &roundpb.OperationStatus{
				OperationId: append(
					[]byte(nil),
					operation.Service.OperationID[:]...,
				),
			},
		}
		if err := a.askEventAndProcessOutbox(
			ctx, a.rounds[keyString], &RegistrationTimedOut{},
		); err != nil {
			return err
		}
	}

	return nil
}
