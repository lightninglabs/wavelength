package round

import (
	"testing"

	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/stretchr/testify/require"
)

// TestDurableRegistrationTrigger restores a local trigger through the actual
// ingress codec before dispatch. Queued intents must reach registration, and
// an empty queue must retain its typed no-op error rather than a wire error.
func TestDurableRegistrationTrigger(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "empty"
		if pending {
			name = "queued"
		}
		t.Run(name, func(t *testing.T) {
			h := newActorTestHarness(t)
			h.setupMockRoundStoreForStart()
			require.NoError(t, h.start())
			if pending {
				h.sendWalletConfirmation(
					h.newTestBoardingIntent(),
				)
				h.sendVTXORequests(50000)
				h.clearServerMessages()
			}

			input, err := durableClientIngress(
				&TriggerRegistrationRequest{},
			)
			require.NoError(t, err)
			codec := newDurableClientCodec()
			encoded, err := codec.Encode(input)
			require.NoError(t, err)
			restored, err := codec.Decode(encoded)
			require.NoError(t, err)
			command, ok := restored.(actormsg.RoundReceivable)
			require.True(t, ok)
			result := h.receive(command)
			if !pending {
				require.ErrorIs(
					t, result.Err(), ErrNoPendingRound,
				)

				return
			}
			require.NoError(t, result.Err())
			h.assertFSMState("IntentSentState")
			h.assertServerMessageSent("SendClientEventRequest")
		})
	}

	// Local triggers must not weaken validation of persisted remote pushes.
	_, err := newDurableClientCodec().Encode(&ServerMessageNotification{
		Message: &IntentRequested{},
	})
	require.ErrorContains(t, err, "unsupported round push method")
}
