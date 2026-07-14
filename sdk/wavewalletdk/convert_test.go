package wavewalletdk

import (
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/stretchr/testify/require"
)

// TestEntryFromProto guards the wrapper-owned wallet activity DTO from
// accidental protobuf field drift, including the progress and request
// sub-messages.
func TestEntryFromProto(t *testing.T) {
	prog := &wavewalletrpc.WalletEntryProgress{
		PhaseLabel:         "settling",
		PaymentHash:        "phash",
		Txid:               "txid",
		ConfirmationHeight: 7,
		VtxoOutpoint:       "vtxo:0",
		Preimage:           "deadbeef",
	}
	prog.Phase = wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING

	invoiceReq := &wavewalletrpc.LightningInvoiceRequest{
		Invoice:     "lnbc1...",
		PaymentHash: "phash",
	}
	req := &wavewalletrpc.WalletEntryRequest{
		Request: &wavewalletrpc.WalletEntryRequest_LightningInvoice{
			LightningInvoice: invoiceReq,
		},
	}

	proto := &wavewalletrpc.WalletEntry{
		Id:            "id",
		Kind:          wavewalletrpc.EntryKind_ENTRY_KIND_SEND,
		Status:        wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		AmountSat:     -1000,
		FeeSat:        12,
		Counterparty:  "counterparty",
		CreatedAtUnix: 11,
		UpdatedAtUnix: 12,
		Note:          "note",
		FailureReason: "reason",
		Progress:      prog,
		Request:       req,
		FailureCode:   failureCodeEnum("TIMED_OUT").Enum(),
	}

	entry := entryFromProto(proto)
	require.Equal(t, "id", entry.ID)
	require.Equal(t, EntryKindSend, entry.Kind)
	require.Equal(t, EntryStatusComplete, entry.Status)
	require.EqualValues(t, -1000, entry.AmountSat)
	require.EqualValues(t, 12, entry.FeeSat)
	require.Equal(t, "counterparty", entry.Counterparty)
	require.Equal(t, time.Unix(11, 0), entry.CreatedAt)
	require.Equal(t, time.Unix(12, 0), entry.UpdatedAt)
	require.Equal(t, "note", entry.Note)
	require.Equal(t, "reason", entry.FailureReason)
	require.NotNil(t, entry.Progress)
	require.Equal(t, EntryPhaseSettling, entry.Progress.Phase)
	require.Equal(t, "settling", entry.Progress.PhaseLabel)
	require.Equal(t, "phash", entry.Progress.PaymentHash)
	require.Equal(t, "txid", entry.Progress.Txid)
	require.EqualValues(t, 7, entry.Progress.ConfirmationHeight)
	require.Equal(t, "vtxo:0", entry.Progress.VTXOOutpoint)
	require.Equal(t, "deadbeef", entry.Progress.Preimage)

	require.NotNil(t, entry.Request)
	require.Equal(t, EntryRequestTypeLightning, entry.Request.Type)
	require.Equal(t, "lnbc1...", entry.Request.LightningInvoice)
	require.Equal(t, "phash", entry.Request.PaymentHash)
	require.Equal(t, EntryFailureCodeTimedOut, entry.FailureCode)
}

// TestEntryFromProtoNoProgressOrRequest confirms a bare entry leaves the
// optional sub-objects nil rather than allocating empty shells, so callers can
// treat absence uniformly.
func TestEntryFromProtoNoProgressOrRequest(t *testing.T) {
	entry := entryFromProto(&wavewalletrpc.WalletEntry{Id: "id"})
	require.Equal(t, "id", entry.ID)
	require.Nil(t, entry.Progress)
	require.Nil(t, entry.Request)

	// A non-failed entry carries no failure code: the empty string, not a
	// sentinel, parallels the empty FailureReason.
	require.Empty(t, entry.FailureCode)
}

// TestEntryFailureCodeFromProto exhaustively maps every wavewalletrpc failure
// code to the wrapper-owned string, including the empty no-failure case.
func TestEntryFailureCodeFromProto(t *testing.T) {
	codes := wavewalletrpc.EntryFailureCode_value
	cases := []struct {
		in   wavewalletrpc.EntryFailureCode
		want EntryFailureCode
	}{{
		in:   failureCodeEnum("UNSPECIFIED"),
		want: "",
	}, {
		in:   failureCodeEnum("TIMED_OUT"),
		want: EntryFailureCodeTimedOut,
	}, {
		in:   failureCodeEnum("EXPIRED"),
		want: EntryFailureCodeExpired,
	}, {
		in:   failureCodeEnum("REFUNDED"),
		want: EntryFailureCodeRefunded,
	}, {
		in:   failureCodeEnum("NEEDS_INTERVENTION"),
		want: EntryFailureCodeNeedsIntervention,
	}, {
		in:   failureCodeEnum("FAILED"),
		want: EntryFailureCodeFailed,
	}}

	// Guard against a new proto code landing without a wrapper mapping.
	require.Len(t, cases, len(codes))

	for _, tc := range cases {
		require.Equal(
			t, tc.want, entryFailureCodeFromProto(tc.in),
			"in=%v", tc.in,
		)
	}
}

// TestEntryPhaseFromProto exhaustively maps every wavewalletrpc phase value to
// the wrapper-owned lowercase string, including the unspecified fallback.
func TestEntryPhaseFromProto(t *testing.T) {
	ph := wavewalletrpc.WalletEntryPhase_value
	cases := []struct {
		in   wavewalletrpc.WalletEntryPhase
		want EntryPhase
	}{
		{
			in:   phaseEnum(ph, "UNSPECIFIED"),
			want: EntryPhaseUnspecified,
		},
		{
			in:   phaseEnum(ph, "REQUEST_CREATED"),
			want: EntryPhaseRequestCreated,
		},
		{
			in:   phaseEnum(ph, "WAITING_FOR_PAYMENT"),
			want: EntryPhaseWaitingForPayment,
		},
		{
			in:   phaseEnum(ph, "PAYMENT_DETECTED"),
			want: EntryPhasePaymentDetected,
		},
		{
			in:   phaseEnum(ph, "SETTLING"),
			want: EntryPhaseSettling,
		},
		{
			in:   phaseEnum(ph, "CONFIRMED"),
			want: EntryPhaseConfirmed,
		},
		{
			in:   phaseEnum(ph, "REFUNDING"),
			want: EntryPhaseRefunding,
		},
		{
			in:   phaseEnum(ph, "REFUNDED"),
			want: EntryPhaseRefunded,
		},
		{
			in:   phaseEnum(ph, "FAILED"),
			want: EntryPhaseFailed,
		},
		{
			in:   phaseEnum(ph, "WAITING_FOR_CONFIRMATION"),
			want: EntryPhaseWaitingForConfirmation,
		},
	}

	// Guard against a new proto phase landing without a wrapper mapping.
	require.Len(t, cases, len(ph))

	for _, tc := range cases {
		require.Equal(
			t, tc.want, entryPhaseFromProto(tc.in),
			"in=%v", tc.in,
		)
	}
}

// phaseEnum resolves a short phase suffix to its generated enum value, keeping
// the table above within the line limit.
func phaseEnum(m map[string]int32,
	suffix string) wavewalletrpc.WalletEntryPhase {

	return wavewalletrpc.WalletEntryPhase(m["WALLET_ENTRY_PHASE_"+suffix])
}

// TestEntryRequestFromProto covers each oneof variant plus the nil and
// empty-oneof paths.
func TestEntryRequestFromProto(t *testing.T) {
	require.Nil(t, entryRequestFromProto(nil))
	require.Nil(
		t,
		entryRequestFromProto(
			&wavewalletrpc.WalletEntryRequest{},
		),
	)

	lnInvoiceReq := &wavewalletrpc.LightningInvoiceRequest{
		Invoice:     "lnbc1...",
		PaymentHash: "phash",
	}
	ln := entryRequestFromProto(&wavewalletrpc.WalletEntryRequest{
		Request: &wavewalletrpc.WalletEntryRequest_LightningInvoice{
			LightningInvoice: lnInvoiceReq,
		},
	})
	require.Equal(t, EntryRequestTypeLightning, ln.Type)
	require.Equal(t, "lnbc1...", ln.LightningInvoice)
	require.Equal(t, "phash", ln.PaymentHash)

	onchain := entryRequestFromProto(&wavewalletrpc.WalletEntryRequest{
		Request: &wavewalletrpc.WalletEntryRequest_OnchainAddress{
			OnchainAddress: &wavewalletrpc.OnchainAddressRequest{
				Address: "bc1qaddr",
			},
		},
	})
	require.Equal(t, EntryRequestTypeOnchain, onchain.Type)
	require.Equal(t, "bc1qaddr", onchain.OnchainAddress)

	ark := entryRequestFromProto(&wavewalletrpc.WalletEntryRequest{
		Request: &wavewalletrpc.WalletEntryRequest_ArkAddress{
			ArkAddress: &wavewalletrpc.ArkAddressRequest{
				Address: "ark1qaddr",
			},
		},
	})
	require.Equal(t, EntryRequestTypeArk, ark.Type)
	require.Equal(t, "ark1qaddr", ark.ArkAddress)

	// A wrapper set with a nil inner message still names the variant via
	// the type switch; the nil-safe getters then yield empty fields. gRPC
	// decode always allocates the inner message, so this only arises from a
	// hand-built struct, but pin the behavior so the type switch stays
	// honest.
	nilInner := entryRequestFromProto(&wavewalletrpc.WalletEntryRequest{
		Request: &wavewalletrpc.WalletEntryRequest_LightningInvoice{},
	})
	require.Equal(t, EntryRequestTypeLightning, nilInner.Type)
	require.Empty(t, nilInner.LightningInvoice)
	require.Empty(t, nilInner.PaymentHash)
}

// TestEntryRequestOneofArity fails fast if a future proto change grows the
// request oneof without a matching entryRequestFromProto branch, since a new
// variant would otherwise be silently dropped to nil.
func TestEntryRequestOneofArity(t *testing.T) {
	oneofs := (&wavewalletrpc.WalletEntryRequest{}).
		ProtoReflect().Descriptor().Oneofs()
	require.Equal(t, 1, oneofs.Len())
	require.Equal(t, 3, oneofs.Get(0).Fields().Len())
}

// failureCodeEnum resolves a short failure-code suffix to its generated enum
// value, keeping the tables above within the line limit.
func failureCodeEnum(suffix string) wavewalletrpc.EntryFailureCode {
	m := wavewalletrpc.EntryFailureCode_value

	return wavewalletrpc.EntryFailureCode(m["ENTRY_FAILURE_CODE_"+suffix])
}

// TestEntryKindToProto verifies filters use the expected wallet RPC enum.
func TestEntryKindToProto(t *testing.T) {
	got, err := entryKindToProto(EntryKindSend)
	require.NoError(t, err)
	require.Equal(
		t, wavewalletrpc.EntryKind_ENTRY_KIND_SEND,
		got,
	)
	got, err = entryKindToProto(EntryKindReceive)
	require.NoError(t, err)
	require.Equal(
		t, wavewalletrpc.EntryKind_ENTRY_KIND_RECV,
		got,
	)
	got, err = entryKindToProto(EntryKindDeposit)
	require.NoError(t, err)
	require.Equal(
		t, wavewalletrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		got,
	)
	got, err = entryKindToProto(EntryKindExit)
	require.NoError(t, err)
	require.Equal(
		t, wavewalletrpc.EntryKind_ENTRY_KIND_EXIT,
		got,
	)

	got, err = entryKindToProto("")
	require.Error(t, err)
	require.Equal(
		t, wavewalletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
		got,
	)
}

// TestEntryKindsToProtoRejectsUnknownKind confirms wrapper callers get a clear
// validation error instead of a silently empty activity list.
func TestEntryKindsToProtoRejectsUnknownKind(t *testing.T) {
	_, err := entryKindsToProto([]EntryKind{EntryKindSend, "junk"})
	require.ErrorContains(t, err, "unknown entry kind")
}

// TestListViewToProto covers every accepted view string plus the
// rejection path for unknown values.
func TestListViewToProto(t *testing.T) {
	cases := []struct {
		in   ListView
		want wavewalletrpc.ListView
		bad  bool
	}{
		{
			in:   ListViewActivity,
			want: wavewalletrpc.ListView_LIST_VIEW_ACTIVITY,
		},
		{
			in:   "",
			want: wavewalletrpc.ListView_LIST_VIEW_ACTIVITY,
		},
		{
			in:   ListViewVTXOs,
			want: wavewalletrpc.ListView_LIST_VIEW_VTXOS,
		},
		{
			in:   ListViewOnchain,
			want: wavewalletrpc.ListView_LIST_VIEW_ONCHAIN,
		},
		{
			in:   "junk",
			want: wavewalletrpc.ListView_LIST_VIEW_UNSPECIFIED,
			bad:  true,
		},
	}
	for _, tc := range cases {
		got, err := listViewToProto(tc.in)
		if tc.bad {
			require.Error(t, err, "in=%q", tc.in)

			continue
		}
		require.NoError(t, err, "in=%q", tc.in)
		require.Equal(t, tc.want, got, "in=%q", tc.in)
	}
}

// TestListResultFromProtoActivity confirms the activity variant is
// projected: WalletEntry rows convert to Entry DTOs and Total carries
// through.
func TestListResultFromProtoActivity(t *testing.T) {
	send := wavewalletrpc.EntryKind_ENTRY_KIND_SEND
	recv := wavewalletrpc.EntryKind_ENTRY_KIND_RECV
	resp := &wavewalletrpc.ListResponse{
		Body: &wavewalletrpc.ListResponse_Activity{
			Activity: &wavewalletrpc.ActivityList{
				Entries: []*wavewalletrpc.WalletEntry{
					{
						Id:   "hash1",
						Kind: send,
					},
					{
						Id:   "hash2",
						Kind: recv,
					},
				},
				Total:      42,
				HasMore:    true,
				NextCursor: "cursor-token",
			},
		},
	}
	out := listResultFromProto(ListViewActivity, resp)
	require.Equal(t, ListViewActivity, out.View)
	require.NotNil(t, out.Activity)
	require.Nil(t, out.VTXOs)
	require.Nil(t, out.Onchain)
	require.Equal(t, uint32(42), out.Activity.Total)
	require.True(t, out.Activity.HasMore)
	require.Equal(t, "cursor-token", out.Activity.NextCursor)
	require.Len(t, out.Activity.Entries, 2)
	require.Equal(t, "hash1", out.Activity.Entries[0].ID)
	require.Equal(t, EntryKindSend, out.Activity.Entries[0].Kind)
	require.Equal(t, "hash2", out.Activity.Entries[1].ID)
}

// TestListResultFromProtoActivityNilBody confirms a missing oneof
// body still produces a populated (empty) Activity variant rather
// than nil — callers always get a usable shape for the requested
// view.
func TestListResultFromProtoActivityNilBody(t *testing.T) {
	resp := &wavewalletrpc.ListResponse{}
	out := listResultFromProto(ListViewActivity, resp)
	require.Equal(t, ListViewActivity, out.View)
	require.NotNil(t, out.Activity)
	require.Empty(t, out.Activity.Entries)
	require.Zero(t, out.Activity.Total)
}

// TestListResultFromProtoVTXOs confirms the VTXOs variant projects
// WalletVTXO rows verbatim and the other variants remain nil.
func TestListResultFromProtoVTXOs(t *testing.T) {
	resp := &wavewalletrpc.ListResponse{
		Body: &wavewalletrpc.ListResponse_Vtxos{
			Vtxos: &wavewalletrpc.VTXOInventory{
				Vtxos: []*wavewalletrpc.WalletVTXO{
					{
						Outpoint:       "a:0",
						AmountSat:      1_000,
						Status:         "live",
						BatchExpiry:    99,
						RelativeExpiry: 144,
						CommitmentTxid: "dead",
					},
				},
				Total: 1,
			},
		},
	}
	out := listResultFromProto(ListViewVTXOs, resp)
	require.Equal(t, ListViewVTXOs, out.View)
	require.NotNil(t, out.VTXOs)
	require.Nil(t, out.Activity)
	require.Nil(t, out.Onchain)
	require.Equal(t, uint32(1), out.VTXOs.Total)
	require.Len(t, out.VTXOs.VTXOs, 1)
	require.Equal(t, "a:0", out.VTXOs.VTXOs[0].Outpoint)
	require.Equal(t, int64(1_000), out.VTXOs.VTXOs[0].AmountSat)
	require.Equal(t, "live", out.VTXOs.VTXOs[0].Status)
	require.Equal(t, int32(99), out.VTXOs.VTXOs[0].BatchExpiry)
	require.Equal(t, "dead", out.VTXOs.VTXOs[0].CommitmentTxid)
}

// TestListResultFromProtoOnchain confirms the Onchain variant
// projects OnchainTx rows and preserves the HasMore pagination flag.
func TestListResultFromProtoOnchain(t *testing.T) {
	resp := &wavewalletrpc.ListResponse{
		Body: &wavewalletrpc.ListResponse_Onchain{
			Onchain: &wavewalletrpc.OnchainHistory{
				Txs: []*wavewalletrpc.OnchainTx{
					{
						Txid:               "txid1",
						Kind:               "boarding",
						AmountSat:          5_000,
						FeeSat:             100,
						Status:             "confirmed",
						ConfirmationHeight: 1234,
						CreatedAtUnix:      500,
						Description:        "deposit",
					},
				},
				Total:   1,
				HasMore: true,
			},
		},
	}
	out := listResultFromProto(ListViewOnchain, resp)
	require.Equal(t, ListViewOnchain, out.View)
	require.NotNil(t, out.Onchain)
	require.True(t, out.Onchain.HasMore)
	require.Len(t, out.Onchain.Txs, 1)
	require.Equal(t, "txid1", out.Onchain.Txs[0].Txid)
	require.Equal(t, "boarding", out.Onchain.Txs[0].Kind)
	require.Equal(t, int64(5_000), out.Onchain.Txs[0].AmountSat)
	require.Equal(t, time.Unix(500, 0), out.Onchain.Txs[0].CreatedAt)
}

// TestExitJobStatusFromProto exhaustively maps every wavewalletrpc enum
// value to the wrapper-owned lowercase string.
func TestExitJobStatusFromProto(t *testing.T) {
	cases := []struct {
		in   wavewalletrpc.ExitJobStatus
		want ExitJobStatus
	}{
		{
			wavewalletrpc.ExitJobStatus_EXIT_JOB_STATUS_PENDING,
			ExitJobStatusPending,
		},
		{
			wavewalletrpc.
				ExitJobStatus_EXIT_JOB_STATUS_MATERIALIZING,
			ExitJobStatusMaterializing,
		},
		{
			wavewalletrpc.ExitJobStatus_EXIT_JOB_STATUS_CSV_PENDING,
			ExitJobStatusCSVPending,
		},
		{
			wavewalletrpc.ExitJobStatus_EXIT_JOB_STATUS_SWEEPING,
			ExitJobStatusSweeping,
		},
		{
			wavewalletrpc.ExitJobStatus_EXIT_JOB_STATUS_COMPLETED,
			ExitJobStatusCompleted,
		},
		{
			wavewalletrpc.ExitJobStatus_EXIT_JOB_STATUS_FAILED,
			ExitJobStatusFailed,
		},
		{
			wavewalletrpc.ExitJobStatus_EXIT_JOB_STATUS_UNSPECIFIED,
			ExitJobStatusUnspecified,
		},
	}
	for _, tc := range cases {
		require.Equal(
			t, tc.want, exitJobStatusFromProto(tc.in),
			"in=%v", tc.in,
		)
	}
}
