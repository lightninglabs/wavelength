package ledger

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

// mockUTXOAuditStore records all InsertUTXOAuditEntry calls for
// assertion.
type mockUTXOAuditStore struct {
	mu      sync.Mutex
	entries []UTXOAuditEntry
}

func (m *mockUTXOAuditStore) InsertUTXOAuditEntry(
	_ context.Context, entry UTXOAuditEntry) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.entries = append(m.entries, entry)

	return nil
}

func (m *mockUTXOAuditStore) getEntries() []UTXOAuditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]UTXOAuditEntry{}, m.entries...)
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

// newTestActorWithAudit creates a LedgerActor with both a mock
// ledger store and a mock UTXO audit store.
func newTestActorWithAudit(
	t *testing.T) (*LedgerActor, *mockUTXOAuditStore) {

	t.Helper()

	auditStore := &mockUTXOAuditStore{}

	actor := &LedgerActor{
		cfg: ActorConfig{
			LedgerStore:    &mockLedgerStore{},
			UTXOAuditStore: auditStore,
		},
		log: disabledLogger(),
	}

	return actor, auditStore
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

// TestHandleVTXOReceivedRoundBoarding verifies that a boarding
// or refresh receive is recorded with wallet_balance ->
// vtxo_balance (own on-chain funds converted to VTXO balance).
func TestHandleVTXOReceivedRoundBoarding(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &VTXOReceivedMsg{
		OutpointHash:  [32]byte{0xaa, 0xbb},
		OutpointIndex: 0,
		AmountSat:     50_000,
		Source:        SourceRoundBoarding,
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

// TestHandleVTXOReceivedRoundTransfer verifies that an in-round
// participant-to-participant VTXO receive is recorded the same
// way as an OOR receive: transfers_in -> vtxo_balance.
func TestHandleVTXOReceivedRoundTransfer(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &VTXOReceivedMsg{
		OutpointHash:  [32]byte{0xab, 0xcd},
		OutpointIndex: 0,
		AmountSat:     30_000,
		Source:        SourceRoundTransfer,
		RoundID:       [16]byte{13, 14, 15},
	}

	err := a.handleVTXOReceived(ctx, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountVTXOBalance,
		entries[0].DebitAccount)
	require.Equal(t, AccountTransfersIn,
		entries[0].CreditAccount)
}

// TestHandleVTXOReceivedOOR verifies that an OOR-sourced VTXO
// received is recorded with vtxo_balance -> transfers_in.
func TestHandleVTXOReceivedOOR(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &VTXOReceivedMsg{
		OutpointHash:  [32]byte{0xcc, 0xdd},
		OutpointIndex: 1,
		AmountSat:     25_000,
		Source:        SourceOOR,
		RoundID:       [16]byte{10, 11, 12},
	}

	err := a.handleVTXOReceived(ctx, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountVTXOBalance,
		entries[0].DebitAccount)
	require.Equal(t, AccountTransfersIn,
		entries[0].CreditAccount)
}

// TestHandleVTXOSent verifies that sending VTXOs via OOR is
// recorded as an expense on transfers_out crediting vtxo_balance.
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
	require.Equal(t, AccountTransfersOut,
		entries[0].DebitAccount)
	require.Equal(t, AccountVTXOBalance,
		entries[0].CreditAccount)
	require.Equal(t, int64(10_000), entries[0].AmountSat)
	require.Equal(t, EventVTXOSent,
		entries[0].EventType)
}

// TestHandleVTXOSendReceiveAreGross verifies that a matched
// receive and send of the same amount do not net to zero on a
// single account: transfers_in accumulates credits and
// transfers_out accumulates debits independently.
func TestHandleVTXOSendReceiveAreGross(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	recv := &VTXOReceivedMsg{
		OutpointHash:  [32]byte{0xaa},
		OutpointIndex: 0,
		AmountSat:     10_000,
		Source:        SourceOOR,
		RoundID:       [16]byte{1},
	}
	require.NoError(t, a.handleVTXOReceived(ctx, recv))

	sent := &VTXOSentMsg{
		SessionID: [32]byte{0x02},
		AmountSat: 10_000,
	}
	require.NoError(t, a.handleVTXOSent(ctx, sent))

	entries := store.getEntries()
	require.Len(t, entries, 2)

	// The receive credits transfers_in; the send debits
	// transfers_out. Neither account nets the other.
	require.Equal(t, AccountTransfersIn, entries[0].CreditAccount)
	require.Equal(t, AccountTransfersOut, entries[1].DebitAccount)
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

// TestHandleUTXOCreated verifies that a UTXO created event is
// recorded in the audit store with the correct fields.
func TestHandleUTXOCreated(t *testing.T) {
	t.Parallel()

	a, auditStore := newTestActorWithAudit(t)
	ctx := context.Background()

	msg := &UTXOCreatedMsg{
		OutpointHash:   [32]byte{0xaa, 0xbb},
		OutpointIndex:  0,
		AmountSat:      50_000,
		BlockHeight:    800_000,
		Classification: "deposit",
	}

	err := a.handleUTXOCreated(ctx, msg)
	require.NoError(t, err)

	entries := auditStore.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "created", entries[0].Event)
	require.Equal(t, "deposit", entries[0].ClassifiedAs)
	require.Equal(t, int64(50_000), entries[0].AmountSat)
	require.Equal(t, int32(800_000), entries[0].BlockHeight)
}

// TestHandleUTXOSpent verifies that a UTXO spent event is
// recorded in the audit store with the correct fields.
func TestHandleUTXOSpent(t *testing.T) {
	t.Parallel()

	a, auditStore := newTestActorWithAudit(t)
	ctx := context.Background()

	msg := &UTXOSpentMsg{
		OutpointHash:   [32]byte{0xcc, 0xdd},
		OutpointIndex:  1,
		AmountSat:      25_000,
		BlockHeight:    800_050,
		Classification: "round_funding",
	}

	err := a.handleUTXOSpent(ctx, msg)
	require.NoError(t, err)

	entries := auditStore.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "spent", entries[0].Event)
	require.Equal(t, "round_funding", entries[0].ClassifiedAs)
	require.Equal(t, int64(25_000), entries[0].AmountSat)
	require.Equal(t, int32(800_050), entries[0].BlockHeight)
}

// TestHandleUTXOCreatedNoAuditStore verifies that UTXO created
// handling succeeds gracefully when no audit store is configured.
func TestHandleUTXOCreatedNoAuditStore(t *testing.T) {
	t.Parallel()

	a, _ := newTestActor(t)
	ctx := context.Background()

	msg := &UTXOCreatedMsg{
		OutpointHash:   [32]byte{0xaa},
		OutpointIndex:  0,
		AmountSat:      10_000,
		BlockHeight:    800_000,
		Classification: "deposit",
	}

	// Should not error even without UTXOAuditStore.
	err := a.handleUTXOCreated(ctx, msg)
	require.NoError(t, err)
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
				Source:        SourceOOR,
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
		{
			name: "UTXOCreated",
			msg: &UTXOCreatedMsg{
				OutpointHash:   [32]byte{0xdd},
				OutpointIndex:  7,
				AmountSat:      30_000,
				BlockHeight:    800_200,
				Classification: "deposit",
			},
			new: func() LedgerMsg {
				return &UTXOCreatedMsg{}
			},
		},
		{
			name: "UTXOSpent",
			msg: &UTXOSpentMsg{
				OutpointHash:   [32]byte{0xee},
				OutpointIndex:  2,
				AmountSat:      45_000,
				BlockHeight:    800_300,
				Classification: "round_funding",
			},
			new: func() LedgerMsg {
				return &UTXOSpentMsg{}
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
