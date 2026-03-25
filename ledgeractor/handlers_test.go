package ledgeractor

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/fees"
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
	entries []fees.LedgerEntry
}

func (m *mockLedgerStore) InsertLedgerEntry(
	_ context.Context, entry fees.LedgerEntry) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.entries = append(m.entries, entry)

	return nil
}

func (m *mockLedgerStore) getEntries() []fees.LedgerEntry {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]fees.LedgerEntry{}, m.entries...)
}

// newTestActor creates a LedgerActor with a mock store for
// testing handlers directly.
func newTestActor(t *testing.T) (*LedgerActor, *mockLedgerStore) {
	t.Helper()

	store := &mockLedgerStore{}
	treasury := fees.NewTreasuryTracker()
	treasury.Initialize(0, 0, 0)

	actor := &LedgerActor{
		cfg: ActorConfig{
			LedgerStore:     store,
			TreasuryTracker: treasury,
		},
		log: nil,
	}

	// Use disabled logger.
	actor.log = disabledLogger()

	return actor, store
}

// TestHandleRoundConfirmed verifies that round confirmation
// creates capital deployment, boarding fee, and mining fee
// ledger entries.
func TestHandleRoundConfirmed(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &RoundConfirmedMsg{
		RoundID:            [16]byte{1, 2, 3},
		TotalVTXOAmountSat: 1_000_000,
		VTXOCount:          10,
		BoardingFeeSat:     2000,
		MiningFeeSat:       500,
		BlockHeight:        800_000,
	}

	err := a.handleRoundConfirmed(ctx, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 3, "expected 3 ledger entries")

	// Check capital deployment entry.
	require.Equal(t, fees.AccountDeployedCapital,
		entries[0].DebitAccount)
	require.Equal(t, fees.AccountTreasuryCapital,
		entries[0].CreditAccount)
	require.Equal(t, int64(1_000_000), entries[0].AmountSat)
	require.Equal(t,
		string(fees.LedgerEventCapitalDeployed),
		entries[0].EventType)

	// Check boarding fee entry.
	require.Equal(t, fees.AccountClientFeesCollected,
		entries[1].DebitAccount)
	require.Equal(t, fees.AccountOperatorRevenue,
		entries[1].CreditAccount)
	require.Equal(t, int64(2000), entries[1].AmountSat)

	// Check mining fee entry.
	require.Equal(t, fees.AccountMiningFees,
		entries[2].DebitAccount)
	require.Equal(t, int64(500), entries[2].AmountSat)

	// Check treasury was updated.
	snap := a.cfg.TreasuryTracker.Snapshot()
	require.Equal(t, int64(1_000_000), snap.DeployedCapitalSat)
	require.Equal(t, 10, snap.LiveVTXOCount)
}

// TestHandleRoundConfirmedZeroFees verifies that zero-value fee
// components are skipped.
func TestHandleRoundConfirmedZeroFees(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &RoundConfirmedMsg{
		RoundID:            [16]byte{4, 5, 6},
		TotalVTXOAmountSat: 500_000,
		VTXOCount:          5,
		BoardingFeeSat:     0,
		MiningFeeSat:       0,
		BlockHeight:        800_001,
	}

	err := a.handleRoundConfirmed(ctx, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1,
		"only capital deployment when fees are zero")
}

// TestHandleVTXOsForfeited verifies forfeit handling.
func TestHandleVTXOsForfeited(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	// Pre-deploy some capital.
	a.cfg.TreasuryTracker.OnRoundConfirmed(1_000_000, 10)

	msg := &VTXOsForfeitedMsg{
		RoundID:        [16]byte{7, 8, 9},
		TotalAmountSat: 300_000,
		Count:          3,
		RefreshFeeSat:  150,
	}

	err := a.handleVTXOsForfeited(ctx, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t,
		string(fees.LedgerEventRefreshFee),
		entries[0].EventType)
	require.Equal(t, int64(150), entries[0].AmountSat)

	// Treasury should be reduced.
	snap := a.cfg.TreasuryTracker.Snapshot()
	require.Equal(t, int64(700_000), snap.DeployedCapitalSat)
}

// TestHandleSweepCompleted verifies sweep handling.
func TestHandleSweepCompleted(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	a.cfg.TreasuryTracker.OnRoundConfirmed(500_000, 5)

	msg := &SweepCompletedMsg{
		BatchID:            [16]byte{10, 11, 12},
		ReclaimedAmountSat: 500_000,
		Count:              5,
		BlockHeight:        800_100,
		FeeRateSatVB:       10,
	}

	err := a.handleSweepCompleted(ctx, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t,
		string(fees.LedgerEventCapitalReclaimed),
		entries[0].EventType)
	require.Equal(t, int64(500_000), entries[0].AmountSat)

	snap := a.cfg.TreasuryTracker.Snapshot()
	require.Equal(t, int64(0), snap.DeployedCapitalSat)
}

// TestHandleOORFinalized verifies OOR volume tracking.
func TestHandleOORFinalized(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &OORFinalizedMsg{
		SessionID:       [32]byte{1},
		InputAmountSat:  100_000,
		OutputAmountSat: 100_000,
	}

	err := a.handleOORFinalized(ctx, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t,
		string(fees.LedgerEventOORTransfer),
		entries[0].EventType)
}

// TestHandleBlockEpoch verifies block epoch handling does not
// error (placeholder for UTXO diff).
func TestHandleBlockEpoch(t *testing.T) {
	t.Parallel()

	a, _ := newTestActor(t)
	ctx := context.Background()

	msg := &BlockEpochMsg{
		BlockHeight: 800_200,
		BlockHash:   [32]byte{0xab, 0xcd},
	}

	err := a.handleBlockEpoch(ctx, msg)
	require.NoError(t, err)
}

// TestMessageTLVRoundTrip verifies that messages can be encoded
// and decoded without data loss.
func TestMessageTLVRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  LedgerMsg
		new  func() LedgerMsg
	}{
		{
			name: "RoundConfirmed",
			msg: &RoundConfirmedMsg{
				RoundID:            [16]byte{1, 2, 3},
				TotalVTXOAmountSat: 999_999,
				VTXOCount:          42,
				BoardingFeeSat:     1234,
				MiningFeeSat:       567,
				BlockHeight:        800_000,
			},
			new: func() LedgerMsg {
				return &RoundConfirmedMsg{}
			},
		},
		{
			name: "SweepCompleted",
			msg: &SweepCompletedMsg{
				BatchID:            [16]byte{4, 5, 6},
				ReclaimedAmountSat: 500_000,
				Count:              5,
				BlockHeight:        800_100,
				FeeRateSatVB:       20,
			},
			new: func() LedgerMsg {
				return &SweepCompletedMsg{}
			},
		},
		{
			name: "BlockEpoch",
			msg: &BlockEpochMsg{
				BlockHeight: 800_200,
				BlockHash:   [32]byte{0xab},
			},
			new: func() LedgerMsg {
				return &BlockEpochMsg{}
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

			// Verify TLV type matches.
			require.Equal(t,
				tc.msg.TLVType(),
				decoded.TLVType(),
			)
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
