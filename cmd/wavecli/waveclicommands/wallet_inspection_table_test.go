package waveclicommands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
	"github.com/stretchr/testify/require"
)

// TestRenderWalletInspectionExpanded verifies the inspection renderer keeps
// identifiers intact while omitting internal hidden-row flags.
func TestRenderWalletInspectionExpanded(t *testing.T) {
	t.Parallel()

	resp := &walletdkrpc.InspectActivityResponse{
		Entry: &walletdkrpc.WalletEntry{
			Id:        "payment-hash",
			Kind:      walletdkrpc.EntryKind_ENTRY_KIND_SEND,
			Status:    walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
			AmountSat: -1234,
		},
		Swap: &walletdkrpc.ActivitySwapTrace{
			PaymentHash:      "payment-hash",
			Direction:        "SWAP_DIRECTION_PAY",
			State:            "SWAP_STATE_WAITING_FOR_CLAIM",
			Pending:          true,
			AmountSat:        1234,
			SettlementType:   "SWAP_SETTLEMENT_TYPE_IN_ARK",
			SenderPubkey:     "sender-pubkey",
			Preimage:         "abcd1234",
			VhtlcOutpoint:    "vhtlc-txid:0",
			VhtlcAmountSat:   1234,
			FundingSessionId: "funding-session",
		},
		Vtxos: []*walletdkrpc.ActivityVTXOTrace{
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
		LedgerRows: []*walletdkrpc.ActivityLedgerTrace{
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
	require.Contains(
		t, got, "- settlement_type: SWAP_SETTLEMENT_TYPE_IN_ARK",
	)
	require.Contains(t, got, "- sender_pubkey: sender-pubkey")
	require.Contains(t, got, "- preimage: abcd1234")
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

// TestRenderWalletInspectionExpandedDeduplicatesPaymentHash verifies inspect
// output prints the Lightning payment hash once when both the request and the
// progress snapshot carry the same stable identifier.
func TestRenderWalletInspectionExpandedDeduplicatesPaymentHash(t *testing.T) {
	t.Parallel()

	const paymentHash = "bdaeb8a1100a27410a86ec49b05e7edb7758e465" +
		"a3bb6855ae840b665523048c"
	const vtxoOutpoint = "0b02c92e32692b03e4f8a336c3e21406" +
		"cf68fa4057f699e6f95b5020d2fc800a:0"

	request := &walletdkrpc.WalletEntryRequest{
		Request: &walletdkrpc.WalletEntryRequest_LightningInvoice{
			LightningInvoice: &walletdkrpc.LightningInvoiceRequest{
				Invoice:     "lntbs50u1example",
				PaymentHash: paymentHash,
			},
		},
	}

	resp := &walletdkrpc.InspectActivityResponse{
		Entry: &walletdkrpc.WalletEntry{
			Id:      paymentHash,
			Kind:    walletdkrpc.EntryKind_ENTRY_KIND_RECV,
			Status:  walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
			Request: request,
			Progress: &walletdkrpc.WalletEntryProgress{
				PaymentHash:  paymentHash,
				VtxoOutpoint: vtxoOutpoint,
			},
		},
	}

	var out bytes.Buffer
	require.NoError(t, renderWalletInspectionExpanded(&out, resp))

	got := out.String()
	require.Equal(t, 1, strings.Count(got, "- payment_hash: "))
	require.Contains(t, got, "- payment_hash: "+paymentHash)
	require.Contains(t, got, "- progress_vtxo: ")
}
