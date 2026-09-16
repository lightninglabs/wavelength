package round

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestDurableClientCommandCodec uses the runtime's type envelope to verify
// command reconstruction, presence flags, and stable re-encoding.
func TestDurableClientCommandCodec(t *testing.T) {
	codec := actor.NewMessageCodec()
	require.NoError(
		t, codec.Register(
			durableClientCommandType, func() actor.TLVMessage {
				return &durableClientCommand{}
			},
		),
	)
	commands := []actormsg.RoundReceivable{
		&RegisterIntentRequest{
			Package: &IntentPackage{}, TriggerRegistration: true,
		},
		&actormsg.RegisterIntentMsg{
			TriggerRegistration: true,
		},
		&WalletBoardingConfirmed{
			Intent: &wallet.BoardingIntent{},
		},
		&VTXORequestsReceived{},
		&TimeoutMsg{
			TimeoutID: "temp:registration",
		},
		&ConfirmationEvent{
			BlockHeight:   -1,
			Confirmations: 3,
		},
		&CancelRoundRequest{},
		&CancelRoundRequest{
			RoundKey: fn.Some(RoundKeyStr("")),
		},
		&CancelRoundRequest{
			RoundKey: fn.Some(RoundKeyStr("round")),
		},
	}
	for _, command := range commands {
		t.Run(command.MessageType(), func(t *testing.T) {
			frozen, err := newDurableClientCommand(command)
			require.NoError(t, err)
			raw, err := codec.Encode(frozen)
			require.NoError(t, err)
			decoded, err := codec.Decode(raw)
			require.NoError(t, err)
			envelope, ok := decoded.(*durableClientCommand)
			require.True(t, ok)
			restored, err := envelope.message(nil)
			require.NoError(t, err)
			again, err := newDurableClientCommand(restored)
			require.NoError(t, err)
			reencoded, err := codec.Encode(again)
			require.NoError(t, err)
			require.Equal(t, raw, reencoded)
		})
	}
}

// TestDurableClientCommandFreezesInput ensures mailbox data cannot change
// when the caller reuses its request buffers after sending.
func TestDurableClientCommandFreezesInput(t *testing.T) {
	req := &RegisterIntentRequest{Package: &IntentPackage{
		Intents: Intents{VTXOs: []types.VTXORequest{{
			Amount: 123, PolicyTemplate: []byte{
				1,
			}, PkScript: []byte{
				2,
			},
		}}},
	}}
	frozen, err := newDurableClientCommand(req)
	require.NoError(t, err)
	req.Package.Intents.VTXOs[0].Amount = 456
	req.Package.Intents.VTXOs[0].PolicyTemplate[0] = 3
	req.Package.Intents.VTXOs[0].PkScript[0] = 4
	restored, err := frozen.message(nil)
	require.NoError(t, err)
	intent, ok := restored.(*RegisterIntentRequest)
	require.True(t, ok)
	require.EqualValues(t, 123, intent.Package.Intents.VTXOs[0].Amount)
	require.Equal(
		t, []byte{1}, intent.Package.Intents.VTXOs[0].PolicyTemplate,
	)
	require.Equal(t, []byte{2}, intent.Package.Intents.VTXOs[0].PkScript)
}

// TestDurableClientCommandMissingFields rejects incomplete envelopes rather
// than interpreting a damaged message as a different command.
func TestDurableClientCommandMissingFields(t *testing.T) {
	var command durableClientCommand
	require.Error(t, command.Decode(bytes.NewReader(nil)))
	command.Kind = 99
	require.Error(t, command.Encode(&bytes.Buffer{}))
}

// TestDurableClientCommandDispatch preserves the normal cancellation response
// after reconstruction through the actor's production dispatch path.
func TestDurableClientCommandDispatch(t *testing.T) {
	a := &RoundClientActor{cfg: &RoundClientConfig{}, log: btclog.Disabled}
	req := &CancelRoundRequest{RoundKey: fn.Some(RoundKeyStr("missing"))}
	frozen, err := newDurableClientCommand(req)
	require.NoError(t, err)
	ordinary, err := a.Receive(t.Context(), req).Unpack()
	require.NoError(t, err)
	restored, err := a.Receive(t.Context(), frozen).Unpack()
	require.NoError(t, err)
	require.Equal(t, ordinary, restored)
}
