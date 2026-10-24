package vtxo

import (
	"testing"

	"github.com/lightninglabs/wavelength/round"
	"github.com/stretchr/testify/require"
)

// TestForfeitReplacementTransfersReservation proves a replacement signs for a
// fresh connector without exposing the VTXO to another spend, and rejects late
// requests and releases from an abandoned attempt.
func TestForfeitReplacementTransfersReservation(t *testing.T) {
	t.Parallel()
	h := newRealVTXOSigningHarness(t)
	descriptor := h.newTestDescriptor()
	h.withState(&ForfeitingState{VTXO: descriptor, NewRoundID: "abandoned"})
	connector := h.newTestConnectorOutput()
	event := &round.ForfeitRequestEvent{RoundID: "replacement",
		ReplacedRoundIDs: []string{
			"abandoned",
		},
		ConnectorOutpoint:     h.newTestOutpoint(),
		ConnectorPkScript:     connector.PkScript,
		ConnectorAmount:       connector.Value,
		ServerForfeitPkScript: h.newServerForfeitScript()}
	h.store.On("UpdateVTXOStatus", h.ctx, descriptor.Outpoint,
		VTXOStatusForfeiting).Return(nil)
	_, err := h.sendEvent(event)
	require.NoError(t, err)
	current := assertStateReal[*ForfeitingState](h)
	require.Equal(t, "replacement", current.NewRoundID)
	require.Equal(t, event.ConnectorOutpoint, current.ConnectorOutpoint)
	require.NotNil(t, current.ForfeitTx)
	for _, out := range h.outboxMessages {
		if update, ok := out.(*VTXOStatusUpdate); ok {
			require.Equal(t, VTXOStatusForfeiting, update.NewStatus)
		}
	}
	// Neither a stale release nor an unrelated claimed predecessor can free
	// or replace the current owner's reservation.
	_, err = h.sendEvent(&round.ForfeitReleasedEvent{RoundID: "abandoned"})
	require.ErrorIs(t, err, ErrForfeitReleaseRoundMismatch)
	event.RoundID = "unrelated"
	_, err = h.sendEvent(event)
	require.ErrorContains(t, err, "does not own")
	require.Equal(
		t, "replacement",
		assertStateReal[*ForfeitingState](h).NewRoundID,
	)
}

// TestForfeitReplacementFailureReleasesUntransferredReservation covers a
// replacement that fails before reaching the next connector-signing phase.
func TestForfeitReplacementFailureReleasesUntransferredReservation(
	t *testing.T) {

	t.Parallel()
	h := newVTXOTestHarness(t)
	descriptor := h.newTestDescriptor()
	h.withState(&ForfeitingState{VTXO: descriptor, NewRoundID: "original"})
	h.store.On("UpdateVTXOStatus", h.ctx, descriptor.Outpoint,
		VTXOStatusLive).Return(nil)
	_, err := h.sendEvent(
		&round.ForfeitReleasedEvent{
			RoundID:          "replacement",
			ReplacedRoundIDs: []string{"original"},
		},
	)
	require.NoError(t, err)
	require.Equal(t, descriptor, assertState[*LiveState](h).VTXO)
}
