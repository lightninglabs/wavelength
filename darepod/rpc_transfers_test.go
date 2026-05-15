package darepod

import (
	"context"
	"testing"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestListTransfersRequiresWalletReady verifies the transfer-list RPC returns
// the shared wallet readiness error before scanning status/history sources.
func TestListTransfersRequiresWalletReady(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()
	_, err := r.ListTransfers(
		context.Background(), &daemonrpc.ListTransfersRequest{},
	)

	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.ErrorContains(t, err, "wallet is not ready")
}

// TestTransferDirectionFromTransaction verifies ledger-backed transfer rows
// are classified from the local wallet's perspective.
func TestTransferDirectionFromTransaction(t *testing.T) {
	t.Parallel()

	unspecified := daemonrpc.
		TransferDirection_TRANSFER_DIRECTION_UNSPECIFIED

	tests := []struct {
		name     string
		tx       *daemonrpc.TransactionHistoryEntry
		expected daemonrpc.TransferDirection
	}{
		{
			name: "received subtype",
			tx: &daemonrpc.TransactionHistoryEntry{
				Subtype: ledger.EventVTXOReceived,
			},
			expected: daemonrpc.
				TransferDirection_TRANSFER_DIRECTION_INCOMING,
		},
		{
			name: "sent subtype",
			tx: &daemonrpc.TransactionHistoryEntry{
				Subtype: ledger.EventVTXOSent,
			},
			expected: daemonrpc.
				TransferDirection_TRANSFER_DIRECTION_OUTGOING,
		},
		{
			name: "transfers in account",
			tx: &daemonrpc.TransactionHistoryEntry{
				CreditAccount: ledger.AccountTransfersIn,
			},
			expected: daemonrpc.
				TransferDirection_TRANSFER_DIRECTION_INCOMING,
		},
		{
			name: "transfers out account",
			tx: &daemonrpc.TransactionHistoryEntry{
				DebitAccount: ledger.AccountTransfersOut,
			},
			expected: daemonrpc.
				TransferDirection_TRANSFER_DIRECTION_OUTGOING,
		},
		{
			name: "subtype wins over account",
			tx: &daemonrpc.TransactionHistoryEntry{
				Subtype:      ledger.EventVTXOReceived,
				DebitAccount: ledger.AccountTransfersOut,
			},
			expected: daemonrpc.
				TransferDirection_TRANSFER_DIRECTION_INCOMING,
		},
		{
			name:     "unknown",
			tx:       &daemonrpc.TransactionHistoryEntry{},
			expected: unspecified,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, test.expected,
				transferDirectionFromTransaction(test.tx),
			)
		})
	}
}

// TestTransferStatusFromTransaction verifies persisted transaction history rows
// map to the public coarse transfer status enum.
func TestTransferStatusFromTransaction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		tx       *daemonrpc.TransactionHistoryEntry
		expected daemonrpc.TransferStatus
	}{
		{
			name: "recorded row",
			tx: &daemonrpc.TransactionHistoryEntry{
				ConfirmationStatus: "recorded",
			},
			expected: daemonrpc.
				TransferStatus_TRANSFER_STATUS_COMPLETED,
		},
		{
			name: "failed status",
			tx: &daemonrpc.TransactionHistoryEntry{
				ConfirmationStatus: "failed",
			},
			expected: daemonrpc.
				TransferStatus_TRANSFER_STATUS_FAILED,
		},
		{
			name: "failed word in subtype",
			tx: &daemonrpc.TransactionHistoryEntry{
				Subtype: "pre_failed_state_reset",
			},
			expected: daemonrpc.
				TransferStatus_TRANSFER_STATUS_COMPLETED,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, test.expected,
				transferStatusFromTransaction(test.tx),
			)
		})
	}
}

// TestTransferDirectionFromRound verifies pending round rows use local
// outpoint ownership hints when the round summary exposes them.
func TestTransferDirectionFromRound(t *testing.T) {
	t.Parallel()

	require.Equal(
		t, daemonrpc.TransferDirection_TRANSFER_DIRECTION_OUTGOING,
		transferDirectionFromRound(
			&daemonrpc.RoundInfo{
				InputOutpoints: []string{"aaa:0"},
			},
		),
	)
	require.Equal(
		t, daemonrpc.TransferDirection_TRANSFER_DIRECTION_INCOMING,
		transferDirectionFromRound(
			&daemonrpc.RoundInfo{
				OutputOutpoints: []string{"bbb:1"},
			},
		),
	)
	require.Equal(
		t, daemonrpc.TransferDirection_TRANSFER_DIRECTION_UNSPECIFIED,
		transferDirectionFromRound(
			&daemonrpc.RoundInfo{},
		),
	)
}

// TestTransferMatchesFilters verifies mode, direction, and status filters are
// all enforced by the shared transfer list predicate.
func TestTransferMatchesFilters(t *testing.T) {
	t.Parallel()

	incoming := daemonrpc.TransferDirection_TRANSFER_DIRECTION_INCOMING
	outgoing := daemonrpc.TransferDirection_TRANSFER_DIRECTION_OUTGOING
	unknownDirection := daemonrpc.
		TransferDirection_TRANSFER_DIRECTION_UNSPECIFIED
	completed := daemonrpc.TransferStatus_TRANSFER_STATUS_COMPLETED
	failed := daemonrpc.TransferStatus_TRANSFER_STATUS_FAILED
	inround := daemonrpc.TransferMode_TRANSFER_MODE_INROUND
	oor := daemonrpc.TransferMode_TRANSFER_MODE_OOR

	transfer := &daemonrpc.TransferInfo{
		Mode:      inround,
		Direction: incoming,
		Status:    completed,
	}

	require.True(
		t,
		transferMatchesFilters(
			transfer, &daemonrpc.ListTransfersRequest{},
		),
	)
	require.True(
		t,
		transferMatchesFilters(
			transfer, &daemonrpc.ListTransfersRequest{
				ModeFilter:      inround,
				DirectionFilter: incoming,
				StatusFilter:    completed,
			},
		),
	)
	require.False(
		t,
		transferMatchesFilters(
			transfer, &daemonrpc.ListTransfersRequest{
				DirectionFilter: outgoing,
			},
		),
	)
	require.False(
		t,
		transferMatchesFilters(
			nil, &daemonrpc.ListTransfersRequest{},
		),
	)
	require.False(
		t,
		transferMatchesFilters(
			transfer, &daemonrpc.ListTransfersRequest{
				ModeFilter: oor,
			},
		),
	)
	require.False(
		t,
		transferMatchesFilters(
			transfer, &daemonrpc.ListTransfersRequest{
				StatusFilter: failed,
			},
		),
	)
	require.True(
		t,
		transferMatchesFilters(
			&daemonrpc.TransferInfo{
				Mode:      inround,
				Direction: unknownDirection,
				Status:    completed,
			}, &daemonrpc.ListTransfersRequest{
				DirectionFilter: outgoing,
			},
		),
	)
}

// TestTransferStatusFromRoundState verifies round FSM states map onto the
// coarse transfer status enum.
func TestTransferStatusFromRoundState(t *testing.T) {
	t.Parallel()

	require.Equal(
		t, daemonrpc.TransferStatus_TRANSFER_STATUS_COMPLETED,
		transferStatusFromRoundState(
			daemonrpc.RoundState_ROUND_STATE_CONFIRMED,
		),
	)
	require.Equal(
		t, daemonrpc.TransferStatus_TRANSFER_STATUS_FAILED,
		transferStatusFromRoundState(
			daemonrpc.RoundState_ROUND_STATE_FAILED,
		),
	)
	require.Equal(
		t, daemonrpc.TransferStatus_TRANSFER_STATUS_PENDING,
		transferStatusFromRoundState(
			daemonrpc.RoundState_ROUND_STATE_UNKNOWN,
		),
	)
}

// TestDedupeTransfers verifies terminal history rows replace pending
// round-status rows for the same persisted round id.
func TestDedupeTransfers(t *testing.T) {
	t.Parallel()

	inround := daemonrpc.TransferMode_TRANSFER_MODE_INROUND
	pending := daemonrpc.TransferStatus_TRANSFER_STATUS_PENDING
	completed := daemonrpc.TransferStatus_TRANSFER_STATUS_COMPLETED

	transfers := []*daemonrpc.TransferInfo{
		{
			TransferId:     "round:abc",
			Mode:           inround,
			Status:         pending,
			RoundId:        "abc",
			UpdatedAtUnixS: 20,
		},
		{
			TransferId:     "round:abc:7",
			Mode:           inround,
			Status:         completed,
			RoundId:        "abc",
			UpdatedAtUnixS: 10,
		},
		{
			TransferId:     "round:abc:10",
			Mode:           inround,
			Status:         completed,
			RoundId:        "abc",
			UpdatedAtUnixS: 10,
		},
		{
			TransferId: "round:",
			Mode:       inround,
			Status:     pending,
		},
	}

	deduped := dedupeTransfers(transfers)

	require.Len(t, deduped, 2)
	require.Equal(t, "round:abc:10", deduped[0].GetTransferId())
	require.Equal(t, "round:", deduped[1].GetTransferId())
}

// TestTransferHistoryStatusAllowed verifies pending-only requests can skip
// committed round history scans.
func TestTransferHistoryStatusAllowed(t *testing.T) {
	t.Parallel()

	completed := daemonrpc.TransferStatus_TRANSFER_STATUS_COMPLETED
	failed := daemonrpc.TransferStatus_TRANSFER_STATUS_FAILED
	pending := daemonrpc.TransferStatus_TRANSFER_STATUS_PENDING

	require.True(
		t,
		transferHistoryStatusAllowed(
			&daemonrpc.ListTransfersRequest{},
		),
	)
	require.True(
		t,
		transferHistoryStatusAllowed(
			&daemonrpc.ListTransfersRequest{
				StatusFilter: completed,
			},
		),
	)
	require.True(
		t,
		transferHistoryStatusAllowed(
			&daemonrpc.ListTransfersRequest{
				StatusFilter: failed,
			},
		),
	)
	require.False(
		t,
		transferHistoryStatusAllowed(
			&daemonrpc.ListTransfersRequest{
				StatusFilter: pending,
			},
		),
	)
}

// TestTransferSupersedesRoundDuplicate verifies duplicate round rows prefer
// terminal history and then the newest numeric history entry id.
func TestTransferSupersedesRoundDuplicate(t *testing.T) {
	t.Parallel()

	inround := daemonrpc.TransferMode_TRANSFER_MODE_INROUND
	pending := daemonrpc.TransferStatus_TRANSFER_STATUS_PENDING
	completed := daemonrpc.TransferStatus_TRANSFER_STATUS_COMPLETED

	pendingRound := &daemonrpc.TransferInfo{
		TransferId:     "round:abc",
		Mode:           inround,
		Status:         pending,
		RoundId:        "abc",
		UpdatedAtUnixS: 20,
	}
	completedRound := &daemonrpc.TransferInfo{
		TransferId:     "round:abc:7",
		Mode:           inround,
		Status:         completed,
		RoundId:        "abc",
		UpdatedAtUnixS: 10,
	}
	newerHistoryEntry := &daemonrpc.TransferInfo{
		TransferId:     "round:abc:10",
		Mode:           inround,
		Status:         completed,
		RoundId:        "abc",
		UpdatedAtUnixS: 10,
	}

	require.True(
		t, transferSupersedesRoundDuplicate(
			completedRound, pendingRound,
		),
	)
	require.False(
		t, transferSupersedesRoundDuplicate(
			pendingRound, completedRound,
		),
	)
	require.True(
		t, transferSupersedesRoundDuplicate(
			newerHistoryEntry, completedRound,
		),
	)
}

// TestRoundHistoryEntryID verifies only round history ids with numeric ledger
// suffixes are parsed as dedupe tie-breakers.
func TestRoundHistoryEntryID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		id       string
		expected int64
		ok       bool
	}{
		{
			name:     "round history",
			id:       "round:abc:7",
			expected: 7,
			ok:       true,
		},
		{
			name: "oor history",
			id:   "oor:sess:7",
		},
		{
			name: "ledger fallback",
			id:   "ledger:7",
		},
		{
			name: "malformed",
			id:   "round:abc:not-a-number",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			entryID, ok := roundHistoryEntryID(
				&daemonrpc.TransferInfo{
					TransferId: test.id,
				},
			)
			require.Equal(t, test.ok, ok)
			require.Equal(t, test.expected, entryID)
		})
	}
}

// TestClampedTransferStart verifies transfer pagination cannot overflow a
// platform int while converting from the RPC cursor type.
func TestClampedTransferStart(t *testing.T) {
	t.Parallel()

	require.Equal(t, 3, clampedTransferStart(3, 10))
	require.Equal(t, 10, clampedTransferStart(10, 10))
	require.Equal(t, 10, clampedTransferStart(11, 10))
	require.Equal(t, 10, clampedTransferStart(^uint32(0), 10))
}

// TestNextTransferOffset verifies transfer pagination cursors advance without
// wrapping on uint32 overflow.
func TestNextTransferOffset(t *testing.T) {
	t.Parallel()

	require.Equal(t, uint32(12), nextTransferOffset(10, 2))
	require.Equal(t, ^uint32(0), nextTransferOffset(^uint32(0)-1, 2))
}

// TestSortTransfersNewestFirst verifies transfer rows sort by best known
// update time and then by transfer id.
func TestSortTransfersNewestFirst(t *testing.T) {
	t.Parallel()

	transfers := []*daemonrpc.TransferInfo{
		{
			TransferId:     "b",
			CreatedAtUnixS: 3,
		},
		{
			TransferId:     "d",
			CreatedAtUnixS: 7,
		},
		{
			TransferId:     "e",
			UpdatedAtUnixS: 6,
		},
		{
			TransferId:     "a",
			UpdatedAtUnixS: 5,
		},
		{
			TransferId:     "c",
			UpdatedAtUnixS: 5,
		},
	}

	sortTransfersNewestFirst(transfers)

	require.Equal(t, "d", transfers[0].GetTransferId())
	require.Equal(t, "e", transfers[1].GetTransferId())
	require.Equal(t, "c", transfers[2].GetTransferId())
	require.Equal(t, "a", transfers[3].GetTransferId())
	require.Equal(t, "b", transfers[4].GetTransferId())
}

// TestTemporaryRoundTransferID verifies live rounds without server ids still
// receive distinct local transfer ids.
func TestTemporaryRoundTransferID(t *testing.T) {
	t.Parallel()

	round := &daemonrpc.RoundInfo{
		IsTemp:         true,
		State:          daemonrpc.RoundState_ROUND_STATE_JOINED,
		CreationTime:   10,
		LastUpdateTime: 12,
		CommitmentTxid: "commitment",
		InputOutpoints: []string{
			"aaa:0",
		},
		OutputOutpoints: []string{
			"bbb:1",
		},
	}

	first := temporaryRoundTransferID(round, 0)
	second := temporaryRoundTransferID(round, 1)

	require.NotEmpty(t, first)
	require.Contains(t, first, "is_temp:true")
	require.Less(t, len(first), 64)
	require.NotEqual(t, first, second)
}
