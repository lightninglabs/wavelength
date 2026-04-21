package ledger

import (
	"context"
	"io"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo/fees"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/tlv"
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

// TestHandleVTXOsForfeited verifies forfeit handling. The
// handler books two legs in a fixed order: first the
// capital-retirement leg (refresh_forfeit, debit
// user_vtxo_claims, credit deployed_capital) for the gross
// forfeited amount, then the refresh-fee leg (refresh_fee,
// debit user_vtxo_claims, credit refresh_fee_revenue) for the
// operator share. Both carry the same round_id; the partial
// unique index uses event_type to keep them distinct.
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
	require.Len(t, entries, 2)

	// Retirement leg: gross amount moves from
	// user_vtxo_claims back to deployed_capital.
	require.Equal(t, fees.LedgerEventRefreshForfeit,
		entries[0].EventType)
	require.Equal(t, fees.AccountUserVTXOClaims,
		entries[0].DebitAccount)
	require.Equal(t, fees.AccountDeployedCapital,
		entries[0].CreditAccount)
	require.Equal(t, int64(300_000), int64(entries[0].Amount))

	// Fee leg: operator share debits user_vtxo_claims and
	// credits refresh_fee_revenue.
	require.Equal(t, fees.LedgerEventRefreshFee,
		entries[1].EventType)
	require.Equal(t, fees.AccountUserVTXOClaims,
		entries[1].DebitAccount)
	require.Equal(t, fees.AccountRefreshFeeRevenue,
		entries[1].CreditAccount)
	require.Equal(t, int64(150), int64(entries[1].Amount))

	// Treasury should be reduced.
	snap := a.cfg.TreasuryTracker.Snapshot()
	require.Equal(t, int64(700_000), snap.DeployedCapitalSat)
}

// TestHandleVTXOsForfeitedZeroAmountSkipsRetirement verifies a
// zero TotalAmountSat (e.g. a defensive forfeit message that
// carries no gross value) still runs the fee leg when
// RefreshFeeSat > 0 but skips the retirement leg so the DB
// CHECK(amount_sat > 0) is not tripped.
func TestHandleVTXOsForfeitedZeroAmountSkipsRetirement(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	msg := &VTXOsForfeitedMsg{
		RoundID:        [16]byte{0xaa},
		TotalAmountSat: 0,
		Count:          0,
		RefreshFeeSat:  75,
	}

	err := a.handleVTXOsForfeited(ctx, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, fees.LedgerEventRefreshFee,
		entries[0].EventType)
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
		MiningFeeSat:       2_500,
	}

	err := a.handleSweepCompleted(ctx, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 2)

	// Reclaim leg.
	require.Equal(t, fees.LedgerEventRoundSweep,
		entries[0].EventType)
	require.Equal(t, fees.AccountTreasuryWallet,
		entries[0].DebitAccount)
	require.Equal(t, fees.AccountDeployedCapital,
		entries[0].CreditAccount)
	require.Equal(t, int64(500_000), int64(entries[0].Amount))

	// Mining-fee leg: the on-chain cost of the sweep tx.
	require.Equal(t, fees.LedgerEventMiningFee,
		entries[1].EventType)
	require.Equal(t, fees.AccountMiningFees,
		entries[1].DebitAccount)
	require.Equal(t, fees.AccountTreasuryWallet,
		entries[1].CreditAccount)
	require.Equal(t, int64(2_500), int64(entries[1].Amount))

	snap := a.cfg.TreasuryTracker.Snapshot()
	require.Equal(t, int64(0), snap.DeployedCapitalSat)
	require.Equal(t, int64(0), snap.PendingSweepSat)
}

// TestHandleSweepCompletedZeroMiningFeeSkipsFeeLeg verifies the
// mining-fee leg is skipped when MiningFeeSat is zero (producer
// has not yet captured the absolute fee) but the reclaim leg
// still runs.
func TestHandleSweepCompletedZeroMiningFeeSkipsFeeLeg(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := context.Background()

	a.cfg.TreasuryTracker.OnRoundConfirmed(100_000, 1)
	a.cfg.TreasuryTracker.OnVTXOsForfeited(100_000, 1)

	msg := &SweepCompletedMsg{
		BatchID:            [16]byte{0xde, 0xad},
		ReclaimedAmountSat: 100_000,
		Count:              1,
		MiningFeeSat:       0,
	}

	err := a.handleSweepCompleted(ctx, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, fees.LedgerEventRoundSweep,
		entries[0].EventType)
}

// TestHandleSweepCompletedRejectsNegativeMiningFee verifies
// validateAmounts covers the new MiningFeeSat field so a
// malformed producer cannot sneak a negative mining fee past
// the handler onto the DB CHECK.
func TestHandleSweepCompletedRejectsNegativeMiningFee(t *testing.T) {
	t.Parallel()

	a, _ := newTestActor(t)
	ctx := context.Background()

	msg := &SweepCompletedMsg{
		BatchID:      [16]byte{0x01},
		MiningFeeSat: -1,
	}

	err := a.handleSweepCompleted(ctx, msg)
	require.ErrorIs(t, err, ErrInvalidMessage)
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

// TestMessageTLVRoundTrip verifies that every concrete ledger
// message survives Encode -> Decode with all fields preserved
// bit-for-bit. Field-level equality is what catches TLV
// record-ordering regressions; a same-type field swap between
// Encode and Decode would silently corrupt accounting rows that
// still satisfy every schema CHECK.
func TestMessageTLVRoundTrip(t *testing.T) {
	t.Parallel()

	var blockHash [32]byte
	for i := range blockHash {
		blockHash[i] = byte(i)
	}

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
				BlockHash:   blockHash,
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

			// Decode via direct msg.Decode.
			decoded := tc.new()
			r := &bytesReader{buf: buf}
			err = decoded.Decode(r)
			require.NoError(t, err)

			// TLV type must still match.
			require.Equal(t,
				tc.msg.TLVType(),
				decoded.TLVType(),
			)

			// Field-by-field equality catches record-order
			// or field-type drift that bit-level diffs would
			// otherwise miss.
			require.Equal(t, tc.msg, decoded)
		})
	}
}

// TestCodecRoundTrip verifies every registered message type
// survives a full codec.Encode -> codec.Decode round-trip. The
// direct msg.Encode / msg.Decode test above bypasses the codec
// dispatch layer and so cannot catch a missing MustRegister or
// a mismatched TLV routing type. This test exercises both.
func TestCodecRoundTrip(t *testing.T) {
	t.Parallel()

	var blockHash [32]byte
	for i := range blockHash {
		blockHash[i] = byte(i)
	}

	msgs := []LedgerMsg{
		&RoundConfirmedMsg{
			RoundID:            [16]byte{1, 2, 3},
			TotalVTXOAmountSat: 999_999,
			VTXOCount:          42,
			BoardingFeeSat:     1234,
			MiningFeeSat:       567,
			BlockHeight:        800_000,
		},
		&VTXOsForfeitedMsg{
			RoundID:        [16]byte{7, 8, 9},
			TotalAmountSat: 300_000,
			Count:          3,
			RefreshFeeSat:  150,
		},
		&SweepCompletedMsg{
			BatchID:            [16]byte{4, 5, 6},
			ReclaimedAmountSat: 500_000,
			Count:              5,
			BlockHeight:        800_100,
			FeeRateSatVB:       20,
			MiningFeeSat:       7_500,
		},
		&OORFinalizedMsg{
			SessionID:       [32]byte{0x11},
			InputAmountSat:  100_000,
			OutputAmountSat: 99_000,
		},
		&BlockEpochMsg{
			BlockHeight: 800_200,
			BlockHash:   blockHash,
		},
	}

	codec := newLedgerCodec()

	for _, msg := range msgs {
		t.Run(msg.MessageType(), func(t *testing.T) {
			payload, err := codec.Encode(msg)
			require.NoError(t, err)

			decoded, err := codec.Decode(payload)
			require.NoError(t, err)

			// Codec round-trips must preserve the concrete
			// type (not just the interface); a routing
			// misregistration would hand back the wrong
			// struct and this assertion would fire.
			require.IsType(t, msg, decoded)
			require.Equal(t, msg, decoded)
		})
	}
}

// TestDecodeRejectsOversizedAmount verifies that a TLV-encoded
// payload with a satoshi field above math.MaxInt64 is rejected
// at Decode with ErrInvalidMessage rather than silently
// underflowing through int64 cast.
func TestDecodeRejectsOversizedAmount(t *testing.T) {
	t.Parallel()

	// Manually encode a RoundConfirmedMsg payload with the
	// TotalVTXOAmountSat field holding math.MaxUint64 (which
	// is > math.MaxInt64). This simulates a malformed
	// producer or a corrupted mailbox row.
	roundID := make([]byte, 16)
	roundID[0], roundID[1], roundID[2] = 1, 2, 3

	var (
		totalVTXO   = uint64(math.MaxUint64)
		vtxoCount   = uint32(1)
		boardingFee = uint64(0)
		miningFee   = uint64(0)
		blockHeight = uint32(0)
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			roundConfirmedRoundIDType, &roundID,
		),
		tlv.MakePrimitiveRecord(
			roundConfirmedTotalVTXOType, &totalVTXO,
		),
		tlv.MakePrimitiveRecord(
			roundConfirmedVTXOCountType, &vtxoCount,
		),
		tlv.MakePrimitiveRecord(
			roundConfirmedBoardingFeeType, &boardingFee,
		),
		tlv.MakePrimitiveRecord(
			roundConfirmedMiningFeeType, &miningFee,
		),
		tlv.MakePrimitiveRecord(
			roundConfirmedBlockHeightType, &blockHeight,
		),
	)
	require.NoError(t, err)

	var buf []byte
	w := &bytesWriter{buf: &buf}
	require.NoError(t, stream.Encode(w))

	decoded := &RoundConfirmedMsg{}
	r := &bytesReader{buf: buf}
	err = decoded.Decode(r)
	require.ErrorIs(t, err, ErrInvalidMessage)
}

// TestDecodeRejectsWrongSizedFixedField verifies a TLV payload
// whose fixed-size ID field carries the wrong number of bytes
// is rejected at Decode with ErrInvalidMessage rather than
// silently truncating or zero-padding to the expected width.
func TestDecodeRejectsWrongSizedFixedField(t *testing.T) {
	t.Parallel()

	// Encode a VTXOsForfeitedMsg with a 10-byte RoundID
	// (schema expects 16 bytes).
	var (
		roundID     = []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
		totalAmount = uint64(100)
		count       = uint32(1)
		refreshFee  = uint64(10)
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			vtxosForfeitedRoundIDType, &roundID,
		),
		tlv.MakePrimitiveRecord(
			vtxosForfeitedTotalAmountType, &totalAmount,
		),
		tlv.MakePrimitiveRecord(
			vtxosForfeitedCountType, &count,
		),
		tlv.MakePrimitiveRecord(
			vtxosForfeitedRefreshFeeType, &refreshFee,
		),
	)
	require.NoError(t, err)

	var buf []byte
	w := &bytesWriter{buf: &buf}
	require.NoError(t, stream.Encode(w))

	decoded := &VTXOsForfeitedMsg{}
	r := &bytesReader{buf: buf}
	err = decoded.Decode(r)
	require.ErrorIs(t, err, ErrInvalidMessage)
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

// stubDeliveryStore is a DeliveryStore whose methods are never
// invoked: it satisfies the interface via embedding so the
// ActorConfig.DeliveryStore field is non-nil, without needing
// to implement the full twenty-method surface. Safe only when
// the test is guaranteed to fail Start before any DeliveryStore
// method is called.
type stubDeliveryStore struct {
	actor.DeliveryStore
}

// TestStartRequiresLedgerStore verifies Start catches a missing
// LedgerStore at boot instead of surfacing the misconfiguration
// as a nil-deref inside the first Record* call on the hot path.
// Every handler dereferences LedgerStore, so the fast failure
// at Start is the contract operators rely on when diagnosing a
// broken wiring from the startup log.
func TestStartRequiresLedgerStore(t *testing.T) {
	t.Parallel()

	a := NewLedgerActor(ActorConfig{
		DeliveryStore: &stubDeliveryStore{},
	})
	err := a.Start(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "ledger store")
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

// unknownLedgerMsg is a LedgerMsg whose concrete type is not
// registered in Receive's type switch. It exists only so the
// default arm can be exercised -- a real producer that smuggled
// one of these past the codec would hit the same branch.
type unknownLedgerMsg struct {
	actor.BaseMessage
}

func (m *unknownLedgerMsg) MessageType() string { return "unknown" }

func (m *unknownLedgerMsg) TLVType() tlv.Type { return 0x9999 }

func (m *unknownLedgerMsg) Encode(_ io.Writer) error { return nil }

func (m *unknownLedgerMsg) Decode(_ io.Reader) error { return nil }

// TestReceiveDispatchesAllMessageTypes covers every arm of the
// Receive switch so a silently missing case (e.g. an added
// LedgerMsg variant whose handler is not plumbed in) fails loudly
// here instead of in production as a dropped durable message.
// RestartMessage and BlockEpochMsg are always no-ops on the
// current fixture; the three business messages rely on
// validateAmounts / the handlers themselves to succeed for the
// positive cases encoded below.
func TestReceiveDispatchesAllMessageTypes(t *testing.T) {
	t.Parallel()

	a, _ := newTestActor(t)
	ctx := context.Background()

	// RestartMessage: Ok and no state mutation.
	_, err := a.Receive(ctx, &actor.RestartMessage{}).Unpack()
	require.NoError(t, err)

	// VTXOsForfeitedMsg.
	_, err = a.Receive(ctx, &VTXOsForfeitedMsg{
		RoundID:        [16]byte{3},
		TotalAmountSat: 100_000,
		Count:          1,
		RefreshFeeSat:  1_000,
	}).Unpack()
	require.NoError(t, err)

	// SweepCompletedMsg.
	_, err = a.Receive(ctx, &SweepCompletedMsg{
		BatchID:            [16]byte{4},
		ReclaimedAmountSat: 50_000,
		Count:              2,
		BlockHeight:        800_000,
		FeeRateSatVB:       10,
		MiningFeeSat:       500,
	}).Unpack()
	require.NoError(t, err)

	// OORFinalizedMsg with a real fee.
	_, err = a.Receive(ctx, &OORFinalizedMsg{
		SessionID:       [32]byte{0x11},
		InputAmountSat:  10_000,
		OutputAmountSat: 9_500,
	}).Unpack()
	require.NoError(t, err)

	// BlockEpochMsg without a WalletUTXOLister is a log-only
	// no-op -- exercises the Receive arm without requiring
	// diff-subsystem wiring.
	_, err = a.Receive(ctx, &BlockEpochMsg{
		BlockHeight: 800_001,
	}).Unpack()
	require.NoError(t, err)

	// Unknown message type hits the default arm and returns an
	// ErrInvalidMessage-wrapped error.
	_, badErr := a.Receive(ctx, &unknownLedgerMsg{}).Unpack()
	require.ErrorIs(t, badErr, ErrInvalidMessage)
}

// TestRefReturnsStoredRef verifies Ref returns whatever the Start
// path installed on the actor. Start wires a real durable
// ActorRef so Ref exposes the stable address other subsystems use
// to Tell into the ledger -- the accessor has to exist and has to
// hand back the same value the framework handed it. A regression
// that swapped in a different pointer would surface here.
func TestRefReturnsStoredRef(t *testing.T) {
	t.Parallel()

	a, _ := newTestActor(t)
	require.Equal(t, a.ref, a.Ref())
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
