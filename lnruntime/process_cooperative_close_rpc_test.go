package lnruntime

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// abortKeyRecorder captures logical request keys without sending any traffic.
type abortKeyRecorder struct {
	mailboxrpc.RPCClient
	keys []string
}

// SendRPC records the key chosen before the transport takes ownership.
func (r *abortKeyRecorder) SendRPC(_ context.Context,
	_ mailboxrpc.ServiceMethod, _ proto.Message,
	opts mailboxrpc.RPCOptions) (mailboxrpc.SendResult, error) {

	r.keys = append(r.keys, opts.IdempotencyKey)

	return mailboxrpc.SendResult{}, context.Canceled
}

// TestCloseAbortReceiptPerCall ensures a fresh decision cannot consume a
// cached successful abort from an earlier attempt to close the same channel.
func TestCloseAbortReceiptPerCall(t *testing.T) {
	t.Parallel()

	recorder := &abortKeyRecorder{}
	peer, err := NewMailboxCooperativeClosePeer(recorder)
	require.NoError(t, err)
	for range 2 {
		err := peer.AbortCooperativeClose(t.Context(), arkchannel.ID{1})
		require.ErrorIs(t, err, context.Canceled)
	}
	require.Len(t, recorder.keys, 2)
	require.NotEmpty(t, recorder.keys[0])
	require.NotEmpty(t, recorder.keys[1])
	require.NotEqual(t, recorder.keys[0], recorder.keys[1])
}

// TestArkChannelRecordToRPCRefreshPhases verifies internal cooperative phases
// never leak through the public channel API.
func TestArkChannelRecordToRPCRefreshPhases(t *testing.T) {
	t.Parallel()

	refreshTxID := chainhash.Hash{2}
	tests := []struct {
		name       string
		phase      arkchannel.Phase
		settlement *arkchannel.CooperativeClose
		want       string
	}{
		{
			name:  "refreshing",
			phase: arkchannel.PhaseCoopClosing,
			want:  "refreshing",
		},
		{
			name:  "authorized",
			phase: arkchannel.PhaseCoopCloseSigned,
			want:  "refresh_authorized",
		},
		{
			name:  "oor finalized",
			phase: arkchannel.PhaseCoopClosePublished,
			want:  "refresh_oor_finalized",
		},
		{
			name:  "refreshed",
			phase: arkchannel.PhaseClosed,
			settlement: &arkchannel.CooperativeClose{
				TxID: refreshTxID,
			},
			want: "refreshed",
		},
		{
			name:  "on-chain closed",
			phase: arkchannel.PhaseClosed,
			want:  "closed",
		},
		{
			name:  "active",
			phase: arkchannel.PhaseActive,
			want:  "active",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			record := arkchannel.Record{
				Snapshot: arkchannel.Snapshot{
					Phase:            test.phase,
					CooperativeClose: test.settlement,
				},
			}
			result := ArkChannelRecordToRPC(record)

			require.Equal(t, test.want, result.GetPhase())
			if test.settlement == nil {
				require.Empty(t, result.GetRefreshOorTxid())

				return
			}
			require.Equal(
				t, refreshTxID[:], result.GetRefreshOorTxid(),
			)
		})
	}
}
