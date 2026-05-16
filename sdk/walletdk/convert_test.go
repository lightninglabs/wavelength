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
