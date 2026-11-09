package arkchannel

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// nativeExecutorHarness implements each installed thin native boundary.
type nativeExecutorHarness struct {
	confirmed    chainhash.Hash
	negotiatedID ID
	committedID  ID
	abortedID    ID
	cancelledID  ID
	publishedID  ID
	handedOff    wire.OutPoint
	forceClosed  wire.OutPoint
	closeID      ID
	recoveryID   ID
	boundSinks   int
	validations  int
}

// HandoffChannel records transfer to lnd's on-chain lifecycle.
func (h *nativeExecutorHarness) HandoffChannel(
	channelPoint wire.OutPoint) error {

	h.handedOff = channelPoint

	return nil
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

// AbortPreparedOOR records release of the prepared source reservation.
func (h *nativeExecutorHarness) AbortPreparedOOR(_ context.Context, id ID,
	_ Terms, _ VTXOBinding, _ string) error {

	h.abortedID = id

	return nil
}

// CancelChannel records native funding cleanup.
func (h *nativeExecutorHarness) CancelChannel(_ context.Context, id ID, _ Terms,
	_ VTXOBinding, _ *Backing, _ string) error {

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

// PrepareChannelRecovery records installation of the source package.
func (h *nativeExecutorHarness) PrepareChannelRecovery(_ context.Context, id ID,
	_ Terms, _ VTXOBinding) error {

	h.recoveryID = id

	return nil
}

// EnsureForceCloseChannel records commitment publication through lnd.
func (h *nativeExecutorHarness) EnsureForceCloseChannel(
	channelPoint wire.OutPoint) error {

	h.forceClosed = channelPoint

	return nil
}

// MaterializeChannel records the unroller dispatch.
func (h *nativeExecutorHarness) MaterializeChannel(_ context.Context, id ID,
	_ Terms, _ VTXOBinding, _ Backing) error {

	h.publishedID = id

	return nil
}

// NegotiateCooperativeClose records direct close negotiation.
func (h *nativeExecutorHarness) NegotiateCooperativeClose(_ context.Context,
	id ID, _ Terms, _ VTXOBinding, _ Backing,
	_ CooperativeCloseRequest) error {

	h.closeID = id

	return nil
}

// TestNativeExecutorRoutesOnlySubsystemBoundaries verifies native routing does
// not duplicate the channel state machine.
func TestNativeExecutorRoutesOnlySubsystemBoundaries(t *testing.T) {
	t.Parallel()
	h := &nativeExecutorHarness{}
	e, err := NewNativeExecutor(PartyClient, h, h, h, h, h, h, h)
	require.NoError(t, err)
	require.NoError(t, e.BindChannelEventSink(harnessSink{}))
	require.Equal(t, 4, h.boundSinks)
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
	require.NoError(
		t,
		e.Execute(
			t.Context(), terms.ID, &AbortOOR{
				Terms:  terms,
				Source: source,
				Reason: "abandoned",
			},
		),
	)
	require.NoError(
		t,
		e.Execute(
			t.Context(), terms.ID, &CancelFunding{
				Terms:  terms,
				Source: source,
				Reason: "abandoned",
			},
		),
	)
	require.Equal(t, terms.ID, h.abortedID)
	require.Equal(t, terms.ID, h.cancelledID)
	require.NoError(
		t,
		e.Execute(
			t.Context(), terms.ID, &PublishChannel{
				Terms:   terms,
				Source:  source,
				Backing: backing,
			},
		),
	)
	require.Equal(t, terms.ID, h.publishedID)
	require.Equal(t, backing.ChannelPoint, h.handedOff)
	require.NoError(
		t,
		e.Execute(
			t.Context(), terms.ID, &ForceCloseChannel{
				Backing: backing,
			},
		),
	)
	require.Equal(t, backing.ChannelPoint, h.forceClosed)
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
	e, err := NewNativeExecutor(PartyHub, h, h, h, h, h, h, h)
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
	require.NoError(
		t,
		e.Execute(
			t.Context(), terms.ID, &AbortOOR{
				Terms:  terms,
				Source: source,
				Reason: "abandoned",
			},
		),
	)
	require.Equal(t, ID{}, h.abortedID)
}

// harnessSink accepts callback events for executor wiring tests.
type harnessSink struct{}

// ApplyLocalEvent accepts one trusted local callback event.
func (harnessSink) ApplyLocalEvent(context.Context, ID, Event) (Record, error) {
	return Record{}, nil
}
