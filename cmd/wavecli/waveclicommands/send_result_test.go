package waveclicommands

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/stretchr/testify/require"
)

// TestSendResultFromInspectionLightning verifies a settled Lightning send
// distills down to its amount, fee, payment hash, and, crucially, the preimage
// proof of payment, pulled from the correlated swap trace.
func TestSendResultFromInspectionLightning(t *testing.T) {
	resp := &wavewalletrpc.InspectActivityResponse{
		Entry: &wavewalletrpc.WalletEntry{
			Id:           "hash-id",
			Kind:         wavewalletrpc.EntryKind_ENTRY_KIND_SEND,
			Status:       entryStatusComplete,
			AmountSat:    -1000,
			FeeSat:       0,
			Counterparty: "lntbs10u1p4ytrgq",
			Progress: &wavewalletrpc.WalletEntryProgress{
				PaymentHash:  "hash-id",
				VtxoOutpoint: "5d2f:0",
			},
		},
		Swap: &wavewalletrpc.ActivitySwapTrace{
			Preimage:       "a6837e6d",
			SettlementType: "SWAP_SETTLEMENT_TYPE_IN_ARK",
			PaymentHash:    "hash-id",
		},
	}

	res := sendResultFromInspection(resp)
	require.Equal(t, "COMPLETE", res.Status)
	require.Equal(t, "SEND", res.Kind)
	require.Equal(t, int64(-1000), res.AmountSat)
	require.Equal(t, int64(0), res.FeeSat)
	require.Equal(t, "IN_ARK", res.Settlement)
	require.Equal(t, "lntbs10u1p4ytrgq", res.Destination)
	require.Equal(t, "hash-id", res.PaymentHash)
	require.Equal(t, "a6837e6d", res.Preimage)
	require.Equal(t, "5d2f:0", res.VtxoOutpoint)
	require.Equal(t, "hash-id", res.ID)

	// An onchain-only field must stay absent for a Lightning send.
	require.Empty(t, res.Txid)
}

// TestSendResultFromEntryFallsBackToRequestHash verifies a dispatched (still
// pending) receipt recovers the payment hash from the invoice request when the
// progress snapshot has not surfaced one yet.
func TestSendResultFromEntryFallsBackToRequestHash(t *testing.T) {
	entry := &wavewalletrpc.WalletEntry{
		Id:     "entry-id",
		Kind:   wavewalletrpc.EntryKind_ENTRY_KIND_SEND,
		Status: entryStatusPending,
		Request: &wavewalletrpc.WalletEntryRequest{
			Request: &wavewalletrpc.
				WalletEntryRequest_LightningInvoice{
				LightningInvoice: &wavewalletrpc.
					LightningInvoiceRequest{
					PaymentHash: "req-hash",
				},
			},
		},
	}

	res := sendResultFromEntry(entry)
	require.Equal(t, "PENDING", res.Status)
	require.Equal(t, "req-hash", res.PaymentHash)
	require.Empty(t, res.Preimage)
	require.Empty(t, res.Settlement)
}

// TestPrintSendResult verifies the compact summary renders as pretty JSON on
// the provided writer and that empty optional fields are omitted entirely, so
// a pipeline can key off field presence.
func TestPrintSendResult(t *testing.T) {
	var buf bytes.Buffer
	res := sendResult{
		Status:    "COMPLETE",
		Kind:      "SEND",
		AmountSat: -1000,
		FeeSat:    2,
		Preimage:  "a6837e6d",
		ID:        "entry-id",
	}
	require.NoError(t, printSendResult(&buf, res))

	// The output must be a single well-formed JSON document followed by a
	// trailing newline so `da send | jq` consumes it cleanly.
	require.True(t, bytes.HasSuffix(buf.Bytes(), []byte("\n")))

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	require.Equal(t, "COMPLETE", decoded["status"])
	require.Equal(t, "SEND", decoded["kind"])
	require.Equal(t, float64(-1000), decoded["amount_sat"])
	require.Equal(t, float64(2), decoded["fee_sat"])
	require.Equal(t, "a6837e6d", decoded["preimage"])
	require.Equal(t, "entry-id", decoded["id"])

	// Optional fields left empty must be omitted, not rendered as empty
	// strings.
	require.NotContains(t, decoded, "settlement")
	require.NotContains(t, decoded, "txid")
	require.NotContains(t, decoded, "payment_hash")
	require.NotContains(t, decoded, "vtxo_outpoint")
	require.NotContains(t, decoded, "destination")
}

// TestTrimSettlementType verifies the settlement enum prefix is stripped to its
// short label and non-prefixed input is left untouched.
func TestTrimSettlementType(t *testing.T) {
	require.Equal(
		t, "IN_ARK", trimSettlementType("SWAP_SETTLEMENT_TYPE_IN_ARK"),
	)
	require.Equal(t, "", trimSettlementType(""))
	require.Equal(t, "LIGHTNING", trimSettlementType("LIGHTNING"))
}
