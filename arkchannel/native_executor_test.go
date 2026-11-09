package arkchannel

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/stretchr/testify/require"
)

// nativeExecutorHarness implements each installed thin native boundary.
type nativeExecutorHarness struct {
	confirmed    chainhash.Hash
	negotiatedID ID
	committedID  ID
	recoveryID   ID
	boundSinks   int
	validations  int
}

// BindChannelEventSink records each subsystem binding.
func (h *nativeExecutorHarness) BindChannelEventSink(ChannelEventSink) error {
	h.boundSinks++

	return nil
}

// ValidatePreparedOOR records successful validation by the OOR owner.
func (h *nativeExecutorHarness) ValidatePreparedOOR(_ context.Context, _ Terms,
	_ VTXOBinding) error {

	h.validations++

	return nil
}

// CommitPreparedOOR records release of the prepared transfer.
func (h *nativeExecutorHarness) CommitPreparedOOR(_ context.Context, id ID,
	_ Terms, _ VTXOBinding) error {

	h.committedID = id

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

// PrepareChannelRecovery records installation of the source package.
func (h *nativeExecutorHarness) PrepareChannelRecovery(_ context.Context, id ID,
	_ Terms, _ VTXOBinding) error {

	h.recoveryID = id

	return nil
}

// TestNativeExecutorRoutesOnlySubsystemBoundaries verifies native routing does
// not duplicate the channel state machine.
func TestNativeExecutorRoutesOnlySubsystemBoundaries(t *testing.T) {
	t.Parallel()
	h := &nativeExecutorHarness{}
	e, err := NewNativeExecutor(PartyClient, h, h, h)
	require.NoError(t, err)
	require.NoError(t, e.BindChannelEventSink(harnessSink{}))
	require.Equal(t, 2, h.boundSinks)
	terms := testTerms(t, KindPromotion)
	source := testBinding(terms)
	backing := testBacking(t, terms, source)
	require.NoError(t, e.ValidatePreparedOOR(t.Context(), terms, source))
	require.Equal(t, 1, h.validations)
	for _, action := range []Action{
		&NegotiateFunding{
			Terms:  terms,
			Source: source,
		},
		&CommitOOR{
			Terms:  terms,
			Source: source,
		},
		&PrepareRecovery{
			Terms:  terms,
			Source: source,
		},
		&ActivateChannel{
			Terms:   terms,
			Backing: backing,
		},
	} {
		require.NoError(t, e.Execute(t.Context(), terms.ID, action))
	}
	require.Equal(t, terms.ID, h.negotiatedID)
	require.Equal(t, terms.ID, h.committedID)
	require.Equal(t, terms.ID, h.recoveryID)
	require.Equal(t, backing.ChannelPoint.Hash, h.confirmed)
}

// TestNativeExecutorFencesOORActionsToFunder prevents the responding endpoint
// from releasing the funding endpoint's prepared OOR transfer.
func TestNativeExecutorFencesOORActionsToFunder(t *testing.T) {
	t.Parallel()
	h := &nativeExecutorHarness{}
	e, err := NewNativeExecutor(PartyHub, h, h, h)
	require.NoError(t, err)
	terms := testTerms(t, KindPromotion)
	source := testBinding(terms)
	require.NoError(t, e.ValidatePreparedOOR(t.Context(), terms, source))
	require.Zero(t, h.validations)
	require.NoError(
		t,
		e.Execute(
			t.Context(), terms.ID, &CommitOOR{
				Terms:  terms,
				Source: source,
			},
		),
	)
	require.Equal(t, ID{}, h.committedID)
}

// harnessSink accepts callback events for executor wiring tests.
type harnessSink struct{}

// ApplyLocalEvent accepts one trusted local callback event.
func (harnessSink) ApplyLocalEvent(context.Context, ID, Event) (Record, error) {
	return Record{}, nil
}
