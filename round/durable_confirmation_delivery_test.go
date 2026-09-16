package round

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/wavelength/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestDurableConfirmationMapping retains block provenance on live and restored
// watches.
func TestDurableConfirmationMapping(t *testing.T) {
	for _, restoredWatch := range []bool{false, true} {
		name := "live"
		if restoredWatch {
			name = "restored"
		}
		t.Run(name, func(t *testing.T) {
			h := newActorTestHarness(t)
			event := chainsource.ConfirmationEvent{
				Txid: chainhash.Hash{
					1,
				}, BlockHash: chainhash.Hash{
					2,
				},
				BlockHeight: 123, NumConfs: 3,
			}
			if restoredWatch {
				h.actor.registerCommitmentConfirmation(
					t.Context(), event.Txid,
					fn.None[*psbt.Packet](), nil,
				)
			} else {
				request := &RegisterConfirmationRequest{
					Txid: &event.Txid, HeightHint: 100,
				}
				err := h.actor.processConfirmationRequest(
					t.Context(), request,
				)
				require.NoError(t, err)
			}
			require.Len(t, h.chainSource.registrations, 1)
			registration := h.chainSource.registrations[0]
			watch := registration.NotifyActor.UnwrapOrFail(t)
			require.NoError(t, watch.Tell(t.Context(), event))
			translated, ok := h.selfRef.waitForMessage(time.Second)
			require.True(t, ok)
			command, err := newDurableClientCommand(translated)
			require.NoError(t, err)
			decoded, err := command.message(nil)
			require.NoError(t, err)
			confirmation, ok := decoded.(*ConfirmationEvent)
			require.True(t, ok)
			require.Equal(
				t, event.BlockHash, confirmation.BlockHash,
			)
			require.Equal(t, event.Txid, confirmation.Txid)
			require.Equal(
				t, event.BlockHeight, confirmation.BlockHeight,
			)
		})
	}
}

// failingConfirmationRef retains query support while failing watch
// registration.
type failingConfirmationRef struct {
	*mockChainSourceRef
	err error
}

// Tell reports a failed enqueue to the confirmation service.
func (r *failingConfirmationRef) Tell(context.Context,
	chainsource.ChainSourceMsg) error {

	return r.err
}

// TestDurableConfirmationDeliveryFailure keeps a failed watch pending for
// retry.
func TestDurableConfirmationDeliveryFailure(t *testing.T) {
	h := newActorTestHarness(t)
	failed := errors.New("chain source unavailable")
	h.actor.cfg.ChainSource = &failingConfirmationRef{
		mockChainSourceRef: h.chainSource,
		err:                failed,
	}
	effect, err := newDurableClientEffect(&RegisterConfirmationRequest{
		CallerID: "watch", HeightHint: 123,
	}, time.Now())
	require.NoError(t, err)
	require.ErrorIs(
		t,
		h.actor.deliverDurableEffect(
			t.Context(), effect,
		),
		failed,
	)
}
