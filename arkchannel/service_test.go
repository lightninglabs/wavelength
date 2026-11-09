package arkchannel

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// serviceExecutor records native dispatch through the public Service API.
type serviceExecutor struct {
	counts map[string]int
}

type noOpActionExecutor struct{}

type recordingResumeExecutor struct {
	failID ID
	calls  []ID
}

// ValidatePreparedOOR accepts fixtures already covered by state validation.
func (*noOpActionExecutor) ValidatePreparedOOR(context.Context, Terms,
	VTXOBinding) error {

	return nil
}

// Execute accepts already-durable actions without completing callbacks.
func (*noOpActionExecutor) Execute(context.Context, ID, Action) error {
	return nil
}

// Execute records every replay and fails only the selected channel.
func (e *recordingResumeExecutor) Execute(_ context.Context, id ID,
	_ Action) error {

	e.calls = append(e.calls, id)
	if id == e.failID {
		return fmt.Errorf("injected channel failure")
	}

	return nil
}

// ValidatePreparedOOR accepts fixtures already covered by state validation.
func (*serviceExecutor) ValidatePreparedOOR(context.Context, Terms,
	VTXOBinding) error {

	return nil
}

// Execute records native negotiation dispatch without manufacturing funding
// completion evidence.
func (e *serviceExecutor) Execute(_ context.Context, _ ID,
	action Action) error {

	e.counts[fmt.Sprintf("%T", action)]++

	return nil
}

// TestServiceSeparatesPromotionRegistrationFromBinding proves registration
// cannot dispatch negotiation before the exact source is durably attached.
func TestServiceSeparatesPromotionRegistrationFromBinding(t *testing.T) {
	t.Parallel()
	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	executor := &serviceExecutor{counts: make(map[string]int)}
	service, err := NewService(PartyClient, coordinator, executor)
	require.NoError(t, err)
	terms := testTerms(t, KindPromotion)
	record, err := service.RegisterPromotion(t.Context(), terms)
	require.NoError(t, err)
	require.Equal(t, PhaseRequested, record.Snapshot.Phase)
	require.Empty(t, executor.counts)
	record, err = service.BindPreparedOOR(
		t.Context(), terms.ID, testBinding(terms),
	)
	require.NoError(t, err)
	require.Equal(t, PhaseNegotiating, record.Snapshot.Phase)
	require.Equal(t, 1, executor.counts["*arkchannel.NegotiateFunding"])
}

// TestServiceEventAuthorityMatrix checks both accepted event ownership and
// rejection when the same evidence is attributed to the other endpoint.
func TestServiceEventAuthorityMatrix(t *testing.T) {
	t.Parallel()
	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	terms := testTerms(t, KindPromotion)
	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	service, err := NewService(
		PartyClient, coordinator, &noOpActionExecutor{},
	)
	require.NoError(t, err)
	for _, event := range []Event{&OORPreparationStarted{}, &BindVTXO{}} {
		require.NoError(
			t,
			service.authorizeEvent(
				t.Context(), terms.ID, event, eventOriginLocal,
			),
		)
		require.Error(
			t,
			service.authorizeEvent(
				t.Context(), terms.ID, event, eventOriginPeer,
			),
		)
	}
}

// TestServiceResumeIsolatesChannelFailures proves one corrupt or unavailable
// channel cannot prevent independent durable channels from replaying.
func TestServiceResumeIsolatesChannelFailures(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	first := testTerms(t, KindPromotion)
	second := testTerms(t, KindPromotion)
	second.ID[0] = 2
	second.PendingChannelID[0] = 6
	second.ReservedSCID += 1 << 16
	for _, terms := range []Terms{first, second} {
		_, err := coordinator.Request(t.Context(), terms)
		require.NoError(t, err)
		_, actions, err := coordinator.Apply(
			t.Context(), terms.ID, &BindVTXO{
				Binding: testBinding(terms),
			},
		)
		require.NoError(t, err)
		require.Len(t, actions, 1)
	}

	executor := &recordingResumeExecutor{failID: first.ID}
	service, err := NewService(PartyClient, coordinator, executor)
	require.NoError(t, err)
	err = service.Resume(t.Context())

	var failures *ResumeFailures
	require.ErrorAs(t, err, &failures)
	require.Len(t, failures.Failures, 1)
	require.Equal(t, first.ID, failures.Failures[0].ChannelID)
	require.ErrorContains(
		t, failures.Failures[0].Err, "injected channel failure",
	)
	require.ElementsMatch(t, []ID{first.ID, second.ID}, executor.calls)
}
