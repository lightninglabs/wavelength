package ledgeractor

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// disabledLogger returns a no-op btclog logger.
func disabledLogger() btclog.Logger {
	return btclog.Disabled
}

// mockLedgerStore records all InsertLedgerEntry calls for
// assertion.
type mockLedgerStore struct {
	mu      sync.Mutex
	entries []LedgerEntry
}

func (m *mockLedgerStore) InsertLedgerEntry(
	_ context.Context, entry LedgerEntry) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.entries = append(m.entries, entry)

	return nil
}

func (m *mockLedgerStore) getEntries() []LedgerEntry {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]LedgerEntry{}, m.entries...)
}

// newTestActor creates a LedgerActor with a mock store for
// testing handlers directly.
func newTestActor(t *testing.T) (*LedgerActor, *mockLedgerStore) {
	t.Helper()

	store := &mockLedgerStore{}

	actor := &LedgerActor{
		cfg: ActorConfig{
			LedgerStore: store,
		},
		log: disabledLogger(),
	}

	return actor, store
}

// TestHandleFeePaidBoarding verifies that a boarding fee is
// recorded with the correct accounts and event type.
func TestHandleFeePaidBoarding(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &FeePaidMsg{
		RoundID:     [16]byte{1, 2, 3},
		AmountSat:   1500,
		FeeType:     "boarding",
		BlockHeight: 800_000,
	}

	err := a.handleFeePaid(ctx, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountFeesPaid, entries[0].DebitAccount)
	require.Equal(t, AccountVTXOBalance,
		entries[0].CreditAccount)
	require.Equal(t, int64(1500), entries[0].AmountSat)
	require.Equal(t, EventBoardingFeePaid,
		entries[0].EventType)
}

// TestHandleFeePaidRefresh verifies that a refresh fee is
// recorded with the correct event type.
func TestHandleFeePaidRefresh(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &FeePaidMsg{
		RoundID:     [16]byte{4, 5, 6},
		AmountSat:   750,
		FeeType:     "refresh",
		BlockHeight: 800_100,
	}

	err := a.handleFeePaid(ctx, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, EventRefreshFeePaid,
		entries[0].EventType)
}

// TestHandleVTXOReceivedRound verifies that a round-sourced VTXO
// received is recorded with wallet_balance -> vtxo_balance.
func TestHandleVTXOReceivedRound(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &VTXOReceivedMsg{
		OutpointHash:  [32]byte{0xaa, 0xbb},
		OutpointIndex: 0,
		AmountSat:     50_000,
		Source:         "round",
		RoundID:       [16]byte{7, 8, 9},
	}

	err := a.handleVTXOReceived(ctx, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountVTXOBalance,
		entries[0].DebitAccount)
	require.Equal(t, AccountWalletBalance,
		entries[0].CreditAccount)
	require.Equal(t, int64(50_000), entries[0].AmountSat)
	require.Equal(t, EventVTXOReceived,
		entries[0].EventType)
}

// TestHandleVTXOReceivedOOR verifies that an OOR-sourced VTXO
// received is recorded with vtxo_balance -> transfer_income.
func TestHandleVTXOReceivedOOR(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &VTXOReceivedMsg{
		OutpointHash:  [32]byte{0xcc, 0xdd},
		OutpointIndex: 1,
		AmountSat:     25_000,
		Source:         "oor",
		RoundID:       [16]byte{10, 11, 12},
	}

	err := a.handleVTXOReceived(ctx, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountVTXOBalance,
		entries[0].DebitAccount)
	require.Equal(t, AccountTransferIncome,
		entries[0].CreditAccount)
}

// TestHandleVTXOSent verifies that sending VTXOs via OOR is
// recorded with transfer_income -> vtxo_balance.
func TestHandleVTXOSent(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &VTXOSentMsg{
		SessionID: [32]byte{0x01},
		AmountSat: 10_000,
	}

	err := a.handleVTXOSent(ctx, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountTransferIncome,
		entries[0].DebitAccount)
	require.Equal(t, AccountVTXOBalance,
		entries[0].CreditAccount)
	require.Equal(t, int64(10_000), entries[0].AmountSat)
	require.Equal(t, EventVTXOSent,
		entries[0].EventType)
}

// TestHandleExitCost verifies that exit costs are recorded
// with onchain_fees -> vtxo_balance using ExitCostSat.
func TestHandleExitCost(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &ExitCostMsg{
		OutpointHash:  [32]byte{0xee, 0xff},
		OutpointIndex: 2,
		AmountSat:     100_000,
		ExitCostSat:   5_000,
		BlockHeight:   800_500,
	}

	err := a.handleExitCost(ctx, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountOnchainFees,
		entries[0].DebitAccount)
	require.Equal(t, AccountVTXOBalance,
		entries[0].CreditAccount)

	// The recorded amount should be ExitCostSat, not
	// AmountSat.
	require.Equal(t, int64(5_000), entries[0].AmountSat)
	require.Equal(t, EventOnchainFeePaid,
		entries[0].EventType)
}

// TestHandleFeePaidUnknownType verifies that an unknown fee type
// returns an error instead of silently misclassifying the entry.
func TestHandleFeePaidUnknownType(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &FeePaidMsg{
		RoundID:     [16]byte{1, 2, 3},
		AmountSat:   1500,
		FeeType:     "unknown_type",
		BlockHeight: 800_000,
	}

	err := a.handleFeePaid(ctx, msg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown fee type")

	// No entry should have been written.
	require.Empty(t, store.getEntries())
}

// TestHandleVTXOReceivedUnknownSource verifies that an unknown
// VTXO source returns an error instead of defaulting to round.
func TestHandleVTXOReceivedUnknownSource(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &VTXOReceivedMsg{
		OutpointHash:  [32]byte{0xaa, 0xbb},
		OutpointIndex: 0,
		AmountSat:     50_000,
		Source:        "collaborative",
		RoundID:       [16]byte{7, 8, 9},
	}

	err := a.handleVTXOReceived(ctx, msg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown vtxo source")

	// No entry should have been written.
	require.Empty(t, store.getEntries())
}

// TestMessageTLVRoundTrip verifies that all client message types
// can be encoded and decoded without data loss.
func TestMessageTLVRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  LedgerMsg
		new  func() LedgerMsg
	}{
		{
			name: "FeePaid",
			msg: &FeePaidMsg{
				RoundID:     [16]byte{1, 2, 3},
				AmountSat:   999,
				FeeType:     "boarding",
				BlockHeight: 800_000,
			},
			new: func() LedgerMsg {
				return &FeePaidMsg{}
			},
		},
		{
			name: "VTXOReceived",
			msg: &VTXOReceivedMsg{
				OutpointHash:  [32]byte{0xaa},
				OutpointIndex: 42,
				AmountSat:     50_000,
				Source:        "oor",
				RoundID:       [16]byte{4, 5, 6},
			},
			new: func() LedgerMsg {
				return &VTXOReceivedMsg{}
			},
		},
		{
			name: "VTXOSent",
			msg: &VTXOSentMsg{
				SessionID: [32]byte{0xbb},
				AmountSat: 10_000,
			},
			new: func() LedgerMsg {
				return &VTXOSentMsg{}
			},
		},
		{
			name: "ExitCost",
			msg: &ExitCostMsg{
				OutpointHash:  [32]byte{0xcc},
				OutpointIndex: 1,
				AmountSat:     100_000,
				ExitCostSat:   5_000,
				BlockHeight:   800_500,
			},
			new: func() LedgerMsg {
				return &ExitCostMsg{}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Encode.
			var buf []byte
			w := &bytesWriter{buf: &buf}
			err := tc.msg.Encode(w)
			require.NoError(t, err)

			// Decode.
			decoded := tc.new()
			r := &bytesReader{buf: buf}
			err = decoded.Decode(r)
			require.NoError(t, err)

			// Verify TLV type and field content
			// match after round-trip.
			require.Equal(t,
				tc.msg.TLVType(),
				decoded.TLVType(),
			)
			require.Equal(t, tc.msg, decoded)
		})
	}
}

// bytesWriter is a simple io.Writer backed by a byte slice.
type bytesWriter struct {
	buf *[]byte
}

func (w *bytesWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)

	return len(p), nil
}

// bytesReader is a simple io.Reader backed by a byte slice.
type bytesReader struct {
	buf []byte
	off int
}

func (r *bytesReader) Read(p []byte) (int, error) {
	if r.off >= len(r.buf) {
		return 0, io.EOF
	}

	n := copy(p, r.buf[r.off:])
	r.off += n

	return n, nil
}
