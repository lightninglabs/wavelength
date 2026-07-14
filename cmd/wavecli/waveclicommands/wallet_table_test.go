package waveclicommands

import (
	"bytes"
	"strings"
	"testing"

	wrpc "github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/stretchr/testify/require"
)

// TestRenderWalletActivityTable verifies the compact activity table columns.
func TestRenderWalletActivityTable(t *testing.T) {
	t.Parallel()

	resp := activityResponse(
		&wrpc.WalletEntry{
			Id:            "abc123",
			Kind:          wrpc.EntryKind_ENTRY_KIND_RECV,
			Status:        wrpc.EntryStatus_ENTRY_STATUS_PENDING,
			AmountSat:     10_000,
			UpdatedAtUnix: 1_700_000_000,
			Note:          "coffee",
			FeeSat:        12,
			FailureReason: "",
			CreatedAtUnix: 1_700_000_000,
			Counterparty:  "",
			Request:       lightningRequest("abc123"),
			Progress:      phase("waiting_for_payment"),
		},
		&wrpc.WalletEntry{
			Id:            "deposit-row",
			Kind:          wrpc.EntryKind_ENTRY_KIND_DEPOSIT,
			Status:        wrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
			AmountSat:     50_000,
			UpdatedAtUnix: 1_700_000_100,
			Request:       onchainRequest("bcrt1qboardingaddress"),
			Progress:      phase("confirmed"),
		},
	)

	var out bytes.Buffer
	require.NoError(t, renderWalletActivityTable(&out, resp))

	got := out.String()
	require.Contains(t, got, "LAST UPDATE")
	require.Contains(t, got, "KIND")
	require.Contains(t, got, "RECV")
	require.Contains(t, got, "PENDING")
	require.Contains(t, got, "10000")
	require.Contains(t, got, ":20.000")
	require.Contains(t, got, "abc123")
	require.Contains(t, got, "coffee")
	require.Contains(t, got, "DEPOSIT")
	require.NotContains(t, got, "REQUEST")
	require.NotContains(t, got, "invoice:lnbc1invoice")
	require.NotContains(t, got, "addr:bcrt1qboardingaddress")
	require.False(t, strings.Contains(got, "ENTRY_KIND_"))
}

// TestRenderWalletActivityTableDoesNotRenderRequest verifies the compact table
// does not print noisy request payloads.
func TestRenderWalletActivityTableDoesNotRenderRequest(t *testing.T) {
	t.Parallel()

	const id = "3c3a812c6e701284f9bf030b713c0e333041497c4548224e" +
		"8342b517044387e2"
	const invoice = "lntbs12340n1p4q7652pp58sagztrwwqfgf7dlqv9hz0qwxv" +
		"cyzjtug4yzyn5rg263wpzrsl3qdqqcqzzsxqyz5vqsp5suw29vm528" +
		"sf4uhzrlykx80le6a3gpsmrqancjfeu5gwdu4qllfq9qxpqysgqwt" +
		"nlgk7gpxflwwdeyudswafewjs6yj9npzjzqyys35a0qy6j4cts3" +
		"wyc4zjr2qvv2gsflxkam7248yw" +
		"rydqwx70xluv6thqxlkjhk4gpv0e02f"

	resp := activityResponse(&wrpc.WalletEntry{
		Id:            id,
		Kind:          wrpc.EntryKind_ENTRY_KIND_SEND,
		Status:        wrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     -1234,
		UpdatedAtUnix: 1_700_000_000,
		Request:       lightningRequestWithInvoice(invoice, id),
	})

	var out bytes.Buffer
	require.NoError(t, renderWalletActivityTable(&out, resp))

	got := out.String()
	require.Contains(t, got, id)
	require.NotContains(t, got, invoice)
	require.NotContains(t, got, "invoice:")
	require.NotContains(t, got, "REQUEST")
}

// TestRenderWalletActivityExpanded verifies the markdown-like activity view.
func TestRenderWalletActivityExpanded(t *testing.T) {
	t.Parallel()

	resp := activityResponse(&wrpc.WalletEntry{
		Id:            "abc123",
		Kind:          wrpc.EntryKind_ENTRY_KIND_RECV,
		Status:        wrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     10_000,
		FeeSat:        12,
		UpdatedAtUnix: 1_700_000_000,
		Note:          "coffee",
		Request:       lightningRequest("abc123"),
		Progress:      phase("waiting_for_payment"),
	})

	var out bytes.Buffer
	require.NoError(t, renderWalletActivityExpanded(&out, resp))

	got := out.String()
	require.Contains(t, got, "Activity\n")
	require.Contains(t, got, "- last_update:")
	require.Contains(t, got, ":20.000")
	require.Contains(t, got, "- kind: RECV")
	require.Contains(t, got, "- amount: 10000 sat")
	require.Contains(t, got, "- invoice: lnbc1invoice")
	require.Contains(t, got, "- payment_hash: abc123")
	require.Contains(t, got, "- note: coffee")
	require.NotContains(t, got, "-[ RECORD 1 ]")
	require.False(t, strings.Contains(got, "ENTRY_KIND_"))
}

// TestRenderWalletActivityExpandedNumbersMultipleEntries verifies repeated
// activity sections receive stable numbered titles.
func TestRenderWalletActivityExpandedNumbersMultipleEntries(t *testing.T) {
	t.Parallel()

	resp := activityResponse(
		&wrpc.WalletEntry{
			Id:            "send-id",
			Kind:          wrpc.EntryKind_ENTRY_KIND_SEND,
			Status:        wrpc.EntryStatus_ENTRY_STATUS_PENDING,
			AmountSat:     -1000,
			UpdatedAtUnix: 1_700_000_000,
		},
		&wrpc.WalletEntry{
			Id:            "recv-id",
			Kind:          wrpc.EntryKind_ENTRY_KIND_RECV,
			Status:        wrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
			AmountSat:     1000,
			UpdatedAtUnix: 1_700_000_100,
		},
	)

	var out bytes.Buffer
	require.NoError(t, renderWalletActivityExpanded(&out, resp))

	got := out.String()
	require.Contains(t, got, "Activity 1\n")
	require.Contains(t, got, "- id: send-id")
	require.Contains(t, got, "Activity 2\n")
	require.Contains(t, got, "- id: recv-id")
}

// TestValidateListFormat verifies that rich activity formats are restricted to
// the activity view.
func TestValidateListFormat(t *testing.T) {
	t.Parallel()

	require.NoError(
		t, validateListFormat(
			"table", wrpc.ListView_LIST_VIEW_ACTIVITY,
		),
	)
	require.NoError(
		t, validateListFormat(
			"expanded", wrpc.ListView_LIST_VIEW_ACTIVITY,
		),
	)
	require.NoError(
		t, validateListFormat(
			"x", wrpc.ListView_LIST_VIEW_ACTIVITY,
		),
	)
	require.Error(
		t, validateListFormat(
			"table", wrpc.ListView_LIST_VIEW_VTXOS,
		),
	)
	require.Error(
		t, validateListFormat(
			"yaml", wrpc.ListView_LIST_VIEW_ACTIVITY,
		),
	)
}

// TestNewActivityCmdDefaultsToTable verifies the CLI defaults to the compact
// human-readable activity view.
func TestNewActivityCmdDefaultsToTable(t *testing.T) {
	t.Parallel()

	format, err := newActivityCmd().Flags().GetString("format")
	require.NoError(t, err)
	require.Equal(t, "table", format)
}

// lightningRequest builds a test Lightning request with a default invoice.
func lightningRequest(hash string) *wrpc.WalletEntryRequest {
	return lightningRequestWithInvoice("lnbc1invoice", hash)
}

// lightningRequestWithInvoice builds a test Lightning request.
func lightningRequestWithInvoice(invoice,
	hash string) *wrpc.WalletEntryRequest {

	return &wrpc.WalletEntryRequest{
		Request: &wrpc.WalletEntryRequest_LightningInvoice{
			LightningInvoice: &wrpc.LightningInvoiceRequest{
				Invoice:     invoice,
				PaymentHash: hash,
			},
		},
	}
}

// onchainRequest builds a test onchain request.
func onchainRequest(address string) *wrpc.WalletEntryRequest {
	return &wrpc.WalletEntryRequest{
		Request: &wrpc.WalletEntryRequest_OnchainAddress{
			OnchainAddress: &wrpc.OnchainAddressRequest{
				Address: address,
			},
		},
	}
}

// activityResponse wraps test entries in the activity list response oneof.
func activityResponse(entries ...*wrpc.WalletEntry) *wrpc.ListResponse {
	return &wrpc.ListResponse{
		Body: &wrpc.ListResponse_Activity{
			Activity: &wrpc.ActivityList{
				Entries: entries,
			},
		},
	}
}

// phase builds a test progress value with the supplied display label.
func phase(label string) *wrpc.WalletEntryProgress {
	return &wrpc.WalletEntryProgress{
		Phase:      wrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING,
		PhaseLabel: label,
	}
}
