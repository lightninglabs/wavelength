package arkchannel

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// serviceExecutor completes native callbacks through the public Service API.
type serviceExecutor struct {
	t          *testing.T
	service    *Service
	localParty Party
	counts     map[string]int
}

type staticFundingFinalizationSource struct {
	finalized bool
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

// applyPartyEvent routes one simulated endpoint callback through the matching
// local or authenticated-peer service boundary.
func (e *serviceExecutor) applyPartyEvent(ctx context.Context, id ID,
	party Party, event Event) (Record, error) {

	if party == e.localParty {
		return e.service.ApplyLocalEvent(ctx, id, event)
	}

	return e.service.ApplyPeerEvent(ctx, id, event)
}

// FundingFinalized returns one fixed lnd database observation.
func (s *staticFundingFinalizationSource) FundingFinalized(context.Context,
	Terms, Backing) (bool, error) {

	return s.finalized, nil
}

// Execute simulates native subsystem completion through authenticated
// callbacks.
func (e *serviceExecutor) Execute(ctx context.Context, id ID,
	action Action) error {

	e.counts[fmt.Sprintf("%T", action)]++
	switch action := action.(type) {
	case *NegotiateFunding:
		backing := testBacking(e.t, action.Terms, action.Source)
		for _, fact := range []struct {
			party Party
			event Event
		}{
			{
				action.Terms.Funder,
				&BackingSigned{
					Backing: backing,
				},
			},
			{
				PartyClient,
				&FundingFinalized{
					Party: PartyClient,
				},
			},
			{
				PartyHub,
				&FundingFinalized{
					Party: PartyHub,
				},
			},
		} {
			if _, err := e.applyPartyEvent(
				ctx, id, fact.party, fact.event,
			); err != nil {
				return err
			}
		}

	case *CommitOOR:
		_, err := e.applyPartyEvent(
			ctx, id, action.Terms.Funder, &OORFinalized{
				SessionID: action.Source.OORSessionID,
			},
		)

		return err

	case *PrepareRecovery:
		for _, party := range []Party{PartyClient, PartyHub} {
			if _, err := e.applyPartyEvent(
				ctx, id, party, &RecoveryPackageInstalled{
					Party: party,
				},
			); err != nil {
				return err
			}
		}

		return nil

	case *ActivateChannel:
		_, err := e.service.ApplyLocalEvent(
			ctx, id, &ChannelActive{
				ChannelPointHash: action.
					Backing.
					ChannelPoint.
					Hash,
				ChannelPointIndex: action.
					Backing.
					ChannelPoint.
					Index,
			},
		)

		return err

	default:
		return fmt.Errorf("unexpected service action %T", action)
	}

	return nil
}

// TestServiceSeparatesPromotionRegistrationFromBinding verifies two peers can
// persist identical terms before either one starts lnd negotiation.
func TestServiceSeparatesPromotionRegistrationFromBinding(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	executor := &serviceExecutor{
		t:      t,
		counts: make(map[string]int),
	}
	service, err := NewService(PartyClient, coordinator, executor)
	require.NoError(t, err)
	executor.service = service
	executor.localParty = PartyClient
	terms := testTerms(t, KindPromotion)

	record, err := service.RegisterPromotion(t.Context(), terms)
	require.NoError(t, err)
	require.Equal(t, PhaseRequested, record.Snapshot.Phase)
	require.Empty(t, executor.counts)

	record, err = service.BindPreparedOOR(
		t.Context(), terms.ID, testBinding(terms),
	)
	require.NoError(t, err)
	require.Equal(t, PhaseActive, record.Snapshot.Phase)
	require.Equal(t, 1, executor.counts["*arkchannel.NegotiateFunding"])
}

// TestServiceReconcilesFundingByChannelPoint proves a missed lnd callback is
// recoverable from the pending channel record keyed by signed backing.
func TestServiceReconcilesFundingByChannelPoint(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	service, err := NewService(
		PartyClient, coordinator, &noOpActionExecutor{},
	)
	require.NoError(t, err)
	terms := testTerms(t, KindPromotion)
	_, err = service.RegisterPromotion(t.Context(), terms)
	require.NoError(t, err)
	binding := testBinding(terms)
	_, err = service.BindPreparedOOR(t.Context(), terms.ID, binding)
	require.NoError(t, err)
	backing := testBacking(t, terms, binding)
	_, err = service.ApplyLocalEvent(t.Context(), terms.ID, &BackingSigned{
		Backing: backing,
	})
	require.NoError(t, err)

	source := &staticFundingFinalizationSource{finalized: true}
	require.NoError(
		t,
		service.ReconcileFunding(t.Context(), source),
	)
	record, err := service.ApplyPeerEvent(
		t.Context(), terms.ID, &FundingFinalized{
			Party: PartyHub,
		},
	)
	require.NoError(t, err)
	require.True(t, record.Snapshot.ClientFinalized)
	require.True(t, record.Snapshot.HubFinalized)
	require.Equal(t, PhaseBackingReady, record.Snapshot.Phase)
	require.True(t, record.Snapshot.ReadyToCommitOOR())

	_, err = service.ObserveFundingFinalized(
		t.Context(), wire.OutPoint{
			Hash: [32]byte{99},
		},
	)
	require.ErrorIs(t, err, ErrNotFound)
}

// TestServiceEventAuthorityMatrix checks the origin of each installed fact.
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
	for _, test := range []struct {
		event  Event
		origin eventOrigin
	}{
		{
			&OORPreparationStarted{},
			eventOriginLocal,
		},
		{
			&BindVTXO{},
			eventOriginLocal,
		},
		{
			&BackingSigned{},
			eventOriginLocal,
		},
		{
			&OORFinalized{},
			eventOriginLocal,
		},
		{
			&OORAborted{},
			eventOriginLocal,
		},
		{
			&FundingCanceled{},
			eventOriginLocal,
		},
		{
			&ExpirePrePONR{},
			eventOriginLocal,
		},
		{
			&Fail{},
			eventOriginLocal,
		},
		{
			&FundingFinalized{
				Party: PartyClient,
			},
			eventOriginLocal,
		},
		{
			&FundingFinalized{
				Party: PartyHub,
			},
			eventOriginPeer,
		},
		{
			&RecoveryPackageInstalled{
				Party: PartyClient,
			},
			eventOriginLocal,
		},
		{
			&RecoveryPackageInstalled{
				Party: PartyHub,
			},
			eventOriginPeer,
		},
		{
			&ChannelActive{},
			eventOriginLocal,
		},
	} {
		require.NoError(
			t,
			service.authorizeEvent(
				t.Context(), terms.ID, test.event, test.origin,
			),
		)
		opposite := eventOriginLocal
		if test.origin == eventOriginLocal {
			opposite = eventOriginPeer
		}
		require.Error(
			t,
			service.authorizeEvent(
				t.Context(), terms.ID, test.event, opposite,
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
