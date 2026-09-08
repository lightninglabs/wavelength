package lnruntime

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/stretchr/testify/require"
)

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
