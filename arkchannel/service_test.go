package arkchannel

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// serviceExecutor completes native callbacks through the public Service API.
type serviceExecutor struct {
	t       *testing.T
	service *Service
	counts  map[string]int
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
		_, err := e.service.Apply(ctx, id, &BackingPublished{
			TxID: action.Backing.ChannelPoint.Hash,
		})

		return err

	default:
		return fmt.Errorf("unexpected service action %T", action)
	}

	return nil
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

	record, err = service.BindVTXO(
		t.Context(), terms.ID, testBinding(terms),
	)
	require.NoError(t, err)
	require.Equal(t, PhaseActive, record.Snapshot.Phase)
	require.Equal(t, 1, executor.counts["*arkchannel.NegotiateFunding"])
}

// TestServiceRegistersReceiveIntentWithoutFunding verifies registration does
// not spend operator liquidity until a matching round output is known.
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
}

var _ ActionExecutor = (*serviceExecutor)(nil)
