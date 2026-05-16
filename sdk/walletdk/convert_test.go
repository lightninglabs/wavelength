package walletdk

import (
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"github.com/stretchr/testify/require"
)

// TestEntryFromProto guards the wrapper-owned wallet activity DTO from
// accidental protobuf field drift.
func TestEntryFromProto(t *testing.T) {
	proto := &walletrpc.WalletEntry{
		Id:            "id",
		Kind:          walletrpc.EntryKind_ENTRY_KIND_SEND,
		Status:        walletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		AmountSat:     -1000,
		FeeSat:        12,
		Counterparty:  "counterparty",
		CreatedAtUnix: 11,
		UpdatedAtUnix: 12,
		Note:          "note",
		FailureReason: "reason",
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
}

// TestEntryKindToProto verifies filters use the expected wallet RPC enum.
func TestEntryKindToProto(t *testing.T) {
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_SEND,
		entryKindToProto(EntryKindSend),
	)
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_RECV,
		entryKindToProto(EntryKindReceive),
	)
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		entryKindToProto(EntryKindDeposit),
	)
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_EXIT,
		entryKindToProto(EntryKindExit),
	)
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
		entryKindToProto(""),
	)
}

// TestListViewToProto covers every accepted view string plus the
// rejection path for unknown values.
func TestListViewToProto(t *testing.T) {
	cases := []struct {
		in   ListView
		want walletrpc.ListView
		bad  bool
	}{
		{
			in:   ListViewActivity,
			want: walletrpc.ListView_LIST_VIEW_ACTIVITY,
		},
		{
			in:   "",
			want: walletrpc.ListView_LIST_VIEW_ACTIVITY,
		},
		{
			in:   ListViewVTXOs,
			want: walletrpc.ListView_LIST_VIEW_VTXOS,
		},
		{
			in:   ListViewOnchain,
			want: walletrpc.ListView_LIST_VIEW_ONCHAIN,
		},
		{
			in:   "junk",
			want: walletrpc.ListView_LIST_VIEW_UNSPECIFIED,
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
	send := walletrpc.EntryKind_ENTRY_KIND_SEND
	recv := walletrpc.EntryKind_ENTRY_KIND_RECV
	resp := &walletrpc.ListResponse{
		Body: &walletrpc.ListResponse_Activity{
			Activity: &walletrpc.ActivityList{
				Entries: []*walletrpc.WalletEntry{
					{Id: "hash1", Kind: send},
					{Id: "hash2", Kind: recv},
				},
				Total: 42,
			},
		},
	}
	out := listResultFromProto(ListViewActivity, resp)
	require.Equal(t, ListViewActivity, out.View)
	require.NotNil(t, out.Activity)
	require.Nil(t, out.VTXOs)
	require.Nil(t, out.Onchain)
	require.Equal(t, uint32(42), out.Activity.Total)
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
	resp := &walletrpc.ListResponse{}
	out := listResultFromProto(ListViewActivity, resp)
	require.Equal(t, ListViewActivity, out.View)
	require.NotNil(t, out.Activity)
	require.Empty(t, out.Activity.Entries)
	require.Zero(t, out.Activity.Total)
}

// TestListResultFromProtoVTXOs confirms the VTXOs variant projects
// WalletVTXO rows verbatim and the other variants remain nil.
func TestListResultFromProtoVTXOs(t *testing.T) {
	resp := &walletrpc.ListResponse{
		Body: &walletrpc.ListResponse_Vtxos{
			Vtxos: &walletrpc.VTXOInventory{
				Vtxos: []*walletrpc.WalletVTXO{
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
	resp := &walletrpc.ListResponse{
		Body: &walletrpc.ListResponse_Onchain{
			Onchain: &walletrpc.OnchainHistory{
				Txs: []*walletrpc.OnchainTx{
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

// TestExitJobStatusFromProto exhaustively maps every walletrpc enum
// value to the wrapper-owned lowercase string.
func TestExitJobStatusFromProto(t *testing.T) {
	cases := []struct {
		in   walletrpc.ExitJobStatus
		want ExitJobStatus
	}{
		{
			walletrpc.ExitJobStatus_EXIT_JOB_STATUS_PENDING,
			ExitJobStatusPending,
		},
		{
			walletrpc.ExitJobStatus_EXIT_JOB_STATUS_MATERIALIZING,
			ExitJobStatusMaterializing,
		},
		{
			walletrpc.ExitJobStatus_EXIT_JOB_STATUS_CSV_PENDING,
			ExitJobStatusCSVPending,
		},
		{
			walletrpc.ExitJobStatus_EXIT_JOB_STATUS_SWEEPING,
			ExitJobStatusSweeping,
		},
		{
			walletrpc.ExitJobStatus_EXIT_JOB_STATUS_COMPLETED,
			ExitJobStatusCompleted,
		},
		{
			walletrpc.ExitJobStatus_EXIT_JOB_STATUS_FAILED,
			ExitJobStatusFailed,
		},
		{
			walletrpc.ExitJobStatus_EXIT_JOB_STATUS_UNSPECIFIED,
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
