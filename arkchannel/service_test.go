package arkchannel

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// serviceExecutor completes native callbacks through the public Service API.
type serviceExecutor struct {
	t           *testing.T
	service     *Service
	counts      map[string]int
	failPublish int
}

type staticFundingFinalizationSource struct {
	finalized bool
}

type noOpActionExecutor struct{}

// ValidatePreparedOOR accepts fixtures already covered by state validation.
func (*noOpActionExecutor) ValidatePreparedOOR(context.Context, Terms,
	VTXOBinding) error {

	return nil
}

// Execute accepts already-durable actions without completing callbacks.
func (*noOpActionExecutor) Execute(context.Context, ID, Action) error {
	return nil
}

// ValidatePreparedOOR accepts fixtures already covered by state validation.
func (*serviceExecutor) ValidatePreparedOOR(context.Context, Terms,
	VTXOBinding) error {

	return nil
}

// FundingFinalized returns one fixed lnd database observation.
func (s *staticFundingFinalizationSource) FundingFinalized(context.Context,
	Terms, Backing) (bool, error) {

	return s.finalized, nil
}

// Execute simulates native lnd and materializer completion callbacks.
func (e *serviceExecutor) Execute(ctx context.Context, id ID,
	action Action) error {

	e.counts[fmt.Sprintf("%T", action)]++

	switch action := action.(type) {
	case *NegotiateFunding:
		backing := testBacking(e.t, action.Terms, action.Source)
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
			if _, err := e.service.Apply(
				ctx, id, event,
			); err != nil {
				return err
			}
		}

	case *CommitOOR:
		_, err := e.service.Apply(ctx, id, &OORFinalized{
			SessionID: action.Source.OORSessionID,
		})

		return err

	case *PrepareRecovery:
		_, err := e.service.Apply(
			ctx, id, &RecoveryPackageInstalled{},
		)

		return err

	case *AbortOOR:
		_, err := e.service.Apply(ctx, id, &OORAborted{
			SessionID: action.Source.OORSessionID,
			Reason:    action.Reason,
		})

		return err

	case *ActivateChannel:
		_, err := e.service.Apply(ctx, id, &ChannelActive{
			ChannelPointHash:  action.Backing.ChannelPoint.Hash,
			ChannelPointIndex: action.Backing.ChannelPoint.Index,
		})

		return err

	case *CancelFunding:
		_, err := e.service.Apply(ctx, id, &FundingCanceled{})

		return err

	case *PublishChannel:
		if e.failPublish > 0 {
			e.failPublish--

			return fmt.Errorf("injected handoff failure")
		}
		_, err := e.service.Apply(ctx, id, &BackingPublished{
			TxID: action.Backing.ChannelPoint.Hash,
		})

		return err

	case *ForceCloseChannel:
		return nil

	default:
		return fmt.Errorf("unexpected service action %T", action)
	}

	return nil
}

// TestServiceReplaysSourceConflictAction proves repeating the same durable
// chain fact retries a backing publication that failed after state commit.
func TestServiceReplaysSourceConflictAction(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	executor := &serviceExecutor{
		t: t, counts: make(map[string]int), failPublish: 1,
	}
	service, err := NewService(coordinator, executor)
	require.NoError(t, err)
	executor.service = service
	terms := testTerms(t, KindPromotion)
	binding := testBinding(terms)
	_, err = service.PromoteVTXO(t.Context(), terms, binding)
	require.NoError(t, err)

	conflict := &SourceSpent{
		OutPoint: binding.OutPoint,
		SpendingTxID: chainhash.Hash{
			44,
		},
	}
	_, err = service.Apply(t.Context(), terms.ID, conflict)
	require.ErrorContains(t, err, "injected handoff failure")
	record, err := service.GetChannel(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, PhaseMaterializing, record.Snapshot.Phase)

	record, err = service.Apply(t.Context(), terms.ID, conflict)
	require.NoError(t, err)
	require.Equal(t, PhaseOnChain, record.Snapshot.Phase)
	require.Equal(t, 2, executor.counts["*arkchannel.PublishChannel"])
	require.Equal(t, 1, executor.counts["*arkchannel.ForceCloseChannel"])
}

// TestServiceResumesMaterializationAction proves a retry cannot acknowledge a
// durable materializing state without re-running the failed handoff and
// publication action.
func TestServiceResumesMaterializationAction(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	executor := &serviceExecutor{
		t: t, counts: make(map[string]int),
	}
	service, err := NewService(coordinator, executor)
	require.NoError(t, err)
	executor.service = service

	terms := testTerms(t, KindPromotion)
	_, err = service.PromoteVTXO(
		t.Context(), terms, testBinding(terms),
	)
	require.NoError(t, err)
	executor.failPublish = 1

	_, err = service.Materialize(t.Context(), terms.ID)
	require.ErrorContains(t, err, "injected handoff failure")
	record, err := service.GetChannel(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, PhaseMaterializing, record.Snapshot.Phase)

	record, err = service.Materialize(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, PhaseOnChain, record.Snapshot.Phase)
	require.Equal(t, 2, executor.counts["*arkchannel.PublishChannel"])
}

// TestServicePromotesAndMaterializesVTXO verifies the compact public workflow
// drives callbacks without owning Lightning state.
func TestServicePromotesAndMaterializesVTXO(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	executor := &serviceExecutor{
		t:      t,
		counts: make(map[string]int),
	}
	service, err := NewService(coordinator, executor)
	require.NoError(t, err)
	executor.service = service

	terms := testTerms(t, KindPromotion)
	record, err := service.PromoteVTXO(
		t.Context(), terms, testBinding(terms),
	)
	require.NoError(t, err)
	require.Equal(t, PhaseActive, record.Snapshot.Phase)
	require.Equal(t, 1, executor.counts["*arkchannel.NegotiateFunding"])
	require.Equal(t, 1, executor.counts["*arkchannel.CommitOOR"])
	require.Equal(t, 1, executor.counts["*arkchannel.ActivateChannel"])

	record, err = service.Materialize(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, PhaseOnChain, record.Snapshot.Phase)
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
	service, err := NewService(coordinator, executor)
	require.NoError(t, err)
	executor.service = service
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

// TestServiceRegistersReceiveIntentWithoutFunding verifies registration does
// not spend operator liquidity until a matching prepared OOR output is known.
func TestServiceRegistersReceiveIntentWithoutFunding(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	executor := &serviceExecutor{
		t:      t,
		counts: make(map[string]int),
	}
	service, err := NewService(coordinator, executor)
	require.NoError(t, err)
	executor.service = service
	terms := testTerms(t, KindReceiveIntent)

	record, err := service.RegisterReceiveIntent(t.Context(), terms)
	require.NoError(t, err)
	require.Equal(t, PhaseRequested, record.Snapshot.Phase)

	record, err = service.BindPreparedOOR(
		t.Context(), terms.ID, testBinding(terms),
	)
	require.NoError(t, err)
	require.Equal(t, PhaseRequested, record.Snapshot.Phase)
	require.NotNil(t, record.Snapshot.Source)
	require.Empty(t, executor.counts)

	record, err = service.Apply(
		t.Context(), terms.ID, &FundingPeerReady{},
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
	service, err := NewService(coordinator, &noOpActionExecutor{})
	require.NoError(t, err)
	terms := testTerms(t, KindPromotion)
	_, err = service.RegisterPromotion(t.Context(), terms)
	require.NoError(t, err)
	binding := testBinding(terms)
	_, err = service.BindPreparedOOR(t.Context(), terms.ID, binding)
	require.NoError(t, err)
	backing := testBacking(t, terms, binding)
	_, err = service.Apply(t.Context(), terms.ID, &BackingSigned{
		Backing: backing,
	})
	require.NoError(t, err)

	source := &staticFundingFinalizationSource{finalized: true}
	require.NoError(
		t,
		service.ReconcileFunding(
			t.Context(), PartyClient, source,
		),
	)
	record, err := service.ObserveFundingFinalized(
		t.Context(), PartyHub, backing.ChannelPoint,
	)
	require.NoError(t, err)
	require.True(t, record.Snapshot.ClientFinalized)
	require.True(t, record.Snapshot.HubFinalized)
	require.Equal(t, PhaseBackingReady, record.Snapshot.Phase)
	require.True(t, record.Snapshot.ReadyToCommitOOR())

	_, err = service.ObserveFundingFinalized(
		t.Context(), PartyHub, wire.OutPoint{
			Hash: [32]byte{99},
		},
	)
	require.ErrorIs(t, err, ErrNotFound)
}

var (
	_ ActionExecutor            = (*serviceExecutor)(nil)
	_ FundingFinalizationSource = (*staticFundingFinalizationSource)(nil)
)
