package arkchannel

import (
	"context"
	"testing"

	"github.com/lightninglabs/wavelength/round"
	"github.com/stretchr/testify/require"
)

// completingFundingExecutor simulates native lnd callbacks during negotiation.
type completingFundingExecutor struct {
	t           *testing.T
	coordinator *Coordinator
	actions     []Action
}

// Execute records the signed backing and both endpoint acknowledgements.
func (e *completingFundingExecutor) Execute(ctx context.Context, id ID,
	action Action) error {

	e.actions = append(e.actions, action)
	negotiate, ok := action.(*NegotiateFunding)
	if !ok {
		return nil
	}
	backing := testBacking(e.t, negotiate.Terms, negotiate.Source)
	for _, event := range []Event{
		&BackingSigned{
			Backing: backing,
		},
		&FundingFinalized{
			Party: PartyClient,
		},
		&FundingFinalized{
			Party: PartyHub,
		},
	} {
		if _, _, err := e.coordinator.Apply(
			ctx, id, event,
		); err != nil {
			return err
		}
	}

	return nil
}

// TestRoundCompletionActivatesMatchingIntent verifies confirmation advances
// only the channel bound to the exact round transaction.
func TestRoundCompletionActivatesMatchingIntent(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	terms := testTerms(t, KindReceiveIntent)
	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	executor := &completingFundingExecutor{
		t:           t,
		coordinator: coordinator,
	}
	gate, err := NewRoundGate(coordinator, executor)
	require.NoError(t, err)
	completion, err := NewRoundCompletion(coordinator, executor)
	require.NoError(t, err)
	request := testRoundReadinessRequest(t, terms)
	token, err := gate.AwaitSigningAuthorization(t.Context(), request)
	require.NoError(t, err)
	require.NoError(
		t,
		gate.CommitSigningAuthorization(
			t.Context(), request, token,
		),
	)

	require.NoError(
		t,
		completion.RoundConfirmed(
			t.Context(), request.RoundID, request.CommitmentTxID,
			round.ConfInfo{},
		),
	)
	record, err := coordinator.Get(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, PhaseActivating, record.Snapshot.Phase)
	require.IsType(
		t, &ActivateChannel{}, executor.actions[len(
			executor.actions)-1],
	)
}

// noOpActionExecutor intentionally returns before lnd funding is durable.
type noOpActionExecutor struct{}

// Execute implements ActionExecutor without recording completion.
func (*noOpActionExecutor) Execute(context.Context, ID, Action) error {
	return nil
}

// TestRoundGateBindsBeforeNonceCommit verifies the two round boundaries map to
// BackingReady and AwaitingConfirmation respectively.
func TestRoundGateBindsBeforeNonceCommit(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	terms := testTerms(t, KindReceiveIntent)
	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	executor := &completingFundingExecutor{
		t:           t,
		coordinator: coordinator,
	}
	gate, err := NewRoundGate(coordinator, executor)
	require.NoError(t, err)

	request := testRoundReadinessRequest(t, terms)
	token, err := gate.AwaitSigningAuthorization(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, terms.ID[:], token)

	record, err := coordinator.Get(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, PhaseBackingReady, record.Snapshot.Phase)
	require.True(t, record.Snapshot.ReadyForRoundSigning())

	require.NoError(
		t,
		gate.CommitSigningAuthorization(
			t.Context(), request, token,
		),
	)
	record, err = coordinator.Get(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, PhaseAwaitingConfirmation, record.Snapshot.Phase)

	require.NoError(
		t,
		gate.CommitSigningAuthorization(
			t.Context(), request, token,
		),
	)
}

// TestRoundGateIgnoresOrdinaryVTXO verifies configuring the generic gate does
// not alter normal round outputs.
func TestRoundGateIgnoresOrdinaryVTXO(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	gate, err := NewRoundGate(coordinator, &noOpActionExecutor{})
	require.NoError(t, err)
	roundID, err := round.NewRoundID()
	require.NoError(t, err)

	token, err := gate.AwaitSigningAuthorization(
		t.Context(), round.RoundReadinessRequest{
			RoundID: roundID,
			Outputs: []round.RoundReadinessOutput{
				{PkScript: []byte{0x51}},
			},
		},
	)
	require.NoError(t, err)
	require.Empty(t, token)
	require.NoError(
		t,
		gate.CommitSigningAuthorization(
			t.Context(), round.RoundReadinessRequest{}, token,
		),
	)
}

// TestRoundGateRejectsEarlyFundingReturn verifies nonce authorization cannot
// proceed when an adapter starts lnd but returns before finalization callbacks.
func TestRoundGateRejectsEarlyFundingReturn(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	terms := testTerms(t, KindReceiveIntent)
	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	gate, err := NewRoundGate(coordinator, &noOpActionExecutor{})
	require.NoError(t, err)

	_, err = gate.AwaitSigningAuthorization(
		t.Context(), testRoundReadinessRequest(t, terms),
	)
	require.ErrorContains(t, err, "before backing was ready")
}

// TestRoundGateReplaysInterruptedFunding verifies a repeated exact round
// output resumes the durable negotiation action after an earlier executor
// failure instead of waiting forever in the negotiating phase.
func TestRoundGateReplaysInterruptedFunding(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	terms := testTerms(t, KindReceiveIntent)
	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	request := testRoundReadinessRequest(t, terms)
	output := request.Outputs[0]
	_, _, err = coordinator.Apply(
		t.Context(), terms.ID, &BindVTXO{
			Binding: VTXOBinding{
				OutPoint:       output.VTXOOutpoint,
				Amount:         output.Amount,
				RoundID:        request.RoundID.String(),
				CommitmentTxID: request.CommitmentTxID,
				PolicyTemplate: output.PolicyTemplate,
				PkScript:       output.PkScript,
			},
		},
	)
	require.NoError(t, err)

	executor := &completingFundingExecutor{
		t:           t,
		coordinator: coordinator,
	}
	gate, err := NewRoundGate(coordinator, executor)
	require.NoError(t, err)

	token, err := gate.AwaitSigningAuthorization(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, terms.ID[:], token)
	require.Len(t, executor.actions, 1)
	require.IsType(t, &NegotiateFunding{}, executor.actions[0])
}

// testRoundReadinessRequest creates one exact receive-intent output.
func testRoundReadinessRequest(t *testing.T,
	terms Terms) round.RoundReadinessRequest {

	t.Helper()
	roundID, err := round.NewRoundID()
	require.NoError(t, err)
	binding := testBinding(terms)

	return round.RoundReadinessRequest{
		RoundID:        roundID,
		CommitmentTxID: binding.CommitmentTxID,
		Outputs: []round.RoundReadinessOutput{
			{
				VTXOOutpoint:   binding.OutPoint,
				Amount:         binding.Amount,
				PolicyTemplate: binding.PolicyTemplate,
				PkScript:       binding.PkScript,
			},
		},
	}
}

var (
	_ ActionExecutor = (*completingFundingExecutor)(nil)
	_ ActionExecutor = (*noOpActionExecutor)(nil)
)
