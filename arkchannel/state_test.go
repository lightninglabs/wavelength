package arkchannel

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestOORChannelLifecycle proves the prepared transfer commits only after both
// lnd endpoints and the immutable backing transaction are durable.
func TestOORChannelLifecycle(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	coordinator, err := NewCoordinator(store)
	require.NoError(t, err)

	terms := testTerms(t, KindReceiveIntent)
	record, err := coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	require.Equal(t, PhaseRequested, record.Snapshot.Phase)

	binding := testBinding(terms)
	record, actions, err := coordinator.Apply(
		t.Context(), terms.ID, &BindVTXO{
			Binding: binding,
		},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseRequested, record.Snapshot.Phase)
	require.Empty(t, actions)
	require.NotNil(t, record.Snapshot.Source)

	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &FundingPeerReady{},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseNegotiating, record.Snapshot.Phase)
	require.IsType(t, &NegotiateFunding{}, requireOneAction(t, actions))

	backing := testBacking(t, terms, binding)
	for _, event := range []Event{
		&BackingSigned{
			Backing: backing,
		},
		&FundingFinalized{
			Party: PartyClient,
		},
	} {
		record, _, err = coordinator.Apply(t.Context(), terms.ID, event)
		require.NoError(t, err)
		require.False(t, record.Snapshot.ReadyToCommitOOR())
	}

	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &FundingFinalized{
			Party: PartyHub,
		},
	)
	require.NoError(t, err)
	require.IsType(t, &CommitOOR{}, requireOneAction(t, actions))
	require.Equal(t, PhaseBackingReady, record.Snapshot.Phase)
	require.True(t, record.Snapshot.ReadyToCommitOOR())

	_, _, err = coordinator.Apply(
		t.Context(), terms.ID, &Fail{
			Reason: "peer disconnected",
		},
	)
	require.ErrorContains(t, err, "after safety boundary")

	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &OORFinalized{
			SessionID: binding.OORSessionID,
		},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseBackingReady, record.Snapshot.Phase)
	require.IsType(t, &PrepareRecovery{}, requireOneAction(t, actions))
	require.False(t, record.Snapshot.RecoveryReady)

	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &RecoveryPackageInstalled{},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseActivating, record.Snapshot.Phase)
	require.IsType(t, &ActivateChannel{}, requireOneAction(t, actions))
	require.True(t, record.Snapshot.RecoveryReady)

	_, resumed, err := coordinator.Resume(t.Context(), terms.ID)
	require.NoError(t, err)
	require.IsType(t, &ActivateChannel{}, requireOneAction(t, resumed))

	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &ChannelActive{
			ChannelPointHash:  backing.ChannelPoint.Hash,
			ChannelPointIndex: backing.ChannelPoint.Index,
		},
	)
	require.NoError(t, err)
	require.Empty(t, actions)
	require.Equal(t, PhaseActive, record.Snapshot.Phase)

	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &Materialize{},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseMaterializing, record.Snapshot.Phase)
	require.IsType(t, &PublishChannel{}, requireOneAction(t, actions))

	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &BackingPublished{
			TxID: backing.ChannelPoint.Hash,
		},
	)
	require.NoError(t, err)
	require.Empty(t, actions)
	require.Equal(t, PhaseOnChain, record.Snapshot.Phase)

	record, _, err = coordinator.Apply(
		t.Context(), terms.ID, &ChannelClosed{},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseClosed, record.Snapshot.Phase)
}

// TestSourceConflictDrivesForceClose proves confirmed ancestry evidence is a
// durable trigger for backing publication followed by native lnd force close.
func TestSourceConflictDrivesForceClose(t *testing.T) {
	t.Parallel()

	terms := testTerms(t, KindPromotion)
	source := testBinding(terms)
	backing := testBacking(t, terms, source)
	coordinator := activeCooperativeChannel(t, terms, source)
	conflict := &SourceSpent{
		OutPoint: source.OutPoint,
		SpendingTxID: chainhash.Hash{
			99,
		},
	}

	record, actions, err := coordinator.Apply(
		t.Context(), terms.ID, conflict,
	)
	require.NoError(t, err)
	require.Equal(t, PhaseMaterializing, record.Snapshot.Phase)
	require.Equal(
		t, conflict.OutPoint, record.Snapshot.SourceConflict.OutPoint,
	)
	require.Equal(
		t, conflict.SpendingTxID,
		record.Snapshot.SourceConflict.SpendingTxID,
	)
	require.IsType(t, &PublishChannel{}, requireOneAction(t, actions))

	// Replaying the same confirmed fact must also replay an action whose
	// prior execution could have failed after the transition was stored.
	_, actions, err = coordinator.Apply(t.Context(), terms.ID, conflict)
	require.NoError(t, err)
	require.IsType(t, &PublishChannel{}, requireOneAction(t, actions))

	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &BackingPublished{
			TxID: backing.ChannelPoint.Hash,
		},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseOnChain, record.Snapshot.Phase)
	require.True(t, record.Snapshot.BackingPublished)
	require.IsType(t, &ForceCloseChannel{}, requireOneAction(t, actions))

	_, actions, err = coordinator.Apply(t.Context(), terms.ID, conflict)
	require.NoError(t, err)
	require.IsType(t, &ForceCloseChannel{}, requireOneAction(t, actions))
}

// TestSourceConflictDuringActivationFinishesActivationFirst proves a parent
// conflict cannot strand a channel between lnd funding and materialization.
func TestSourceConflictDuringActivationFinishesActivationFirst(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	coordinator, err := NewCoordinator(store)
	require.NoError(t, err)
	terms := testTerms(t, KindPromotion)
	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	source := testBinding(terms)
	backing := testBacking(t, terms, source)
	for _, event := range []Event{
		&BindVTXO{
			Binding: source,
		},
		&BackingSigned{
			Backing: backing,
		},
		&FundingFinalized{
			Party: PartyClient,
		},
		&FundingFinalized{
			Party: PartyHub,
		},
		&OORFinalized{
			SessionID: source.OORSessionID,
		},
		&RecoveryPackageInstalled{},
	} {
		_, _, err = coordinator.Apply(t.Context(), terms.ID, event)
		require.NoError(t, err)
	}

	conflict := &SourceSpent{
		OutPoint: source.OutPoint,
		SpendingTxID: chainhash.Hash{
			100,
		},
	}
	record, actions, err := coordinator.Apply(
		t.Context(), terms.ID, conflict,
	)
	require.NoError(t, err)
	require.Equal(t, PhaseActivating, record.Snapshot.Phase)
	require.IsType(t, &ActivateChannel{}, requireOneAction(t, actions))

	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &ChannelActive{
			ChannelPointHash:  backing.ChannelPoint.Hash,
			ChannelPointIndex: backing.ChannelPoint.Index,
		},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseMaterializing, record.Snapshot.Phase)
	require.IsType(t, &PublishChannel{}, requireOneAction(t, actions))
}

// TestSourceConflictAbandonsAuthorizedCooperativeClose proves a hub-authorized
// but not yet finalized OOR close cannot override independent source-spend
// evidence. Recovery materializes the channel point instead.
func TestSourceConflictAbandonsAuthorizedCooperativeClose(t *testing.T) {
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
	for _, party := range []Party{PartyClient, PartyHub} {
		_, _, err = coordinator.Apply(
			t.Context(), terms.ID, &CooperativeCloseSigned{
				Close: settlement,
				Party: party,
			},
		)
		require.NoError(t, err)
	}

	record, actions, err := coordinator.Apply(
		t.Context(), terms.ID, &SourceSpent{
			OutPoint: source.OutPoint,
			SpendingTxID: chainhash.Hash{
				101,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseMaterializing, record.Snapshot.Phase)
	require.False(t, record.Snapshot.BackingPublished)
	require.Nil(t, record.Snapshot.CooperativeClose)
	require.Nil(t, record.Snapshot.CooperativeCloseRequest)
	require.IsType(t, &PublishChannel{}, requireOneAction(t, actions))
}

// TestSourceConflictAbandonsPartialCooperativeClose proves an unavailable peer
// cannot strand recovery after close negotiation starts but before both
// signatures become durable.
func TestSourceConflictAbandonsPartialCooperativeClose(t *testing.T) {
	t.Parallel()

	terms, source, request, clientKey, hubKey, operatorKey :=
		testCooperativeCloseFixture(t, KindPromotion, 5_000)
	template, err := NewCooperativeCloseTemplate(
		terms, source, request, 70_000, 30_000, 5,
	)
	require.NoError(t, err)
	settlement := completeTestCooperativeClose(
		t, template, terms, source, request, clientKey, hubKey,
		operatorKey,
	)

	for _, test := range []struct {
		name          string
		partialSigner Party
	}{
		{
			name: "before signatures",
		},
		{
			name:          "after client signature",
			partialSigner: PartyClient,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator := activeCooperativeChannel(
				t, terms, source,
			)
			_, _, err := coordinator.Apply(
				t.Context(), terms.ID, &RequestCooperativeClose{
					Request: request,
				},
			)
			require.NoError(t, err)
			if test.partialSigner != 0 {
				_, _, err = coordinator.Apply(
					t.Context(), terms.ID,
					&CooperativeCloseSigned{
						Close: settlement,
						Party: test.partialSigner,
					},
				)
				require.NoError(t, err)
			}

			record, actions, err := coordinator.Apply(
				t.Context(), terms.ID, &SourceSpent{
					OutPoint: source.OutPoint,
					SpendingTxID: chainhash.Hash{
						102,
					},
				},
			)
			require.NoError(t, err)
			require.Equal(
				t, PhaseMaterializing, record.Snapshot.Phase,
			)
			require.Nil(t, record.Snapshot.CooperativeCloseRequest)
			require.Nil(t, record.Snapshot.CooperativeClose)
			require.False(t, record.Snapshot.ClientCloseSigned)
			require.False(t, record.Snapshot.HubCloseSigned)
			require.IsType(
				t, &PublishChannel{},
				requireOneAction(t, actions),
			)
		})
	}
}

// TestCooperativeCloseLifecycle proves a finalized in-Ark OOR close is separate
// from backing materialization and requires both lnd databases to archive
// before the channel is terminal.
func TestCooperativeCloseLifecycle(t *testing.T) {
	t.Parallel()

	terms, source, request, clientKey, hubKey, operatorKey :=
		testCooperativeCloseFixture(t, KindPromotion, 5_000)
	coordinator := activeCooperativeChannel(t, terms, source)
	backing := testBacking(t, terms, source)

	record, actions, err := coordinator.Apply(
		t.Context(), terms.ID, &RequestCooperativeClose{
			Request: request,
		},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseCoopClosing, record.Snapshot.Phase)
	require.IsType(
		t, &NegotiateCooperativeClose{}, requireOneAction(t, actions),
	)

	template, err := NewCooperativeCloseTemplate(
		terms, source, request, 70_000, 30_000, 5,
	)
	require.NoError(t, err)
	settlement := completeTestCooperativeClose(
		t, template, terms, source, request, clientKey, hubKey,
		operatorKey,
	)
	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &CooperativeCloseSigned{
			Close: settlement,
			Party: PartyClient,
		},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseCoopClosing, record.Snapshot.Phase)
	require.True(t, record.Snapshot.ClientCloseSigned)
	require.Empty(t, actions)
	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &CooperativeCloseSigned{
			Close: settlement,
			Party: PartyHub,
		},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseCoopCloseSigned, record.Snapshot.Phase)
	require.True(t, record.Snapshot.HubCloseSigned)
	require.IsType(
		t, &PublishCooperativeClose{}, requireOneAction(t, actions),
	)
	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &ChannelActive{
			ChannelPointHash:  backing.ChannelPoint.Hash,
			ChannelPointIndex: backing.ChannelPoint.Index,
		},
	)
	require.NoError(t, err)
	require.Empty(t, actions)
	require.Equal(t, PhaseCoopCloseSigned, record.Snapshot.Phase)

	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &CooperativeClosePublished{
			TxID: settlement.TxID,
		},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseCoopClosePublished, record.Snapshot.Phase)
	require.IsType(
		t, &FinalizeCooperativeClose{}, requireOneAction(t, actions),
	)

	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &CooperativeCloseFinalized{
			Party: PartyClient,
		},
	)
	require.NoError(t, err)
	require.Empty(t, actions)
	require.Equal(t, PhaseCoopClosePublished, record.Snapshot.Phase)
	require.True(t, record.Snapshot.ClientCloseFinalized)

	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &CooperativeCloseFinalized{
			Party: PartyHub,
		},
	)
	require.NoError(t, err)
	require.Empty(t, actions)
	require.Equal(t, PhaseClosed, record.Snapshot.Phase)
	require.True(t, record.Snapshot.HubCloseFinalized)
	require.Equal(
		t, backing.ChannelPoint, record.Snapshot.Backing.ChannelPoint,
	)
	require.False(t, record.Snapshot.BackingPublished)
}

// TestMaterializationSupersedesCooperativeNegotiation proves chain evidence
// can move an endpoint into lnd resolution before the cooperative-close OOR is
// finalized. This keeps a peer from blocking force close by going offline.
func TestMaterializationSupersedesCooperativeNegotiation(t *testing.T) {
	t.Parallel()

	terms, source, request, clientKey, hubKey, operatorKey :=
		testCooperativeCloseFixture(t, KindPromotion, 5_000)
	template, err := NewCooperativeCloseTemplate(
		terms, source, request, 70_000, 30_000, 5,
	)
	require.NoError(t, err)
	settlement := completeTestCooperativeClose(
		t, template, terms, source, request, clientKey, hubKey,
		operatorKey,
	)
	backing := testBacking(t, terms, source)

	tests := []struct {
		name   string
		signed bool
	}{
		{
			name: "negotiating",
		},
		{
			name:   "fully signed",
			signed: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator := activeCooperativeChannel(
				t, terms, source,
			)
			_, _, err := coordinator.Apply(
				t.Context(), terms.ID,
				&RequestCooperativeClose{
					Request: request,
				},
			)
			require.NoError(t, err)
			if test.signed {
				for _, party := range []Party{
					PartyClient,
					PartyHub,
				} {
					_, _, err = coordinator.Apply(
						t.Context(), terms.ID,
						&CooperativeCloseSigned{
							Close: settlement,
							Party: party,
						},
					)
					require.NoError(t, err)
				}
			}

			record, actions, err := coordinator.Apply(
				t.Context(), terms.ID, &BackingObserved{
					TxID: backing.ChannelPoint.Hash,
				},
			)
			require.NoError(t, err)
			require.Equal(t, PhaseOnChain, record.Snapshot.Phase)
			require.Nil(t, record.Snapshot.CooperativeCloseRequest)
			require.Nil(t, record.Snapshot.CooperativeClose)
			require.Empty(t, actions)
		})
	}
}

// TestCooperativeCloseAbortReturnsActive proves a failure before the signed
// transaction is durable does not strand a quiesced channel.
func TestCooperativeCloseAbortReturnsActive(t *testing.T) {
	t.Parallel()

	terms, source, request, _, _, _ := testCooperativeCloseFixture(
		t, KindReceiveIntent, 5_000,
	)
	coordinator := activeCooperativeChannel(t, terms, source)
	_, _, err := coordinator.Apply(
		t.Context(), terms.ID, &RequestCooperativeClose{
			Request: request,
		},
	)
	require.NoError(t, err)
	record, actions, err := coordinator.Apply(
		t.Context(), terms.ID, &CooperativeCloseAborted{},
	)
	require.NoError(t, err)
	require.Empty(t, actions)
	require.Equal(t, PhaseActive, record.Snapshot.Phase)
	require.Nil(t, record.Snapshot.CooperativeCloseRequest)
	require.Nil(t, record.Snapshot.CooperativeClose)
}

// TestOORChannelCanAbortBeforePONR proves a definitively failed prepared
// transfer releases its source before lnd funding is canceled.
func TestOORChannelCanAbortBeforePONR(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	terms := testTerms(t, KindReceiveIntent)
	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	binding := testBinding(terms)
	backing := testBacking(t, terms, binding)

	for _, event := range []Event{
		&BindVTXO{
			Binding: binding,
		},
		&FundingPeerReady{},
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
		_, _, err = coordinator.Apply(t.Context(), terms.ID, event)
		require.NoError(t, err)
	}

	record, actions, err := coordinator.Apply(
		t.Context(), terms.ID, &OORAborted{
			SessionID: binding.OORSessionID,
			Reason:    "operator rejected transfer",
		},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseCancelling, record.Snapshot.Phase)
	require.IsType(t, &CancelFunding{}, requireOneAction(t, actions))
	require.True(t, record.Snapshot.OORAborted)

	record, _, err = coordinator.Apply(
		t.Context(), terms.ID, &FundingCanceled{},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseFailed, record.Snapshot.Phase)
}

// TestPromotionWaitsForOORFinalization proves a client-funded promotion uses
// the same OOR commit gate as a hub-funded receive intent.
func TestPromotionWaitsForOORFinalization(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	coordinator, err := NewCoordinator(store)
	require.NoError(t, err)
	terms := testTerms(t, KindPromotion)
	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)

	binding := testBinding(terms)
	backing := testBacking(t, terms, binding)
	for _, event := range []Event{
		&BindVTXO{
			Binding: binding,
		},
		&BackingSigned{
			Backing: backing,
		},
		&FundingFinalized{
			Party: PartyHub,
		},
	} {
		_, _, err = coordinator.Apply(t.Context(), terms.ID, event)
		require.NoError(t, err)
	}

	record, actions, err := coordinator.Apply(
		t.Context(), terms.ID, &FundingFinalized{
			Party: PartyClient,
		},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseBackingReady, record.Snapshot.Phase)
	require.IsType(t, &CommitOOR{}, requireOneAction(t, actions))
	require.False(t, record.Snapshot.OORFinalized)

	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &OORFinalized{
			SessionID: binding.OORSessionID,
		},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseBackingReady, record.Snapshot.Phase)
	require.IsType(t, &PrepareRecovery{}, requireOneAction(t, actions))

	record, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &RecoveryPackageInstalled{},
	)
	require.NoError(t, err)
	require.Equal(t, PhaseActivating, record.Snapshot.Phase)
	require.IsType(t, &ActivateChannel{}, requireOneAction(t, actions))
}

// TestFundingFinalizationRequiresBacking ensures an edge-triggered lnd
// notification cannot become the only durable key for recovery.
func TestFundingFinalizationRequiresBacking(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	terms := testTerms(t, KindReceiveIntent)
	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	_, _, err = coordinator.Apply(
		t.Context(), terms.ID, &BindVTXO{
			Binding: testBinding(terms),
		},
	)
	require.NoError(t, err)

	_, _, err = coordinator.Apply(
		t.Context(), terms.ID, &FundingFinalized{
			Party: PartyHub,
		},
	)
	require.ErrorContains(t, err, "before signed backing")
}

// TestCoordinatorIdempotency verifies duplicate facts do not advance the
// store revision or re-emit an action.
func TestCoordinatorIdempotency(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	coordinator, err := NewCoordinator(store)
	require.NoError(t, err)
	terms := testTerms(t, KindReceiveIntent)
	requested, err := coordinator.Request(t.Context(), terms)
	require.NoError(t, err)

	repeated, err := coordinator.Request(t.Context(), terms.Clone())
	require.NoError(t, err)
	require.Equal(t, requested.Revision, repeated.Revision)

	binding := testBinding(terms)
	bound, _, err := coordinator.Apply(
		t.Context(), terms.ID, &BindVTXO{
			Binding: binding,
		},
	)
	require.NoError(t, err)

	duplicate, actions, err := coordinator.Apply(
		t.Context(), terms.ID, &BindVTXO{
			Binding: binding.Clone(),
		},
	)
	require.NoError(t, err)
	require.Empty(t, actions)
	require.Equal(t, bound.Revision, duplicate.Revision)

	restarted, err := NewCoordinator(store)
	require.NoError(t, err)
	work, err := restarted.ResumeAll(t.Context())
	require.NoError(t, err)
	require.Empty(t, work)

	_, actions, err = restarted.Apply(
		t.Context(), terms.ID, &FundingPeerReady{},
	)
	require.NoError(t, err)
	require.IsType(t, &NegotiateFunding{}, requireOneAction(t, actions))

	restarted, err = NewCoordinator(store)
	require.NoError(t, err)
	work, err = restarted.ResumeAll(t.Context())
	require.NoError(t, err)
	require.Len(t, work, 1)
	require.IsType(t, &NegotiateFunding{}, work[0].Action)

	changedTerms := terms.Clone()
	changedTerms.Capacity++
	_, err = coordinator.Request(t.Context(), changedTerms)
	require.ErrorContains(t, err, "different terms")
}

// TestFindByPendingChannelID resolves the durable intent used by lnd's
// responder-side channel acceptor.
func TestFindByPendingChannelID(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	terms := testTerms(t, KindReceiveIntent)
	expected, err := coordinator.Request(t.Context(), terms)
	require.NoError(t, err)

	record, err := coordinator.FindByPendingChannelID(
		t.Context(), terms.PendingChannelID,
	)
	require.NoError(t, err)
	require.Equal(t, expected, record)

	_, err = coordinator.FindByPendingChannelID(
		t.Context(), [32]byte{99},
	)
	require.ErrorIs(t, err, ErrNotFound)

	conflicting := testTerms(t, KindReceiveIntent)
	conflicting.ID[0] = 99
	conflicting.PendingChannelID = terms.PendingChannelID
	_, err = coordinator.Request(t.Context(), conflicting)
	require.ErrorContains(t, err, "belongs to another Ark channel")
}

// TestReceiveIntentRejectsUnsafeTerms verifies the hub must own all initial
// receive-channel liquidity.
func TestReceiveIntentRejectsUnsafeTerms(t *testing.T) {
	t.Parallel()

	terms := testTerms(t, KindReceiveIntent)
	terms.Funder = PartyClient

	_, err := NewState(terms)
	require.ErrorContains(t, err, "hub funded")
}

// TestChannelTermsRejectUnsafeRefundDelay verifies a funder cannot reclaim the
// VTXO before the channel parties have the configured reaction window.
func TestChannelTermsRejectUnsafeRefundDelay(t *testing.T) {
	t.Parallel()

	terms := testTerms(t, KindPromotion)
	terms.VTXO.FunderDelay = terms.VTXO.ChannelDelay + 431

	_, err := NewState(terms)
	require.ErrorContains(t, err, "preserve the reaction window")
}

// TestBindingRejectsDifferentChannelPolicy verifies an OOR preparation cannot
// bind an arbitrary output while retaining otherwise valid channel terms.
func TestBindingRejectsDifferentChannelPolicy(t *testing.T) {
	t.Parallel()

	terms := testTerms(t, KindPromotion)
	binding := testBinding(terms)
	binding.PolicyTemplate[0] ^= 1

	err := binding.Validate(terms)
	require.ErrorContains(t, err, "policy does not match")
}

// TestBackingMustBeSignedAndBound rejects a transaction that lnd could not
// safely activate against the exact VTXO.
func TestBackingMustBeSignedAndBound(t *testing.T) {
	t.Parallel()

	terms := testTerms(t, KindPromotion)
	binding := testBinding(terms)
	backing := testBacking(t, terms, binding)

	tx := wire.NewMsgTx(2)
	require.NoError(t, tx.Deserialize(bytes.NewReader(backing.Transaction)))
	tx.TxIn[0].Witness = nil
	backing.Transaction = serializeTx(t, tx)
	backing.ChannelPoint.Hash = tx.TxHash()

	err := backing.Validate(terms, binding)
	require.ErrorContains(t, err, "not fully signed")
}

// TestCoordinatorDoesNotEmitBeforeCommit verifies failed persistence cannot
// leak the side effect produced by an in-memory transition.
func TestCoordinatorDoesNotEmitBeforeCommit(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	coordinator, err := NewCoordinator(store)
	require.NoError(t, err)
	terms := testTerms(t, KindReceiveIntent)
	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)

	store.writeErr = errors.New("disk unavailable")
	_, actions, err := coordinator.Apply(
		t.Context(), terms.ID, &BindVTXO{
			Binding: testBinding(terms),
		},
	)
	require.ErrorIs(t, err, store.writeErr)
	require.Empty(t, actions)
}

// TestPartialFundingFactsDoNotRestartNegotiation verifies callback progress
// stays within one native lnd funding attempt.
func TestPartialFundingFactsDoNotRestartNegotiation(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	terms := testTerms(t, KindReceiveIntent)
	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	binding := testBinding(terms)
	_, actions, err := coordinator.Apply(
		t.Context(), terms.ID, &BindVTXO{
			Binding: binding,
		},
	)
	require.NoError(t, err)
	require.Empty(t, actions)
	_, actions, err = coordinator.Apply(
		t.Context(), terms.ID, &FundingPeerReady{},
	)
	require.NoError(t, err)
	require.IsType(t, &NegotiateFunding{}, requireOneAction(t, actions))

	for _, event := range []Event{
		&BackingSigned{
			Backing: testBacking(t, terms, binding),
		},
		&FundingFinalized{
			Party: PartyClient,
		},
	} {
		_, actions, err = coordinator.Apply(
			t.Context(), terms.ID, event,
		)
		require.NoError(t, err)
		require.Empty(t, actions)
	}
}

// memoryStore is a revisioned in-memory implementation for coordinator tests.
type memoryStore struct {
	mu       sync.Mutex
	records  map[ID]Record
	writeErr error
}

// newMemoryStore creates an empty test store.
func newMemoryStore() *memoryStore {
	return &memoryStore{records: make(map[ID]Record)}
}

// Create stores a new requested channel at revision one.
func (s *memoryStore) Create(_ context.Context, snapshot Snapshot) (Record,
	error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	id := snapshot.Terms.ID
	if _, ok := s.records[id]; ok {
		return Record{}, ErrConflict
	}
	record := Record{Snapshot: snapshot.Clone(), Revision: 1}
	s.records[id] = record

	return cloneRecord(record), nil
}

// Get loads one channel record.
func (s *memoryStore) Get(_ context.Context, id ID) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.records[id]
	if !ok {
		return Record{}, ErrNotFound
	}

	return cloneRecord(record), nil
}

// GetByPendingChannelID loads one record by its lnd funding correlation ID.
func (s *memoryStore) GetByPendingChannelID(_ context.Context,
	pendingID [32]byte) (Record, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, record := range s.records {
		if record.Snapshot.Terms.PendingChannelID == pendingID {
			return cloneRecord(record), nil
		}
	}

	return Record{}, ErrNotFound
}

// GetByChannelPoint loads one record by its signed lnd funding outpoint.
func (s *memoryStore) GetByChannelPoint(_ context.Context,
	channelPoint wire.OutPoint) (Record, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, record := range s.records {
		backing := record.Snapshot.Backing
		if backing != nil && backing.ChannelPoint == channelPoint {
			return cloneRecord(record), nil
		}
	}

	return Record{}, ErrNotFound
}

// ListNonTerminal loads resumable records in stable ID order.
func (s *memoryStore) ListNonTerminal(_ context.Context) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	records := make([]Record, 0, len(s.records))
	for _, record := range s.records {
		if record.Snapshot.Phase.IsTerminal() {
			continue
		}

		records = append(records, cloneRecord(record))
	}
	sort.Slice(records, func(i, j int) bool {
		return bytes.Compare(
			records[i].Snapshot.Terms.ID[:],
			records[j].Snapshot.Terms.ID[:],
		) < 0
	})

	return records, nil
}

// CompareAndSwap advances a channel only from the expected revision.
func (s *memoryStore) CompareAndSwap(_ context.Context, id ID, revision uint64,
	snapshot Snapshot) (Record, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.writeErr != nil {
		return Record{}, s.writeErr
	}
	record, ok := s.records[id]
	if !ok {
		return Record{}, ErrNotFound
	}
	if record.Revision != revision {
		return Record{}, ErrConflict
	}

	record = Record{
		Snapshot: snapshot.Clone(),
		Revision: revision + 1,
	}
	s.records[id] = record

	return cloneRecord(record), nil
}

// cloneRecord returns a record with isolated snapshot byte slices.
func cloneRecord(record Record) Record {
	record.Snapshot = record.Snapshot.Clone()

	return record
}

// activeCooperativeChannel drives one channel through OOR finalization and
// virtual lnd activation.
func activeCooperativeChannel(t *testing.T, terms Terms,
	source VTXOBinding) *Coordinator {

	t.Helper()
	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	backing := testBacking(t, terms, source)
	for _, event := range []Event{
		&BindVTXO{
			Binding: source,
		},
		&BackingSigned{
			Backing: backing,
		},
		&FundingFinalized{
			Party: PartyClient,
		},
		&FundingFinalized{
			Party: PartyHub,
		},
		&OORFinalized{
			SessionID: source.OORSessionID,
		},
		&RecoveryPackageInstalled{},
		&ChannelActive{
			ChannelPointHash:  backing.ChannelPoint.Hash,
			ChannelPointIndex: backing.ChannelPoint.Index,
		},
	} {
		if _, receive := event.(*BackingSigned); receive &&
			terms.Kind == KindReceiveIntent {

			_, _, err = coordinator.Apply(
				t.Context(), terms.ID, &FundingPeerReady{},
			)
			require.NoError(t, err)
		}
		_, _, err = coordinator.Apply(t.Context(), terms.ID, event)
		require.NoError(t, err)
	}

	return coordinator
}

// testTerms creates valid immutable terms for one channel kind.
func testTerms(t *testing.T, kind Kind) Terms {
	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	hubKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	newPolicyKey := func() [33]byte {
		key, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		var serialized [33]byte
		copy(serialized[:], key.PubKey().SerializeCompressed())

		return serialized
	}

	var clientNodeKey, hubNodeKey [33]byte
	copy(clientNodeKey[:], clientKey.PubKey().SerializeCompressed())
	copy(hubNodeKey[:], hubKey.PubKey().SerializeCompressed())

	terms := Terms{
		ID: ID{
			1,
			2,
			byte(kind),
		},
		Kind:   kind,
		Funder: PartyClient,
		PendingChannelID: [32]byte{
			4,
			5,
			byte(kind),
		},
		Capacity: btcutil.Amount(100_000),
		ReservedSCID: lnwire.ShortChannelID{
			BlockHeight: 16_000_000 + uint32(kind),
			TxIndex:     uint32(kind),
		}.ToUint64(),
		ClientNodeKey: clientNodeKey,
		HubNodeKey:    hubNodeKey,
		VTXO: VTXOTerms{
			ClientArkKey:     newPolicyKey(),
			HubArkKey:        newPolicyKey(),
			ArkOperatorKey:   newPolicyKey(),
			ClientChannelKey: newPolicyKey(),
			HubChannelKey:    newPolicyKey(),
			FunderKey:        newPolicyKey(),
			ChannelDelay:     144,
			FunderDelay:      576,
			MinExitDelay:     144,
		},
	}
	if kind == KindReceiveIntent {
		terms.Funder = PartyHub
		terms.PaymentHash = [32]byte{9, 9, byte(kind)}
	}

	return terms
}

// testBinding creates one exact prepared OOR output for channel terms.
func testBinding(terms Terms) VTXOBinding {
	policy, pkScript, err := terms.VTXO.Artifacts()
	if err != nil {
		panic(err)
	}
	amount := terms.Capacity + 1_000
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash: chainhash.Hash{10, byte(terms.Kind)},
		},
	})
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x51}})
	tx.AddTxOut(&wire.TxOut{Value: 2, PkScript: []byte{0x51}})
	tx.AddTxOut(&wire.TxOut{Value: int64(amount), PkScript: pkScript})
	var arkTransaction bytes.Buffer
	if err := tx.Serialize(&arkTransaction); err != nil {
		panic(err)
	}
	sessionID := [32]byte(tx.TxHash())

	return VTXOBinding{
		OORSessionID: sessionID,
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash(sessionID),
			Index: 2,
		},
		Amount:         amount,
		ArkTransaction: arkTransaction.Bytes(),
		PolicyTemplate: policy,
		PkScript:       pkScript,
	}
}

// testBacking creates a signed single-input funding transaction.
func testBacking(t *testing.T, terms Terms, binding VTXOBinding) Backing {
	t.Helper()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: binding.OutPoint,
		Witness:          wire.TxWitness{bytes.Repeat([]byte{1}, 64)},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    int64(terms.Capacity),
		PkScript: []byte{0x51, 0x20, 1},
	})

	return Backing{
		Transaction: serializeTx(t, tx),
		ChannelPoint: wire.OutPoint{
			Hash:  tx.TxHash(),
			Index: 0,
		},
	}
}

// serializeTx serializes one witness transaction for a test fixture.
func serializeTx(t *testing.T, tx *wire.MsgTx) []byte {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, tx.Serialize(&buf))

	return buf.Bytes()
}

// requireOneAction asserts and returns one emitted coordinator action.
func requireOneAction(t *testing.T, actions []Action) Action {
	t.Helper()
	require.Len(t, actions, 1)

	return actions[0]
}
