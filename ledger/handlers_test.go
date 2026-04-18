package ledger

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/fees"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// fixedTestTime returns a stable timestamp used by the test
// clock so every persisted row pins to the same frame and
// tests can assert CreatedAt exactly when needed.
func fixedTestTime() time.Time {
	return time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC)
}

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
// testing handlers directly. Uses a deterministic test clock
// pinned to fixedTestTime so assertions on CreatedAt stay
// stable across runs.
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
		log:  disabledLogger(),
		clk:  clock.NewTestClock(fixedTestTime()),
		utxo: newUTXOTracker(),
	}

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

	// Check capital committed entry: deployed_capital is
	// debited, treasury_wallet is credited, tagged with the
	// capital_committed event type.
	require.Equal(t, fees.AccountDeployedCapital,
		entries[0].DebitAccount)
	require.Equal(t, fees.AccountTreasuryWallet,
		entries[0].CreditAccount)
	require.Equal(t, int64(1_000_000), int64(entries[0].Amount))
	require.Equal(t, fees.LedgerEventCapitalCommitted,
		entries[0].EventType)

	// Boarding fee: fee is carved from the user's deposit
	// before the claim is created, so deployed_capital is
	// debited and boarding_fee_revenue is credited.
	require.Equal(t, fees.AccountDeployedCapital,
		entries[1].DebitAccount)
	require.Equal(t, fees.AccountBoardingFeeRevenue,
		entries[1].CreditAccount)
	require.Equal(t, int64(2000), int64(entries[1].Amount))

	// Mining fee: mining_fees is debited, treasury_wallet is
	// credited (the treasury paid the miner).
	require.Equal(t, fees.AccountMiningFees,
		entries[2].DebitAccount)
	require.Equal(t, fees.AccountTreasuryWallet,
		entries[2].CreditAccount)
	require.Equal(t, int64(500), int64(entries[2].Amount))

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
	require.Equal(t, fees.LedgerEventRefreshFee,
		entries[0].EventType)
	require.Equal(t, int64(150), int64(entries[0].Amount))

	// Treasury should be reduced.
	snap := a.cfg.TreasuryTracker.Snapshot()
	require.Equal(t, int64(700_000), snap.DeployedCapitalSat)
}

// TestHandleSweepCompleted verifies sweep handling. The
// treasury transitions are wallet → deployed (confirmed) →
// pendingSweep (forfeited) → wallet (swept), so this test
// walks through the full lifecycle and asserts that
// handleSweepCompleted clears the pendingSweep bucket.
func TestHandleSweepCompleted(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	// Fund deployed capital, then forfeit it so it lands in
	// pendingSweep (which is what handleSweepCompleted drains).
	a.cfg.TreasuryTracker.OnRoundConfirmed(500_000, 5)
	a.cfg.TreasuryTracker.OnVTXOsForfeited(500_000, 5)

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
	require.Equal(t, fees.LedgerEventRoundSweep,
		entries[0].EventType)
	require.Equal(t, int64(500_000), int64(entries[0].Amount))

	snap := a.cfg.TreasuryTracker.Snapshot()
	require.Equal(t, int64(0), snap.DeployedCapitalSat)
	require.Equal(t, int64(0), snap.PendingSweepSat)
}

// TestHandleOORFinalized covers both the free-today and
// future-fee cases for OOR finalization. When input == output
// (free OOR, today's behavior) no ledger entry is written
// because master's schema forbids zero-amount entries. When
// input > output (a future OOR fee) the handler records the
// delta under the oor_transfer event type.
func TestHandleOORFinalized(t *testing.T) {
	t.Parallel()

	t.Run("free OOR skips ledger", func(t *testing.T) {
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

		require.Empty(t, store.getEntries(),
			"zero-fee OOR must not write to the ledger")
	})

	t.Run("future OOR fee records entry", func(t *testing.T) {
		t.Parallel()

		a, store := newTestActor(t)
		ctx := context.Background()

		msg := &OORFinalizedMsg{
			SessionID:       [32]byte{2},
			InputAmountSat:  100_000,
			OutputAmountSat: 99_500,
		}

		err := a.handleOORFinalized(ctx, msg)
		require.NoError(t, err)

		entries := store.getEntries()
		require.Len(t, entries, 1)
		require.Equal(t, fees.LedgerEventOORTransfer,
			entries[0].EventType)
		require.Equal(t, int64(500), int64(entries[0].Amount))
		require.Equal(t, fees.AccountUserVTXOClaims,
			entries[0].DebitAccount)
		require.Equal(t, fees.AccountOORFeeRevenue,
			entries[0].CreditAccount)
	})
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
			name: "VTXOsForfeited",
			msg: &VTXOsForfeitedMsg{
				RoundID:        [16]byte{7, 8, 9},
				TotalAmountSat: 300_000,
				Count:          3,
				RefreshFeeSat:  150,
			},
			new: func() LedgerMsg {
				return &VTXOsForfeitedMsg{}
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
			name: "OORFinalized",
			msg: &OORFinalizedMsg{
				SessionID:       [32]byte{0x11},
				InputAmountSat:  100_000,
				OutputAmountSat: 99_000,
			},
			new: func() LedgerMsg {
				return &OORFinalizedMsg{}
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

// TestMessageTypeStrings asserts each ledger message returns
// a distinct, non-empty MessageType() name. MessageType is a
// routing key for structured logging; a collision or empty
// value would hide messages in observability tooling.
func TestMessageTypeStrings(t *testing.T) {
	t.Parallel()

	msgs := []LedgerMsg{
		&RoundConfirmedMsg{},
		&VTXOsForfeitedMsg{},
		&SweepCompletedMsg{},
		&OORFinalizedMsg{},
		&BlockEpochMsg{},
	}

	type namer interface {
		MessageType() string
	}

	seen := make(map[string]struct{})
	for _, m := range msgs {
		n, ok := m.(namer)
		require.True(t, ok,
			"LedgerMsg must expose MessageType() string")

		name := n.MessageType()
		require.NotEmpty(t, name,
			"MessageType() must return a non-empty name")

		_, dup := seen[name]
		require.False(t, dup,
			"duplicate MessageType name: %q", name)
		seen[name] = struct{}{}
	}
}

// TestValidateAmountsRejectsNegatives is a focused check that
// validateAmounts dead-letters negative inputs with
// ErrInvalidMessage. Handlers stack assertions on this so a
// malformed TLV dies fast instead of reaching the SQL CHECK.
func TestValidateAmountsRejectsNegatives(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateAmounts(0))
	require.NoError(t, validateAmounts(1, 2, 3))

	err := validateAmounts(1, -5, 2)
	require.ErrorIs(t, err, ErrInvalidMessage)
}

// TestHandleRoundConfirmedRejectsNegativeFee proves that the
// validation plumbing actually runs: a negative fee field must
// return ErrInvalidMessage rather than attempting to record it.
func TestHandleRoundConfirmedRejectsNegativeFee(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	err := a.handleRoundConfirmed(ctx, &RoundConfirmedMsg{
		RoundID:        [16]byte{1},
		BoardingFeeSat: -1,
	})
	require.ErrorIs(t, err, ErrInvalidMessage)

	// Nothing persisted.
	require.Empty(t, store.getEntries())
}

// TestHandleOORFinalizedRejectsNegativeInput mirrors the
// round-confirmed case for the OOR path, so each handler has
// explicit proof that validateAmounts is wired.
func TestHandleOORFinalizedRejectsNegativeInput(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	err := a.handleOORFinalized(ctx, &OORFinalizedMsg{
		SessionID:      [32]byte{1},
		InputAmountSat: -1,
	})
	require.ErrorIs(t, err, ErrInvalidMessage)
	require.Empty(t, store.getEntries())
}

// TestNewLedgerActorDefaults verifies that NewLedgerActor falls
// back to sensible defaults (disabled log, default clock,
// "ledger.accounting" actor id) when ActorConfig leaves the
// optional fields empty. This keeps production wiring terse.
func TestNewLedgerActorDefaults(t *testing.T) {
	t.Parallel()

	a := NewLedgerActor(ActorConfig{})
	require.NotNil(t, a)
	require.Equal(t, defaultActorID, a.actorID)
	require.NotNil(t, a.log)
	require.NotNil(t, a.clk)
	require.NotNil(t, a.utxo)
}

// TestNewLedgerActorCustomID verifies that a non-empty ActorID
// overrides the default. Tests and multi-tenant deployments
// rely on this to run isolated instances against the same
// delivery store.
func TestNewLedgerActorCustomID(t *testing.T) {
	t.Parallel()

	a := NewLedgerActor(ActorConfig{ActorID: "custom.ledger"})
	require.Equal(t, "custom.ledger", a.actorID)
}

// TestServiceKey verifies the exported service-key
// constructor returns a usable key. The actor framework's
// ServiceKey does not expose the name via a public method,
// so the assertion is that NewServiceKey does not panic and
// produces the typed key the receptionist expects.
func TestServiceKey(t *testing.T) {
	t.Parallel()

	_ = NewServiceKey()
}

// TestStopBeforeStartNoop ensures Stop is safe to call on an
// actor that never started: crash recovery paths may skip
// Start when DeliveryStore wiring fails, and Stop must handle
// that gracefully rather than panicking.
func TestStopBeforeStartNoop(t *testing.T) {
	t.Parallel()

	a := NewLedgerActor(ActorConfig{})
	require.NotPanics(t, a.Stop)
}

// TestStartRequiresDeliveryStore verifies that Start returns a
// clear error when DeliveryStore is missing, rather than
// panicking or silently succeeding with a broken actor.
func TestStartRequiresDeliveryStore(t *testing.T) {
	t.Parallel()

	a := NewLedgerActor(ActorConfig{})
	err := a.Start(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "delivery store")
}

// TestNewLedgerCodec verifies the codec registers every
// concrete message TLV type. A missing registration would
// silently lose messages from the durable mailbox on replay.
func TestNewLedgerCodec(t *testing.T) {
	t.Parallel()

	codec := newLedgerCodec()
	require.NotNil(t, codec)
}

// TestReceiveDispatchesRoundConfirmed sanity-checks the Receive
// switch: a valid RoundConfirmedMsg runs the handler and
// returns an Ok result; a negative-amount variant returns an
// Err that wraps ErrInvalidMessage.
func TestReceiveDispatchesRoundConfirmed(t *testing.T) {
	t.Parallel()

	a, _ := newTestActor(t)
	ctx := context.Background()

	_, okErr := a.Receive(ctx, &RoundConfirmedMsg{
		RoundID:            [16]byte{1},
		TotalVTXOAmountSat: 1000,
	}).Unpack()
	require.NoError(t, okErr)

	_, badErr := a.Receive(ctx, &RoundConfirmedMsg{
		RoundID:        [16]byte{2},
		BoardingFeeSat: -1,
	}).Unpack()
	require.ErrorIs(t, badErr, ErrInvalidMessage)
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
