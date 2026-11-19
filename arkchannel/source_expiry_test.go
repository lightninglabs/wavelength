package arkchannel

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSourceExpiryPersistsUnilateralHandoff proves expiry cannot wait on an
// offline peer's recovery or activation acknowledgement after funding exists.
func TestSourceExpiryPersistsUnilateralHandoff(t *testing.T) {
	t.Parallel()

	terms := testTerms(t, KindPromotion)
	source := testBinding(terms)
	coordinator := activeCooperativeChannel(t, terms, source)
	record, err := coordinator.Get(t.Context(), terms.ID)
	require.NoError(t, err)
	for _, phase := range []Phase{
		PhaseActive, PhaseActivating, PhaseBackingReady,
	} {
		t.Run(phase.String(), func(t *testing.T) {
			snapshot := record.Snapshot.Clone()
			snapshot.Phase = phase
			if phase == PhaseBackingReady {
				snapshot.HubRecoveryReady = false
			}
			state, err := RestoreState(snapshot)
			require.NoError(t, err)
			transition, err := state.ProcessEvent(
				t.Context(), &SourceExpiring{}, nil,
			)
			require.NoError(t, err)
			nextState, ok := transition.NextState.(State)
			require.True(t, ok)
			next := nextState.Snapshot()
			require.Equal(t, PhaseMaterializing, next.Phase)
			require.Nil(
				t, next.SourceConflict, "a deadline is not "+
					"fabricated chain-spend evidence",
			)
			require.Equal(t, snapshot.Backing, next.Backing)
			_, err = RestoreState(next)
			require.NoError(t, err)
			action, err := PendingAction(next)
			require.NoError(t, err)
			require.IsType(t, &PublishChannel{}, action)
			changed, err := applySourceExpiring(&next)
			require.NoError(t, err)
			require.False(t, changed)
			_, err = applyBackingPublished(
				&next, next.Backing.ChannelPoint.Hash,
			)
			require.NoError(t, err)
			_, err = RestoreState(next)
			require.NoError(t, err)
			_, err = applyEvent(&next, &ChannelClosed{})
			require.NoError(t, err)
			_, err = RestoreState(next)
			require.NoError(t, err)
		})
	}
}

// TestSourceExpiryDoesNotAuthorizeUnfundedExit preserves the OOR signing and
// native commitment barriers even when a local deadline fires.
func TestSourceExpiryDoesNotAuthorizeUnfundedExit(t *testing.T) {
	t.Parallel()

	terms := testTerms(t, KindPromotion)
	source := testBinding(terms)
	coordinator := activeCooperativeChannel(t, terms, source)
	record, err := coordinator.Get(t.Context(), terms.ID)
	require.NoError(t, err)
	for _, missing := range []string{"funding", "recovery"} {
		snapshot := record.Snapshot.Clone()
		if missing == "funding" {
			snapshot.OORFinalized = false
		} else {
			snapshot.ClientRecoveryReady = false
			snapshot.HubRecoveryReady = false
		}
		_, err := applySourceExpiring(&snapshot)
		require.Error(t, err)
		require.Equal(t, PhaseActive, snapshot.Phase)
	}
}

// TestSourceExpiryPreservesAuthorizedRefresh protects the old source even if
// the operator stalls after release, without treating the release as revoked
// or accepting a peer receipt as proof that native watchers can be retired.
func TestSourceExpiryPreservesAuthorizedRefresh(t *testing.T) {
	t.Parallel()

	terms, source, request, clientKey, hubKey, operatorKey :=
		testCooperativeCloseFixture(t, KindPromotion, 5_000)
	coordinator := activeCooperativeChannel(t, terms, source)
	_, _, err := coordinator.Apply(
		t.Context(), terms.ID, &RequestCooperativeClose{
			Request: request,
		},
	)
	require.NoError(t, err)
	template, err := NewCooperativeCloseTemplate(
		terms, source, request, 70_000, 30_000, 5,
	)
	require.NoError(t, err)
	settlement := completeTestCooperativeClose(
		t, template, terms, source, request, clientKey, hubKey,
		operatorKey,
	)
	for _, event := range []Event{
		&CooperativeCloseSigned{
			Close: settlement,
		},
		&CooperativeCloseReady{
			Receipt: settlement.AuthorizationReceipt(),
		},
	} {
		_, _, err = coordinator.Apply(t.Context(), terms.ID, event)
		require.NoError(t, err)
	}
	record, actions, err := coordinator.Apply(
		t.Context(), terms.ID, &SourceExpiring{},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseMaterializing, record.Snapshot.Phase)
	require.Equal(t, &settlement, record.Snapshot.CooperativeClose)

	require.IsType(t, &PublishChannel{}, requireOneAction(t, actions))
	_, err = RestoreState(record.Snapshot)
	require.NoError(t, err)
	record, _, err = coordinator.Apply(
		t.Context(), terms.ID, &CooperativeClosePublished{
			TxID: settlement.TxID,
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, PhaseMaterializing, record.Snapshot.Phase,
		"an off-chain receipt cannot prove a competing backing lost",
	)
	_, _, err = coordinator.Apply(
		t.Context(), terms.ID, &CooperativeCloseObserved{
			TxID: [32]byte{1},
		},
	)
	require.Error(t, err)
	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &CooperativeCloseObserved{
			TxID: settlement.TxID,
		},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseCoopClosePublished, record.Snapshot.Phase)
	require.IsType(
		t, &FinalizeCooperativeClose{}, requireOneAction(t, actions),
	)
	_, err = RestoreState(record.Snapshot)
	require.NoError(t, err)
}
