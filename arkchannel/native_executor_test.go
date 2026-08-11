package arkchannel

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/stretchr/testify/require"
)

// nativeExecutorHarness implements each thin native boundary.
type nativeExecutorHarness struct {
	confirmed    chainhash.Hash
	negotiatedID ID
	cancelledID  ID
	publishedID  ID
}

// CancelChannel records native funding cleanup.
func (h *nativeExecutorHarness) CancelChannel(_ context.Context, id ID, _ Terms,
	_ *Backing) error {

	h.cancelledID = id

	return nil
}

// ConfirmBacking records lnd virtual activation.
func (h *nativeExecutorHarness) ConfirmBacking(txID chainhash.Hash) error {
	h.confirmed = txID

	return nil
}

// NegotiateChannel records the native funding dispatch.
func (h *nativeExecutorHarness) NegotiateChannel(_ context.Context, id ID,
	_ Terms, _ VTXOBinding) error {

	h.negotiatedID = id

	return nil
}

// MaterializeChannel records the unroller dispatch.
func (h *nativeExecutorHarness) MaterializeChannel(_ context.Context, id ID,
	_ VTXOBinding, _ Backing) error {

	h.publishedID = id

	return nil
}

// TestNativeExecutorRoutesOnlySubsystemBoundaries verifies the adapter has no
// channel state of its own.
func TestNativeExecutorRoutesOnlySubsystemBoundaries(t *testing.T) {
	t.Parallel()

	harness := &nativeExecutorHarness{}
	executor, err := NewNativeExecutor(harness, harness, harness)
	require.NoError(t, err)
	terms := testTerms(t, KindPromotion)
	source := testBinding(terms)
	backing := testBacking(t, terms, source)

	require.NoError(
		t,
		executor.Execute(
			t.Context(), terms.ID, &NegotiateFunding{
				Terms:  terms,
				Source: source,
			},
		),
	)
	require.Equal(t, terms.ID, harness.negotiatedID)

	require.NoError(
		t,
		executor.Execute(
			t.Context(), terms.ID, &CancelFunding{
				Terms:   terms,
				Backing: &backing,
			},
		),
	)
	require.Equal(t, terms.ID, harness.cancelledID)

	require.NoError(
		t,
		executor.Execute(
			t.Context(), terms.ID, &ActivateChannel{
				Terms:   terms,
				Backing: backing,
			},
		),
	)
	require.Equal(t, backing.ChannelPoint.Hash, harness.confirmed)

	require.NoError(
		t,
		executor.Execute(
			t.Context(), terms.ID, &PublishChannel{
				Terms:   terms,
				Source:  source,
				Backing: backing,
			},
		),
	)
	require.Equal(t, terms.ID, harness.publishedID)
}

var (
	_ VirtualFundingActivator = (*nativeExecutorHarness)(nil)
	_ FundingNegotiator       = (*nativeExecutorHarness)(nil)
	_ ChannelMaterializer     = (*nativeExecutorHarness)(nil)
)
