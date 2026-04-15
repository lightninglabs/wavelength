package ledger

import (
	"context"
	"errors"
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
		FeeType:     FeeTypeBoarding,
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
		FeeType:     FeeTypeRefresh,
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
// recorded as an expense on transfers_out crediting vtxo_balance
// and that SessionID is stored in the dedicated session_id column
// with RoundID left nil.
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

	// Session identifier lives in session_id, not round_id, so
	// the 32-byte OOR session does not conflict with the
	// 16-byte round idempotency index.
	require.Equal(t, msg.SessionID[:], entries[0].SessionID)
	require.Nil(t, entries[0].RoundID)
}

// TestHandleVTXOSentInRound verifies that a send with RoundID
// set (and SessionID zero) is recorded with round_id populated
// and session_id NULL. Applies to participant-to-participant
// transfers inside a round.
func TestHandleVTXOSentInRound(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &VTXOSentMsg{
		RoundID:   [16]byte{0xaa, 0xbb, 0xcc},
		AmountSat: 25_000,
	}

	err := a.handleVTXOSent(ctx, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountTransfersOut,
		entries[0].DebitAccount)
	require.Equal(t, AccountVTXOBalance,
		entries[0].CreditAccount)
	require.Equal(t, msg.RoundID[:], entries[0].RoundID)
	require.Nil(t, entries[0].SessionID)
}

// TestHandleVTXOSentNeitherSet verifies that a send carrying
// neither SessionID nor RoundID is rejected with a clear error.
// Both zero is ambiguous: the actor cannot tell whether the send
// was in-round or out-of-round.
func TestHandleVTXOSentNeitherSet(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &VTXOSentMsg{
		SessionID: [32]byte{},
		RoundID:   [16]byte{},
		AmountSat: 1,
	}

	err := a.handleVTXOSent(ctx, msg)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMessage)
	require.Contains(t, err.Error(),
		"requires one of SessionID or RoundID")
	require.Empty(t, store.getEntries())
}

// TestHandleVTXOSentBothSet verifies that a send carrying both
// SessionID and RoundID is rejected. An in-round send and an
// OOR send are mutually exclusive contexts.
func TestHandleVTXOSentBothSet(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &VTXOSentMsg{
		SessionID: [32]byte{0x11},
		RoundID:   [16]byte{0x22},
		AmountSat: 1,
	}

	err := a.handleVTXOSent(ctx, msg)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMessage)
	require.Contains(t, err.Error(),
		"cannot set both SessionID and RoundID")
	require.Empty(t, store.getEntries())
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

// TestHandleExitCost verifies that a unilateral exit books two
// ledger entries that together reduce vtxo_balance by the gross
// AmountSat: a send leg for (AmountSat - ExitCostSat) debiting
// transfers_out and a fee leg for ExitCostSat debiting
// onchain_fees. Both legs credit vtxo_balance.
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
	require.Len(t, entries, 2)

	// Send leg: debit transfers_out (net of fee), credit
	// vtxo_balance.
	require.Equal(t, AccountTransfersOut,
		entries[0].DebitAccount)
	require.Equal(t, AccountVTXOBalance,
		entries[0].CreditAccount)
	require.Equal(t, int64(95_000), entries[0].AmountSat)
	require.Equal(t, EventVTXOSent, entries[0].EventType)

	// Fee leg: debit onchain_fees, credit vtxo_balance.
	require.Equal(t, AccountOnchainFees,
		entries[1].DebitAccount)
	require.Equal(t, AccountVTXOBalance,
		entries[1].CreditAccount)
	require.Equal(t, int64(5_000), entries[1].AmountSat)
	require.Equal(t, EventOnchainFeePaid,
		entries[1].EventType)

	// Sanity: the two credit amounts sum to the gross VTXO
	// value, so vtxo_balance drops by the full exited amount.
	require.Equal(
		t, msg.AmountSat,
		entries[0].AmountSat+entries[1].AmountSat,
	)
}

// TestHandleExitCostFeeExceedsValue verifies that an exit whose
// fee meets or exceeds the VTXO amount is rejected rather than
// silently producing a non-positive send leg.
func TestHandleExitCostFeeExceedsValue(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &ExitCostMsg{
		OutpointHash:  [32]byte{0xab},
		OutpointIndex: 0,
		AmountSat:     1_000,
		ExitCostSat:   1_000,
		BlockHeight:   800_600,
	}

	err := a.handleExitCost(ctx, msg)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMessage)
	require.Contains(
		t, err.Error(), "exceeds or equals VTXO amount",
	)
	require.Empty(t, store.getEntries())
}

// TestHandleExitCostInvalidAmounts verifies non-positive inputs
// are rejected.
func TestHandleExitCostInvalidAmounts(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &ExitCostMsg{
		OutpointHash:  [32]byte{0xcd},
		OutpointIndex: 0,
		AmountSat:     0,
		ExitCostSat:   100,
		BlockHeight:   800_700,
	}

	err := a.handleExitCost(ctx, msg)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMessage)
	require.Contains(
		t, err.Error(),
		"positive amount_sat and exit_cost_sat",
	)
	require.Empty(t, store.getEntries())
}

// TestDBErrorDoesNotWrapErrInvalidMessage verifies that an
// error returned by the underlying ledger store does not wrap
// ErrInvalidMessage, so Receive routes it to WarnS instead of
// ErrorS. This guards against DB transient failures paging.
func TestDBErrorDoesNotWrapErrInvalidMessage(t *testing.T) {
	t.Parallel()

	dbErr := errors.New("simulated db lock contention")
	store := &failingLedgerStore{err: dbErr}

	a := &LedgerActor{
		cfg: ActorConfig{LedgerStore: store},
		log: disabledLogger(),
	}

	msg := &FeePaidMsg{
		RoundID:     [16]byte{1},
		AmountSat:   100,
		FeeType:     FeeTypeBoarding,
		BlockHeight: 1,
	}

	err := a.handleFeePaid(t.Context(), msg)
	require.Error(t, err)
	require.ErrorIs(t, err, dbErr)
	require.NotErrorIs(t, err, ErrInvalidMessage)
}

// failingLedgerStore is a LedgerStore that always returns the
// configured error, used to simulate DB failures in tests.
type failingLedgerStore struct {
	err error
}

func (f *failingLedgerStore) InsertLedgerEntry(
	_ context.Context, _ LedgerEntry) error {

	return f.err
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
	require.ErrorIs(t, err, ErrInvalidMessage)
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
	require.ErrorIs(t, err, ErrInvalidMessage)
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
		Classification: ClassificationDeposit,
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
		Classification: ClassificationRoundFunding,
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
		Classification: ClassificationDeposit,
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
				FeeType:     FeeTypeBoarding,
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
			name: "VTXOSentOOR",
			msg: &VTXOSentMsg{
				SessionID: [32]byte{0xbb},
				AmountSat: 10_000,
			},
			new: func() LedgerMsg {
				return &VTXOSentMsg{}
			},
		},
		{
			name: "VTXOSentInRound",
			msg: &VTXOSentMsg{
				RoundID:   [16]byte{0xcc, 0xdd},
				AmountSat: 20_000,
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
				Classification: ClassificationDeposit,
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
				Classification: ClassificationRoundFunding,
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
