package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestDurableLocalCommands verifies all local requests through the runtime
// codec.
func TestDurableLocalCommands(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	refresh := &RefreshVTXORequest{
		VTXOOutpoint: wire.OutPoint{
			Index: 17,
		}, Amount: 123,
		OperatorFee: -1, Automatic: true, BatchExpiry: 234,
		TriggerHeight: -2, ExpandCohort: true,
		OperatorKey: key.PubKey(),
		PolicyTemplate: []byte{
			1,
			2,
			3,
		},
		OwnerKey: keychain.KeyDescriptor{
			PubKey: key.PubKey(), KeyLocator: keychain.KeyLocator{
				Family: 7,
				Index:  8,
			},
		},
		SigningKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 9,
				Index:  10,
			},
		},
	}
	index := 0
	commands := []actormsg.RoundReceivable{
		&GetClientStateRequest{},
		&RegisterVTXORequestsRequest{
			Amounts: []btcutil.Amount{
				0,
				-1,
				345,
			},
			Assets: []AssetVTXORequest{
				{
					AmountSat:   67,
					AssetRef:    "asset",
					AssetAmount: 89,
				},
			},
			ChangeIndex: &index,
		},
		&RegisterVTXORequestsRequest{},
		refresh,
		&RefreshVTXOCohortRequest{
			Requests: []*RefreshVTXORequest{
				refresh,
				refresh,
			},
		},
		&actormsg.TriggerBoardMsg{
			Amounts: []btcutil.Amount{
				123,
				456,
			},
			Outpoints: []wire.OutPoint{
				{
					Index: 7,
				},
				{
					Index: 8,
				},
			},
			Change: &types.LeaveRequest{
				Output: &wire.TxOut{
					Value: 9,
					PkScript: []byte{
						10,
					},
				},
			},
		},
	}
	codec := actor.NewMessageCodec()
	require.NoError(
		t, codec.Register(
			durableClientCommandType,
			func() actor.TLVMessage {
				return &durableClientCommand{}
			},
		),
	)
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
			require.Equal(t, command, restored)
			again, err := newDurableClientCommand(restored)
			require.NoError(t, err)
			reencoded, err := codec.Encode(again)
			require.NoError(t, err)
			require.Equal(t, raw, reencoded)
		})
	}
}

// TestDurableLocalCommandFreezesKeys guards callers reusing mutable input
// buffers.
func TestDurableLocalCommandFreezesKeys(t *testing.T) {
	request := &RefreshVTXORequest{PolicyTemplate: []byte{1}}
	frozen, err := newDurableClientCommand(request)
	require.NoError(t, err)
	request.PolicyTemplate[0] = 2
	restored, err := frozen.message(nil)
	require.NoError(t, err)
	refresh, ok := restored.(*RefreshVTXORequest)
	require.True(t, ok)
	require.Equal(t, []byte{1}, refresh.PolicyTemplate)
	_, err = newDurableClientCommand(&RefreshVTXOCohortRequest{
		Requests: []*RefreshVTXORequest{nil},
	})
	require.ErrorContains(t, err, "missing refresh request")
}
