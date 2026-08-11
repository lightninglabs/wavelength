package arkchannel

import (
	"bytes"
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/round"
)

const roundTokenSize = len(ID{})

// ActionExecutor performs an idempotent side effect after its state is
// durable. Funding execution must return only after callbacks have recorded
// the signed backing and both lnd finalization acknowledgements.
type ActionExecutor interface {
	Execute(context.Context, ID, Action) error
}

// RoundGate binds receive intents to exact round outputs before nonce release.
type RoundGate struct {
	coordinator *Coordinator
	executor    ActionExecutor
}

// NewRoundGate constructs the Ark implementation of round readiness.
func NewRoundGate(coordinator *Coordinator,
	executor ActionExecutor) (*RoundGate, error) {

	if coordinator == nil {
		return nil, fmt.Errorf("channel coordinator is required")
	}
	if executor == nil {
		return nil, fmt.Errorf("channel action executor is required")
	}

	return &RoundGate{
		coordinator: coordinator,
		executor:    executor,
	}, nil
}

// AwaitSigningAuthorization prepares the one registered receive intent in a
// validated round. Ordinary VTXOs return an empty token immediately.
func (g *RoundGate) AwaitSigningAuthorization(ctx context.Context,
	request round.RoundReadinessRequest) ([]byte, error) {

	records, err := g.coordinator.ListNonTerminal(ctx)
	if err != nil {
		return nil, err
	}

	match, output, err := matchReceiveOutput(records, request.Outputs)
	if err != nil {
		return nil, err
	}
	if match == nil {
		return nil, nil
	}

	binding := VTXOBinding{
		OutPoint:       output.VTXOOutpoint,
		Amount:         output.Amount,
		RoundID:        request.RoundID.String(),
		CommitmentTxID: request.CommitmentTxID,
		PolicyTemplate: output.PolicyTemplate,
		PkScript:       output.PkScript,
	}
	record, actions, err := g.coordinator.Apply(
		ctx, match.Snapshot.Terms.ID, &BindVTXO{
			Binding: binding,
		},
	)
	if err != nil {
		return nil, err
	}
	if len(actions) == 0 && record.Snapshot.Phase == PhaseNegotiating {
		var resumeErr error
		record, actions, resumeErr = g.coordinator.Resume(
			ctx, match.Snapshot.Terms.ID,
		)
		if resumeErr != nil {
			return nil, resumeErr
		}
	}
	for _, action := range actions {
		if err := g.executor.Execute(
			ctx, match.Snapshot.Terms.ID, action,
		); err != nil {
			return nil, err
		}
	}

	if len(actions) > 0 {
		record, err = g.coordinator.Get(ctx, match.Snapshot.Terms.ID)
		if err != nil {
			return nil, err
		}
	}
	if !record.Snapshot.ReadyForRoundSigning() {
		return nil, fmt.Errorf("channel %x funding returned before "+
			"backing was ready", match.Snapshot.Terms.ID[:4])
	}

	return bytes.Clone(match.Snapshot.Terms.ID[:]), nil
}

// CommitSigningAuthorization records the irreversible nonce boundary for the
// exact round/output prepared by AwaitSigningAuthorization.
func (g *RoundGate) CommitSigningAuthorization(ctx context.Context,
	request round.RoundReadinessRequest, token []byte) error {

	if len(token) == 0 {
		return nil
	}
	if len(token) != roundTokenSize {
		return fmt.Errorf("Ark channel round token has %d bytes, "+
			"expected %d", len(token), roundTokenSize)
	}

	var id ID
	copy(id[:], token)
	_, actions, err := g.coordinator.Apply(ctx, id, &RoundCommitted{
		RoundID:        request.RoundID.String(),
		CommitmentTxID: request.CommitmentTxID,
	})
	if err != nil {
		return err
	}
	if len(actions) != 0 {
		return fmt.Errorf("round commitment emitted unexpected "+
			"action %T", actions[0])
	}

	return nil
}

// matchReceiveOutput finds at most one locally registered channel output.
func matchReceiveOutput(records []Record,
	outputs []round.RoundReadinessOutput) (*Record,
	*round.RoundReadinessOutput, error) {

	var matchedRecord *Record
	var matchedOutput *round.RoundReadinessOutput
	for i := range outputs {
		output := &outputs[i]
		for j := range records {
			record := &records[j]
			kind := record.Snapshot.Terms.Kind
			isReceive := kind == KindReceiveIntent
			canBindRound := record.Snapshot.Phase <=
				PhaseAwaitingConfirmation
			if !isReceive || !canBindRound {
				continue
			}
			policy, pkScript, err := record.Snapshot.Terms.VTXO.
				Artifacts()
			if err != nil {
				return nil, nil, err
			}
			if !bytes.Equal(pkScript, output.PkScript) {
				continue
			}
			if !bytes.Equal(policy, output.PolicyTemplate) {
				return nil, nil, fmt.Errorf("channel VTXO " +
					"policy mismatch")
			}
			if matchedRecord != nil {
				return nil, nil, fmt.Errorf("round contains " +
					"multiple local receive channel " +
					"intents")
			}

			matchedRecord = record
			matchedOutput = output
		}
	}

	return matchedRecord, matchedOutput, nil
}

var _ round.RoundReadinessGate = (*RoundGate)(nil)
