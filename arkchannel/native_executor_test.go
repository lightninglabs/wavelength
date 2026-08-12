package arkchannel

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// nativeExecutorHarness implements each thin native boundary.
type nativeExecutorHarness struct {
	confirmed    chainhash.Hash
	negotiatedID ID
	committedID  ID
	abortedID    ID
	cancelledID  ID
	publishedID  ID
	recoveryID   ID
	handedOff    wire.OutPoint
	forceClosed  wire.OutPoint
	closeID      ID
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

// PrepareChannelRecovery records installation of the source package.
func (h *nativeExecutorHarness) PrepareChannelRecovery(_ context.Context, id ID,
	_ Terms, _ VTXOBinding) error {

	h.recoveryID = id

	return nil
}

// ResumeForceCloseChannel records commitment publication through lnd.
func (h *nativeExecutorHarness) ResumeForceCloseChannel(
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

// PublishCooperativeClose records direct close publication.
func (h *nativeExecutorHarness) PublishCooperativeClose(_ context.Context,
	id ID, _ Terms, _ VTXOBinding, _ CooperativeClose) error {

	h.closeID = id

	return nil
}

// FinalizeCooperativeClose records lnd archival.
func (h *nativeExecutorHarness) FinalizeCooperativeClose(_ context.Context,
	id ID, _ Terms, _ Backing, _ VTXOBinding, _ CooperativeCloseRequest,
	_ CooperativeClose) error {

	h.closeID = id

	return nil
}

// TestNativeExecutorRoutesOnlySubsystemBoundaries verifies the adapter has no
// channel state of its own.
func TestNativeExecutorRoutesOnlySubsystemBoundaries(t *testing.T) {
	t.Parallel()

	harness := &nativeExecutorHarness{}
	executor, err := NewNativeExecutor(
		PartyClient, harness, harness, harness, harness, harness,
		harness, harness,
	)
	require.NoError(t, err)
	require.NoError(t, executor.BindChannelEventSink(harnessSink{}))
	require.Equal(t, 4, harness.boundSinks)
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
			t.Context(), terms.ID, &CommitOOR{
				Terms:  terms,
				Source: source,
			},
		),
	)
	require.Equal(t, terms.ID, harness.committedID)

	require.NoError(
		t,
		executor.Execute(
			t.Context(), terms.ID, &AbortOOR{
				Terms:  terms,
				Source: source,
				Reason: "peer rejected funding",
			},
		),
	)
	require.Equal(t, terms.ID, harness.abortedID)

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
	require.Equal(t, backing.ChannelPoint, harness.handedOff)
}

// TestNativeExecutorFencesOORActionsToFunder proves the responder cannot commit
// or abort the funder's OOR actor session, while either recovery-ready endpoint
// can publish the shared source package after activation.
func TestNativeExecutorFencesOORActionsToFunder(t *testing.T) {
	t.Parallel()

	harness := &nativeExecutorHarness{}
	executor, err := NewNativeExecutor(
		PartyHub, harness, harness, harness, harness, harness, harness,
		harness,
	)
	require.NoError(t, err)
	terms := testTerms(t, KindPromotion)
	source := testBinding(terms)
	require.NoError(
		t,
		executor.ValidatePreparedOOR(
			t.Context(), terms, source,
		),
	)
	require.Zero(t, harness.validations)

	require.NoError(
		t,
		executor.Execute(
			t.Context(), terms.ID, &CommitOOR{
				Terms:  terms,
				Source: source,
			},
		),
	)
	require.NoError(
		t,
		executor.Execute(
			t.Context(), terms.ID, &AbortOOR{
				Terms:  terms,
				Source: source,
				Reason: "test abort",
			},
		),
	)
	require.Equal(t, ID{}, harness.committedID)
	require.Equal(t, ID{}, harness.abortedID)

	backing := testBacking(t, terms, source)
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
	require.Equal(t, backing.ChannelPoint, harness.handedOff)
	require.Equal(t, terms.ID, harness.publishedID)
}

// harnessSink accepts callback events for executor wiring tests.
type harnessSink struct{}

// Apply accepts one callback event.
func (harnessSink) Apply(context.Context, ID, Event) (Record, error) {
	return Record{}, nil
}

var (
	_ VirtualFundingActivator  = (*nativeExecutorHarness)(nil)
	_ FundingNegotiator        = (*nativeExecutorHarness)(nil)
	_ OORTransferController    = (*nativeExecutorHarness)(nil)
	_ ChannelMaterializer      = (*nativeExecutorHarness)(nil)
	_ ChannelOnchainHandoff    = (*nativeExecutorHarness)(nil)
	_ ChannelForceCloser       = (*nativeExecutorHarness)(nil)
	_ ChannelCooperativeCloser = (*nativeExecutorHarness)(nil)
	_ ChannelEventSinkBinder   = (*nativeExecutorHarness)(nil)
	_ ChannelEventSink         = harnessSink{}
)
