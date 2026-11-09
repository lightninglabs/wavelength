package arkchannel

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// nativeExecutorHarness implements the installed thin native boundaries.
type nativeExecutorHarness struct {
	negotiatedID ID
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

// NegotiateChannel records the native funding dispatch.
func (h *nativeExecutorHarness) NegotiateChannel(_ context.Context, id ID,
	_ Terms, _ VTXOBinding) error {

	h.negotiatedID = id

	return nil
}

// TestNativeExecutorRoutesOnlySubsystemBoundaries verifies the adapter has no
// channel state of its own.
func TestNativeExecutorRoutesOnlySubsystemBoundaries(t *testing.T) {
	t.Parallel()
	harness := &nativeExecutorHarness{}
	executor, err := NewNativeExecutor(PartyClient, harness, harness)
	require.NoError(t, err)
	require.NoError(t, executor.BindChannelEventSink(harnessSink{}))
	require.Equal(t, 2, harness.boundSinks)
	terms := testTerms(t, KindPromotion)
	source := testBinding(terms)
	require.NoError(
		t,
		executor.ValidatePreparedOOR(
			t.Context(), terms, source,
		),
	)
	require.Equal(t, 1, harness.validations)
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
}

// harnessSink accepts callback events for executor wiring tests.
type harnessSink struct{}

// ApplyLocalEvent accepts one trusted local callback event.
func (harnessSink) ApplyLocalEvent(context.Context, ID, Event) (Record, error) {
	return Record{}, nil
}
