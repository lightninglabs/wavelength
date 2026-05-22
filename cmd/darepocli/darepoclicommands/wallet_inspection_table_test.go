package darepoclicommands

import (
	"bytes"
	"testing"

	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"github.com/stretchr/testify/require"
)

// TestRenderWalletInspectionExpanded verifies the inspection renderer keeps
// identifiers intact while omitting internal hidden-row flags.
func TestRenderWalletInspectionExpanded(t *testing.T) {
	t.Parallel()

	resp := &walletrpc.InspectActivityResponse{
		Entry: &walletrpc.WalletEntry{
			Id:        "payment-hash",
			Kind:      walletrpc.EntryKind_ENTRY_KIND_SEND,
			Status:    walletrpc.EntryStatus_ENTRY_STATUS_PENDING,
			AmountSat: -1234,
		},
		Swap: &walletrpc.ActivitySwapTrace{
			PaymentHash:      "payment-hash",
			Direction:        "SWAP_DIRECTION_PAY",
			State:            "SWAP_STATE_WAITING_FOR_CLAIM",
			Pending:          true,
			AmountSat:        1234,
			VhtlcOutpoint:    "vhtlc-txid:0",
			VhtlcAmountSat:   1234,
			FundingSessionId: "funding-session",
		},
		Vtxos: []*walletrpc.ActivityVTXOTrace{
			{
				Id:        "input-id",
				AmountSat: 999745,
				Role:      "spent_input",
				Ours:      true,
				Source:    "ledger",
			},
			{
				Id:        "change-id:1",
				AmountSat: 998511,
				Role:      "change_output",
				Ours:      true,
				Source:    "ledger",
			},
		},
		LedgerRows: []*walletrpc.ActivityLedgerTrace{
			{
				EntryId:            13,
				Type:               "oor",
				Subtype:            "vtxo_sent",
				AmountSat:          999745,
				HiddenFromActivity: true,
				Role:               "spent_input",
				SessionId:          "input-id",
			},
		},
		Notes: []string{
			"best effort",
		},
	}

	var out bytes.Buffer
	require.NoError(t, renderWalletInspectionExpanded(&out, resp))

	got := out.String()
	require.Contains(t, got, "Activity\n")
	require.Contains(t, got, "- payment_hash: payment-hash")
	require.Contains(t, got, "VTXOs\n")
	require.Contains(t, got, "spent_input")
	require.Contains(t, got, "Ledger\n")
	require.Contains(t, got, "- id: input-id")
	require.Contains(t, got, "- session_id: input-id")
	require.Contains(t, got, "true")
	require.NotContains(t, got, "HIDDEN")
	require.NotContains(t, got, "hidden")
	require.Contains(t, got, "Notes\n- best effort")
}
