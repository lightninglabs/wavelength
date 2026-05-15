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

// TestSwapStateFromProtoUnknownFallback verifies that unknown future enum
// values fall back to a lowercased proto name rather than being flattened
// into "unspecified". The latter is reserved for the explicit UNSPECIFIED
// enum value so host UIs can distinguish the two.
func TestSwapStateFromProtoUnknownFallback(t *testing.T) {
	require.Equal(
		t, "unspecified", swapStateFromProto(
			swapclientrpc.SwapState_SWAP_STATE_UNSPECIFIED,
		),
	)

	// A value that is not listed in the proto generates a String() of the
	// form "SwapState(<n>)" — the trim leaves the parenthesized form,
	// which is fine: it is a stable, non-empty signal that something new
	// is happening rather than a silent erase.
	unknown := swapStateFromProto(swapclientrpc.SwapState(9999))
	require.NotEqual(t, "unspecified", unknown)
	require.NotEmpty(t, unknown)
}

// TestSwapDirectionFromProtoUnknownFallback mirrors the state-unknown test
// for direction. Unspecified maps to the empty string; future values must
// produce a non-empty fallback.
func TestSwapDirectionFromProtoUnknownFallback(t *testing.T) {
	require.Equal(
		t, SwapDirection(""), swapDirectionFromProto(
			swapclientrpc.SwapDirection_SWAP_DIRECTION_UNSPECIFIED,
		),
	)

	unknown := swapDirectionFromProto(swapclientrpc.SwapDirection(9999))
	require.NotEmpty(t, unknown)
}
