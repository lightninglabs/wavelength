package round

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestDurableEffectsRoundTrip retains all fields consumed by local deliveries.
func TestDurableEffectsRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 123, time.UTC)
	txid := chainhash.Hash{1}
	effects := []ClientOutMsg{
		&RegisterConfirmationRequest{
			CallerID: "watch",
			PkScript: []byte{
				2,
			},
			Txid:        &txid,
			TargetConfs: 3,
			HeightHint:  4,
		},
		&RegisterConfirmationRequest{
			PkScript: []byte{},
		},
		&StartTimeoutReq{
			RoundKey: "temp:key",
			Phase:    TimeoutPhaseRegistration,
			Duration: time.Minute,
		},
		&CancelTimeoutReq{
			RoundKey: "temp:key",
			Phase:    TimeoutPhaseRegistration,
		},
		&ReleaseForfeitReservation{
			Outpoints: []wire.OutPoint{
				{
					Index: 5,
				},
			},
		},
		&DropCustomForfeitReservation{
			Outpoints: []wire.OutPoint{
				{
					Index: 6,
				},
			},
		},
		&VTXOCreatedNotification{
			VTXOs: []*ClientVTXO{
				{
					Outpoint: wire.OutPoint{
						Index: 7,
					},
					Amount: 100,
					PolicyTemplate: []byte{
						18,
					}, PkScript: []byte{
						19,
					},
				},
			},
			Outflows: []RoundLedgerOutflow{
				{
					AmountSat: 8,
					IdempotencyKey: []byte{
						9,
					},
				},
			},
			RoundID: "round", CommitmentTxID: txid, BatchExpiry: 10,
			CreatedHeight: -1, OperatorFeeSat: 12,
			OperatorFeeType: "fee",
		},
		&ForfeitRequestToVTXO{
			VTXOOutpoint: wire.OutPoint{
				Index: 13,
			}, RoundID: "round",
			ConnectorOutpoint: wire.OutPoint{
				Index: 14,
			}, ConnectorAmount: -1,
			ConnectorPkScript: []byte{
				15,
			}, ServerForfeitPkScript: []byte{
				16,
			},
		},
		&ForfeitConfirmedToVTXO{
			VTXOOutpoint: wire.OutPoint{
				Index: 17,
			},
			CommitmentTxID: txid,
			BlockHeight:    -1,
		},
	}
	codec := actor.NewMessageCodec()
	codec.MustRegister(
		durableClientEffectType,
		func() actor.TLVMessage { return &durableClientEffect{} },
	)
	for _, effect := range effects {
		t.Run(fmt.Sprintf("%T", effect), func(t *testing.T) {
			frozen, err := newDurableClientEffect(effect, now)
			require.NoError(t, err)
			raw, err := codec.Encode(frozen)
			require.NoError(t, err)
			decoded, err := codec.Decode(raw)
			require.NoError(t, err)
			envelope, ok := decoded.(*durableClientEffect)
			require.True(t, ok)
			restored, err := envelope.localMessage(now)
			require.NoError(t, err)
			if original, ok := effect.(*StartTimeoutReq); ok {
				expected := *original
				expected.deadline = now.Add(original.Duration)
				require.Equal(t, &expected, restored)
			} else {
				require.Equal(t, effect, restored)
			}
			again, err := newDurableClientEffect(restored, now)
			require.NoError(t, err)
			encoded, err := codec.Encode(again)
			require.NoError(t, err)
			require.Equal(t, raw, encoded)
		})
	}
}

// TestDurableTimerDeadline does not renew a timeout on retries or clock jumps.
func TestDurableTimerDeadline(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 123, time.UTC)
	request := &StartTimeoutReq{
		RoundKey: "temp:key", Phase: TimeoutPhaseRegistration,
		Duration: time.Minute,
	}
	frozen, err := newDurableClientEffect(request, now)
	require.NoError(t, err)
	for _, offset := range []time.Duration{
		-time.Hour,
		30 * time.Second,
		2 * time.Minute,
	} {
		restored, err := frozen.localMessage(now.Add(offset))
		require.NoError(t, err)
		timer, ok := restored.(*StartTimeoutReq)
		require.True(t, ok)
		require.True(t, timer.deadline.Equal(now.Add(time.Minute)))
		recaptured, err := newDurableClientEffect(
			timer, now.Add(offset),
		)
		require.NoError(t, err)
		require.Equal(t, frozen.Payload, recaptured.Payload)
		require.Equal(
			t,
			max(
				time.Duration(0), time.Minute-offset,
			),
			timer.Duration,
		)
	}
	_, _, due, err := decodeDurableTimer(frozen.Payload)
	require.NoError(t, err)
	require.True(t, due.Equal(now.Add(time.Minute)))
}

// TestDurableServerEffect preserves the existing transport's dedupe envelope.
func TestDurableServerEffect(t *testing.T) {
	original := &JoinRoundAcceptOutbox{
		RoundID: RoundID{
			1,
		},
		QuoteID: [32]byte{
			2,
		},
	}
	frozen, err := newDurableClientEffect(original, time.Now())
	require.NoError(t, err)
	original.QuoteID[0] = 9
	first, err := frozen.serverRequest()
	require.NoError(t, err)
	second, err := frozen.serverRequest()
	require.NoError(t, err)
	require.NotEmpty(t, first.MsgID)
	require.NotEmpty(t, first.IdempotencyKey)
	require.Equal(t, first.MsgID, second.MsgID)
	require.Equal(t, first.IdempotencyKey, second.IdempotencyKey)
	require.Equal(t, roundpb.ServiceName, first.Service)
	require.Equal(t, roundpb.MethodAcceptQuote, first.Method)
	message, err := first.Message.ToProto().Unpack()
	require.NoError(t, err)
	expected := &roundpb.JoinRoundAccept{
		RoundId: original.RoundID.String(),
		QuoteId: make([]byte, 32),
	}
	expected.QuoteId[0] = 2
	require.True(t, proto.Equal(expected, message))
	var reencoded bytes.Buffer
	require.NoError(t, first.Encode(&reencoded))
	require.Equal(t, frozen.Payload, reencoded.Bytes())
}
