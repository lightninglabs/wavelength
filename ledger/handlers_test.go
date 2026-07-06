package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/tlv"
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

func (m *mockLedgerStore) InsertLedgerEntry(_ context.Context,
	entry LedgerEntry) error {

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

func (m *mockUTXOAuditStore) InsertUTXOAuditEntry(_ context.Context,
	entry UTXOAuditEntry) error {

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

	a := &LedgerActor{
		cfg: ActorConfig{
			LedgerStore: store,
		},
		log: disabledLogger(),
		clk: clock.NewDefaultClock(),
	}

	return a, store
}

// newTestActorWithStore builds an actor bound to an explicit
// LedgerStore implementation. Used by tests that wire a custom
// store (e.g. the replay-idempotency dedup mock) instead of the
// default append-only mockLedgerStore.
func newTestActorWithStore(t *testing.T, store LedgerStore) *LedgerActor {
	t.Helper()

	return &LedgerActor{
		cfg: ActorConfig{
			LedgerStore: store,
		},
		log: disabledLogger(),
		clk: clock.NewDefaultClock(),
	}
}

// newTestActorWithAudit creates a LedgerActor with both a mock
// ledger store and a mock UTXO audit store. Returning both lets
// a test assert on the double-entry wallet deposit row that
// handleUTXOCreated writes alongside the wallet_utxo_log audit
// row, instead of only seeing the audit side.
func newTestActorWithAudit(t *testing.T) (*LedgerActor, *mockLedgerStore,
	*mockUTXOAuditStore) {

	t.Helper()

	ledgerStore := &mockLedgerStore{}
	auditStore := &mockUTXOAuditStore{}

	a := &LedgerActor{
		cfg: ActorConfig{
			LedgerStore:    ledgerStore,
			UTXOAuditStore: auditStore,
		},
		log: disabledLogger(),
		clk: clock.NewDefaultClock(),
	}

	return a, ledgerStore, auditStore
}

// fakeExec is a synchronous Exec[ledgerTx] for handler unit tests. It runs
// Read/Commit closures immediately against the actor's stores, with no real
// transaction or lease fence, so a handler's build-then-Commit flow can be
// exercised without standing up a durable mailbox.
type fakeExec struct {
	store ledgerTx
}

// Read runs fn against the actor's stores.
func (e fakeExec) Read(ctx context.Context,
	fn func(context.Context, ledgerTx) error) error {

	return fn(ctx, e.store)
}

// Stage runs fn against the actor's stores. The ledger handlers do not stage
// (they validate then Commit), but the Exec interface requires it.
func (e fakeExec) Stage(ctx context.Context,
	fn func(context.Context, ledgerTx) error) error {

	return fn(ctx, e.store)
}

// Commit runs fn against the actor's stores.
func (e fakeExec) Commit(ctx context.Context,
	fn func(context.Context, ledgerTx) error) error {

	return fn(ctx, e.store)
}

// run drives a message through the actor's Receive with a synchronous fake
// Exec and returns the handler error. The insert closures execute against the
// actor's mock stores, so the existing store-based assertions still hold while
// the validation/build work runs (as in production) outside any transaction.
func run(ctx context.Context, a *LedgerActor, msg LedgerMsg) error {
	ax := fakeExec{store: a.bindStores(ctx, nil)}

	return a.Receive(ctx, msg, ax).Err()
}

// TestHandleFeePaidBoarding verifies that a boarding fee is
// recorded with the correct accounts and event type.
func TestHandleFeePaidBoarding(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &FeePaidMsg{
		RoundID: [16]byte{
			1,
			2,
			3,
		},
		AmountSat:   1500,
		FeeType:     FeeTypeBoarding,
		BlockHeight: 800_000,
	}

	err := run(ctx, a, msg)
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
	ctx := t.Context()

	msg := &FeePaidMsg{
		RoundID: [16]byte{
			4,
			5,
			6,
		},
		AmountSat:   750,
		FeeType:     FeeTypeRefresh,
		BlockHeight: 800_100,
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, EventRefreshFeePaid,
		entries[0].EventType)
}

// TestHandleFeePaidOnchainSweep verifies that a boarding-sweep chain cost
// is booked against onchain_fees / wallet_clearing with the
// boarding_sweep_fee_paid event type, and that an empty RoundID is
// accepted alongside a sweep-txid IdempotencyKey.
func TestHandleFeePaidOnchainSweep(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	sweepTxid := [32]byte{
		0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11,
		0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99,
		0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11,
		0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99,
	}
	msg := &FeePaidMsg{
		AmountSat:      444,
		FeeType:        FeeTypeOnchainSweep,
		BlockHeight:    800_650,
		IdempotencyKey: append([]byte(nil), sweepTxid[:]...),
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountOnchainFees, entries[0].DebitAccount)
	require.Equal(t, AccountWalletClearing, entries[0].CreditAccount)
	require.Equal(t, int64(444), entries[0].AmountSat)
	require.Equal(t, EventBoardingSweepFeePaid, entries[0].EventType)

	// Onchain-sweep fees do NOT carry a RoundID; the dedup key is the
	// sweep txid plumbed via IdempotencyKey, so the
	// idx_client_ledger_idempotent_key partial unique index handles
	// replay safety.
	require.Nil(
		t, entries[0].RoundID,
		"onchain-sweep fees must not carry a RoundID",
	)
	require.Equal(t, sweepTxid[:], entries[0].IdempotencyKey)
}

// TestHandleFeePaidOnchainSweepRejectsZeroAmount verifies that the same
// non-positive-amount guard the operator-fee path uses also fires for
// onchain-sweep fees.
func TestHandleFeePaidOnchainSweepRejectsZeroAmount(t *testing.T) {
	t.Parallel()

	a, _ := newTestActor(t)
	ctx := t.Context()

	msg := &FeePaidMsg{
		AmountSat:   0,
		FeeType:     FeeTypeOnchainSweep,
		BlockHeight: 800_700,
	}

	err := run(ctx, a, msg)
	require.ErrorIs(t, err, ErrInvalidMessage)
}

// TestHandleFeePaidOnchainSweepRejectsBadIdempotencyKey verifies that
// sweep fee rows cannot bypass every ledger idempotency index. Onchain
// sweep fees have no RoundID, so the sweep txid key is mandatory.
func TestHandleFeePaidOnchainSweepRejectsBadIdempotencyKey(t *testing.T) {
	t.Parallel()

	for _, key := range [][]byte{
		nil,
		{},
		{
			0x01,
			0x02,
		},
		make([]byte, chainhash.HashSize+1),
	} {
		t.Run(fmt.Sprintf("len_%d", len(key)), func(t *testing.T) {
			t.Parallel()

			a, store := newTestActor(t)
			ctx := t.Context()

			msg := &FeePaidMsg{
				AmountSat:      1_000,
				FeeType:        FeeTypeOnchainSweep,
				BlockHeight:    800_700,
				IdempotencyKey: key,
			}

			err := run(ctx, a, msg)
			require.ErrorIs(t, err, ErrInvalidMessage)
			require.Empty(t, store.getEntries())
		})
	}
}

// TestHandleVTXOReceivedRoundBoarding verifies that a boarding
// or refresh receive is recorded with wallet_balance ->
// vtxo_balance (own on-chain funds converted to VTXO balance).
func TestHandleVTXOReceivedRoundBoarding(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &VTXOReceivedMsg{
		OutpointHash: [32]byte{
			0xaa,
			0xbb,
		},
		OutpointIndex: 0,
		AmountSat:     50_000,
		Source:        SourceRoundBoarding,
		RoundID: [16]byte{
			7,
			8,
			9,
		},
	}

	err := run(ctx, a, msg)
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

	// The VTXO outpoint must land on the row's structured chain
	// fields too. Issue #504: without these the wallet's onchain
	// view renders a "round"-kind row with an empty txid and the
	// outpoint only available by parsing the description blob.
	require.Equal(
		t, msg.OutpointHash[:], entries[0].ChainTxid,
		"chain_txid must carry the VTXO outpoint hash",
	)
	require.NotNil(t, entries[0].ChainVout)
	require.Equal(
		t, int32(msg.OutpointIndex), *entries[0].ChainVout,
		"chain_vout must carry the VTXO outpoint index",
	)
}

// TestHandleVTXOReceivedRoundTransfer verifies that an in-round
// participant-to-participant VTXO receive is recorded the same
// way as an OOR receive: transfers_in -> vtxo_balance.
func TestHandleVTXOReceivedRoundTransfer(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &VTXOReceivedMsg{
		OutpointHash: [32]byte{
			0xab,
			0xcd,
		},
		OutpointIndex: 0,
		AmountSat:     30_000,
		Source:        SourceRoundTransfer,
		RoundID: [16]byte{
			13,
			14,
			15,
		},
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountVTXOBalance,
		entries[0].DebitAccount)
	require.Equal(t, AccountTransfersIn,
		entries[0].CreditAccount)
}

// TestHandleVTXOReceivedRoundRefresh verifies that a refresh
// (or directed-send self-change) receive is booked vtxo_balance
// -> transfers_out, so the paired VTXOSentMsg (gross forfeit)
// and this leg cancel on transfers_out and the net effect on
// vtxo_balance is -fee once the FeePaidMsg lands separately.
// Crediting transfers_out instead of wallet_balance prevents
// wallet_balance from drifting on a flow that never touches the
// on-chain wallet.
func TestHandleVTXOReceivedRoundRefresh(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &VTXOReceivedMsg{
		OutpointHash: [32]byte{
			0xef,
			0x01,
		},
		OutpointIndex: 2,
		AmountSat:     40_000,
		Source:        SourceRoundRefresh,
		RoundID: [16]byte{
			16,
			17,
			18,
		},
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountVTXOBalance,
		entries[0].DebitAccount)
	require.Equal(t, AccountTransfersOut,
		entries[0].CreditAccount)
	require.Equal(t, int64(40_000), entries[0].AmountSat)
	require.Equal(t, EventVTXOReceived,
		entries[0].EventType)
}

// TestRefreshRoundNetsToFeeOnVTXOBalance is a scenario-level test
// that runs the three messages a refresh round emits
// (VTXOSent(gross) + VTXOReceived(SourceRoundRefresh, gross) +
// FeePaidMsg(refresh, fee)) against a shared mock store and asserts
// the running balances match the intended accounting model:
//   - transfers_out: net zero (debit from VTXOSent cancels credit
//     from SourceRoundRefresh).
//   - wallet_balance: untouched.
//   - vtxo_balance: down by exactly the fee (two offsetting legs
//     plus one fee credit).
//   - fees_paid: up by the fee.
//
// A regression here would indicate the refresh-handling invariant
// was broken in a later refactor.
func TestRefreshRoundNetsToFeeOnVTXOBalance(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	roundID := [16]byte{0xaa, 0xbb, 0xcc}
	const gross int64 = 100_000
	const fee int64 = 750

	// Leg 1: forfeit of the old VTXO shows as a VTXOSent with
	// RoundID set.
	require.NoError(
		t,
		run(
			ctx, a, &VTXOSentMsg{
				RoundID:   roundID,
				AmountSat: gross,
			},
		),
	)

	// Leg 2: new VTXO materializes with Source=SourceRoundRefresh.
	require.NoError(
		t,
		run(
			ctx, a, &VTXOReceivedMsg{
				OutpointHash:  [32]byte{0x01},
				OutpointIndex: 0,
				AmountSat:     gross,
				Source:        SourceRoundRefresh,
				RoundID:       roundID,
			},
		),
	)

	// Leg 3: the operator fee for the refresh round.
	require.NoError(
		t,
		run(
			ctx, a, &FeePaidMsg{
				RoundID:     roundID,
				AmountSat:   fee,
				FeeType:     FeeTypeRefresh,
				BlockHeight: 800_000,
			},
		),
	)

	entries := store.getEntries()
	require.Len(t, entries, 3)

	// Compute the running account balances (debit - credit per
	// account) across the three entries, matching what the DB
	// GetAccountBalance query would return.
	balances := map[string]int64{}
	for _, e := range entries {
		balances[e.DebitAccount] += e.AmountSat
		balances[e.CreditAccount] -= e.AmountSat
	}

	require.Equal(
		t, int64(0), balances[AccountTransfersOut],
		"forfeit+refresh legs must cancel on transfers_out",
	)
	require.Equal(
		t, int64(0), balances[AccountWalletBalance],
		"refresh must not touch wallet_balance",
	)
	require.Equal(
		t, -fee, balances[AccountVTXOBalance],
		"vtxo_balance must drop by exactly the operator fee",
	)
	require.Equal(
		t, fee, balances[AccountFeesPaid],
		"fees_paid must rise by exactly the operator fee",
	)
}

// TestHandleVTXOReceivedOOR verifies that an OOR-sourced VTXO
// received is recorded with vtxo_balance -> transfers_in.
func TestHandleVTXOReceivedOOR(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &VTXOReceivedMsg{
		OutpointHash: [32]byte{
			0xcc,
			0xdd,
		},
		OutpointIndex: 1,
		AmountSat:     25_000,
		Source:        SourceOOR,
		RoundID: [16]byte{
			10,
			11,
			12,
		},
	}

	err := run(ctx, a, msg)
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
	ctx := t.Context()

	msg := &VTXOSentMsg{
		SessionID: [32]byte{
			0x01,
		},
		AmountSat: 10_000,
	}

	err := run(ctx, a, msg)
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
	ctx := t.Context()

	msg := &VTXOSentMsg{
		RoundID: [16]byte{
			0xaa,
			0xbb,
			0xcc,
		},
		AmountSat: 25_000,
	}

	err := run(ctx, a, msg)
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
	ctx := t.Context()

	msg := &VTXOSentMsg{
		SessionID: [32]byte{},
		RoundID:   [16]byte{},
		AmountSat: 1,
	}

	err := run(ctx, a, msg)
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
	ctx := t.Context()

	msg := &VTXOSentMsg{
		SessionID: [32]byte{
			0x11,
		},
		RoundID: [16]byte{
			0x22,
		},
		AmountSat: 1,
	}

	err := run(ctx, a, msg)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMessage)
	require.Contains(
		t, err.Error(),
		"cannot set both SessionID and RoundID",
	)
	require.Empty(t, store.getEntries())
}

// TestHandleVTXOSendReceiveAreGross verifies that a matched
// receive and send of the same amount do not net to zero on a
// single account: transfers_in accumulates credits and
// transfers_out accumulates debits independently.
func TestHandleVTXOSendReceiveAreGross(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	recv := &VTXOReceivedMsg{
		OutpointHash: [32]byte{
			0xaa,
		},
		OutpointIndex: 0,
		AmountSat:     10_000,
		Source:        SourceOOR,
		RoundID: [16]byte{
			1,
		},
	}
	require.NoError(
		t,
		run(
			ctx, a, recv,
		),
	)

	sent := &VTXOSentMsg{
		SessionID: [32]byte{
			0x02,
		},
		AmountSat: 10_000,
	}
	require.NoError(t, run(ctx, a, sent))

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
	ctx := t.Context()

	msg := &ExitCostMsg{
		OutpointHash: [32]byte{
			0xee,
			0xff,
		},
		OutpointIndex: 2,
		AmountSat:     100_000,
		ExitCostSat:   5_000,
		BlockHeight:   800_500,
	}

	err := run(ctx, a, msg)
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
		t, msg.AmountSat, entries[0].AmountSat+entries[1].AmountSat,
	)
}

// TestHandleExitCostFeeExceedsValue verifies that an exit whose
// fee meets or exceeds the VTXO amount is rejected rather than
// silently producing a non-positive send leg.
func TestHandleExitCostFeeExceedsValue(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &ExitCostMsg{
		OutpointHash: [32]byte{
			0xab,
		},
		OutpointIndex: 0,
		AmountSat:     1_000,
		ExitCostSat:   1_000,
		BlockHeight:   800_600,
	}

	err := run(ctx, a, msg)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMessage)
	require.Contains(
		t, err.Error(), "exceeds or equals VTXO amount",
	)
	require.Empty(t, store.getEntries())
}

// dedupLedgerStore mirrors the DB-side behavior of the partial
// unique indexes combined with ON CONFLICT DO NOTHING: inserts
// whose (idempotency_key, event_type, debit_account, credit_account)
// already appear are silently dropped. Tests use this to assert
// replay semantics without running a real DB migration.
type dedupLedgerStore struct {
	mu      sync.Mutex
	entries []LedgerEntry
	keys    map[string]struct{}
}

// newDedupLedgerStore constructs a fresh dedupLedgerStore.
func newDedupLedgerStore() *dedupLedgerStore {
	return &dedupLedgerStore{
		keys: make(map[string]struct{}),
	}
}

// InsertLedgerEntry appends the entry unless a previous insert
// already covered the same idempotency_key + account/event tuple,
// in which case the call is a silent no-op. Mirrors the
// idx_client_ledger_idempotent_key partial unique index plus the
// ON CONFLICT DO NOTHING clause on InsertClientLedgerEntry.
func (d *dedupLedgerStore) InsertLedgerEntry(_ context.Context,
	entry LedgerEntry) error {

	d.mu.Lock()
	defer d.mu.Unlock()

	if len(entry.IdempotencyKey) > 0 {
		k := fmt.Sprintf("%x|%s|%s|%s", entry.IdempotencyKey,
			entry.EventType, entry.DebitAccount,
			entry.CreditAccount)
		if _, seen := d.keys[k]; seen {
			return nil
		}
		d.keys[k] = struct{}{}
	}

	d.entries = append(d.entries, entry)

	return nil
}

// getEntries returns a snapshot of the persisted entries.
func (d *dedupLedgerStore) getEntries() []LedgerEntry {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]LedgerEntry{}, d.entries...)
}

// TestHandleExitCostWritesBothLegsWithSharedKey verifies that
// handleExitCost emits the send leg and the fee leg with the
// correct account sides and that both carry the same
// outpoint-derived IdempotencyKey. The durable actor's outer tx
// provides crash atomicity for the two writes; the shared
// idempotency key is what makes an out-of-band replay safe via
// idx_client_ledger_idempotent_key + ON CONFLICT DO NOTHING.
func TestHandleExitCostWritesBothLegsWithSharedKey(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &ExitCostMsg{
		OutpointHash: [32]byte{
			0xab,
		},
		OutpointIndex: 7,
		AmountSat:     50_000,
		ExitCostSat:   3_500,
		BlockHeight:   800_800,
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 2)

	// Send leg: transfers_out <- vtxo_balance for net amount.
	require.Equal(t, AccountTransfersOut, entries[0].DebitAccount)
	require.Equal(
		t, AccountVTXOBalance, entries[0].CreditAccount,
	)
	require.Equal(t, int64(46_500), entries[0].AmountSat)
	require.Equal(t, EventVTXOSent, entries[0].EventType)

	// Fee leg: onchain_fees <- vtxo_balance for the exit cost.
	require.Equal(t, AccountOnchainFees, entries[1].DebitAccount)
	require.Equal(
		t, AccountVTXOBalance, entries[1].CreditAccount,
	)
	require.Equal(t, int64(3_500), entries[1].AmountSat)
	require.Equal(t, EventOnchainFeePaid, entries[1].EventType)

	// Both legs must carry the same outpoint-scoped
	// IdempotencyKey so the partial unique index
	// idx_client_ledger_idempotent_key dedups a replay.
	key := exitIdempotencyKey(msg.OutpointHash, msg.OutpointIndex)
	require.Equal(t, key, entries[0].IdempotencyKey)
	require.Equal(t, key, entries[1].IdempotencyKey)
	require.Len(t, key, 36)
}

// TestHandleExitCostReplayIsIdempotent simulates an at-least-once
// redelivery of the same ExitCostMsg and asserts that the store
// still ends up with exactly the two original legs rather than
// four. This validates the combined contract of:
//
//   - two handleExitCost invocations produce four insert calls
//   - the shared outpoint-derived IdempotencyKey puts every
//     leg under the partial unique index
//     idx_client_ledger_idempotent_key
//   - ON CONFLICT DO NOTHING at the DB adapter layer turns the
//     second pass into a silent no-op
//
// Dropping any one of those three pieces causes the row count to
// grow and the test to fail.
func TestHandleExitCostReplayIsIdempotent(t *testing.T) {
	t.Parallel()

	store := newDedupLedgerStore()
	a := newTestActorWithStore(t, store)
	ctx := t.Context()

	msg := &ExitCostMsg{
		OutpointHash: [32]byte{
			0xab,
		},
		OutpointIndex: 7,
		AmountSat:     50_000,
		ExitCostSat:   3_500,
		BlockHeight:   800_800,
	}

	// First delivery persists both legs.
	require.NoError(t, run(ctx, a, msg))
	require.Len(t, store.getEntries(), 2)

	// Second delivery of the identical message is the
	// at-least-once replay scenario. Row count must not grow.
	require.NoError(t, run(ctx, a, msg))
	require.Len(
		t, store.getEntries(), 2,
		"replay must not double-book ledger entries",
	)

	// A third run with a different outpoint (different
	// idempotency key) must still persist; this guards against
	// an overzealous dedup that keys only on event_type or
	// only on account pairs.
	other := *msg
	other.OutpointIndex = 8
	require.NoError(
		t,
		run(
			ctx, a, &other,
		),
	)
	require.Len(
		t, store.getEntries(), 4,
		"distinct outpoint must not be deduped",
	)
}

// TestExitIdempotencyKeyDistinguishesOutputs confirms the key
// derivation distinguishes outputs that share a txid but differ
// in the output index -- the scenario where two exit legs on the
// same tx must not collide in the unique index.
func TestExitIdempotencyKeyDistinguishesOutputs(t *testing.T) {
	t.Parallel()

	hash := [32]byte{0xde, 0xad}

	k0 := exitIdempotencyKey(hash, 0)
	k1 := exitIdempotencyKey(hash, 1)
	kMax := exitIdempotencyKey(hash, 1<<31)

	require.NotEqual(t, k0, k1)
	require.NotEqual(t, k1, kMax)
	require.NotEqual(t, k0, kMax)
}

// TestHandleExitCostInvalidAmounts verifies non-positive inputs
// are rejected. The table covers the three distinct invalid
// shapes: zero amount (caller forgot the VTXO value), zero fee
// (caller emits before the final sweep cost is known -- this is
// the exact poison-pill the vtxo.emitExitCost no-op guards
// against), and both-zero.
func TestHandleExitCostInvalidAmounts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		amount  int64
		exit    int64
		contain string
	}{
		{
			name:    "zero amount",
			amount:  0,
			exit:    100,
			contain: "positive amount_sat and exit_cost_sat",
		},
		{
			name:    "zero exit cost (poison-pill shape)",
			amount:  10_000,
			exit:    0,
			contain: "positive amount_sat and exit_cost_sat",
		},
		{
			name:    "both zero",
			amount:  0,
			exit:    0,
			contain: "positive amount_sat and exit_cost_sat",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, store := newTestActor(t)
			ctx := t.Context()

			msg := &ExitCostMsg{
				OutpointHash: [32]byte{
					0xcd,
				},
				OutpointIndex: 0,
				AmountSat:     tc.amount,
				ExitCostSat:   tc.exit,
				BlockHeight:   800_700,
			}

			err := run(
				ctx, a, msg,
			)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrInvalidMessage)
			require.Contains(t, err.Error(), tc.contain)
			require.Empty(t, store.getEntries())
		})
	}
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
		cfg: ActorConfig{
			LedgerStore: store,
		},
		log: disabledLogger(),
		clk: clock.NewDefaultClock(),
	}

	msg := &FeePaidMsg{
		RoundID: [16]byte{
			1,
		},
		AmountSat:   100,
		FeeType:     FeeTypeBoarding,
		BlockHeight: 1,
	}

	err := run(t.Context(), a, msg)
	require.Error(t, err)
	require.ErrorIs(t, err, dbErr)
	require.NotErrorIs(t, err, ErrInvalidMessage)
}

// failingLedgerStore is a LedgerStore that always returns the
// configured error, used to simulate DB failures in tests.
type failingLedgerStore struct {
	err error
}

func (f *failingLedgerStore) InsertLedgerEntry(_ context.Context,
	_ LedgerEntry) error {

	return f.err
}

// TestHandleFeePaidUnknownType verifies that an unknown fee type
// returns an error instead of silently misclassifying the entry.
// TestHandleNonPositiveAmounts exercises the early-return guards
// on every handler that writes a single positive-amount ledger
// entry. A corrupt TLV that decodes to a zero or negative amount
// must surface as ErrInvalidMessage (rejection dead-letters at
// the mailbox layer) rather than hitting the SQL CHECK and
// driving an infinite durable retry.
func TestHandleNonPositiveAmounts(t *testing.T) {
	t.Parallel()

	type handlerFn func(
		ctx context.Context, a *LedgerActor, amt int64,
	) error

	cases := []struct {
		name string
		run  handlerFn
	}{
		{
			name: "FeePaid",
			run: func(ctx context.Context, a *LedgerActor,
				amt int64) error {

				return run(
					ctx, a, &FeePaidMsg{
						RoundID:   [16]byte{1},
						AmountSat: amt,
						FeeType:   FeeTypeBoarding,
					},
				)
			},
		},
		{
			name: "VTXOReceived",
			run: func(ctx context.Context, a *LedgerActor,
				amt int64) error {

				return run(
					ctx, a, &VTXOReceivedMsg{
						OutpointHash: [32]byte{1},
						AmountSat:    amt,
						Source:       SourceOOR,
					},
				)
			},
		},
		{
			name: "VTXOSent",
			run: func(ctx context.Context, a *LedgerActor,
				amt int64) error {

				return run(
					ctx, a, &VTXOSentMsg{
						SessionID: [32]byte{1},
						AmountSat: amt,
					},
				)
			},
		},
	}

	amounts := []int64{0, -1, -1_000}

	for _, tc := range cases {
		for _, amt := range amounts {
			name := fmt.Sprintf("%s amount=%d", tc.name, amt)
			t.Run(name, func(t *testing.T) {
				a, store := newTestActor(t)
				err := tc.run(t.Context(), a, amt)

				require.Error(t, err)
				require.ErrorIs(t, err, ErrInvalidMessage)
				require.Empty(
					t, store.getEntries(),
					"no entry should be written on "+
						"invalid amount",
				)
			})
		}
	}
}

// TestDecodeAmountSatOverflow exercises the int64 narrowing
// guard on the TLV Decode path. A corrupt payload whose satoshi
// field exceeds math.MaxInt64 must surface as ErrInvalidMessage
// rather than silently producing a negative int64 that the
// handler (or the SQL CHECK) would later reject with a less
// actionable error. The single-case structure here keeps the
// addressable temporaries local: tlv.MakePrimitiveRecord needs
// pointers to backing storage, and the test frame happily gives
// them stack lifetimes.
func TestDecodeAmountSatOverflow(t *testing.T) {
	t.Parallel()

	// Full 16-byte RoundID so the fixed-length guard accepts
	// it and the overflow guard is the next thing that fires.
	roundIDArr := [16]byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	}
	roundID := roundIDArr[:]
	over := uint64(math.MaxInt64) + 1
	feeType := []byte("boarding_fee")
	height := uint32(100)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			feePaidRoundIDType, &roundID,
		),
		tlv.MakePrimitiveRecord(
			feePaidAmountSatType, &over,
		),
		tlv.MakePrimitiveRecord(
			feePaidFeeTypeType, &feeType,
		),
		tlv.MakePrimitiveRecord(
			feePaidBlockHeightType, &height,
		),
	)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, stream.Encode(&buf))

	m := &FeePaidMsg{}
	err = m.Decode(&buf)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMessage)
	require.Contains(t, err.Error(), "exceeds int64 range")
}

func TestHandleFeePaidUnknownType(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &FeePaidMsg{
		RoundID: [16]byte{
			1,
			2,
			3,
		},
		AmountSat:   1500,
		FeeType:     "unknown_type",
		BlockHeight: 800_000,
	}

	err := run(ctx, a, msg)
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
	ctx := t.Context()

	msg := &VTXOReceivedMsg{
		OutpointHash: [32]byte{
			0xaa,
			0xbb,
		},
		OutpointIndex: 0,
		AmountSat:     50_000,
		Source:        "collaborative",
		RoundID: [16]byte{
			7,
			8,
			9,
		},
	}

	err := run(ctx, a, msg)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMessage)
	require.Contains(t, err.Error(), "unknown vtxo source")

	// No entry should have been written.
	require.Empty(t, store.getEntries())
}

// TestHandleUTXOCreated verifies that a UTXO created event is
// recorded in both the wallet_utxo_log audit store AND the
// double-entry ledger. The ledger row books wallet_balance as an
// asset inflow sourced from opening_balance equity so subsequent
// SourceRoundBoarding entries have a non-negative wallet_balance
// to draw from.
func TestHandleUTXOCreated(t *testing.T) {
	t.Parallel()

	a, ledgerStore, auditStore := newTestActorWithAudit(t)
	ctx := t.Context()

	msg := &UTXOCreatedMsg{
		OutpointHash: [32]byte{
			0xaa,
			0xbb,
		},
		OutpointIndex:  0,
		AmountSat:      50_000,
		BlockHeight:    800_000,
		Classification: ClassificationDeposit,
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	// Audit-log side: the wallet_utxo_log row is still written.
	audit := auditStore.getEntries()
	require.Len(t, audit, 1)
	require.Equal(t, "created", audit[0].Event)
	require.Equal(t, "deposit", audit[0].ClassifiedAs)
	require.Equal(t, int64(50_000), audit[0].AmountSat)
	require.Equal(t, int32(800_000), audit[0].BlockHeight)

	// Double-entry side: debit wallet_balance, credit
	// opening_balance, stamped with the outpoint-derived
	// idempotency key so a replay is a silent no-op via
	// idx_client_ledger_idempotent_key.
	entries := ledgerStore.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountWalletBalance, entries[0].DebitAccount)
	require.Equal(
		t, AccountOpeningBalance, entries[0].CreditAccount,
	)
	require.Equal(t, int64(50_000), entries[0].AmountSat)
	require.Equal(
		t, EventWalletUTXOCreated, entries[0].EventType,
	)
	require.Equal(
		t, walletUTXOIdempotencyKey(
			msg.OutpointHash, msg.OutpointIndex,
		),
		entries[0].IdempotencyKey, "wallet UTXO ledger entry must "+
			"carry an outpoint-scoped idempotency key for "+
			"replay dedup",
	)
}

// TestHandleUTXOCreatedRejectsNonPositive locks in the validation
// guard: a zero or negative AmountSat on UTXOCreatedMsg is a
// malformed caller payload (impossible on-chain but reachable
// via a corrupt TLV). The handler must return ErrInvalidMessage
// and write nothing to either store, so a malformed durable
// message dead-letters cleanly instead of hitting the SQL
// CHECK (amount_sat > 0) and driving an infinite retry.
func TestHandleUTXOCreatedRejectsNonPositive(t *testing.T) {
	t.Parallel()

	a, ledgerStore, auditStore := newTestActorWithAudit(t)
	ctx := t.Context()

	for _, amt := range []int64{0, -1, -50_000} {
		err := run(
			ctx, a, &UTXOCreatedMsg{
				OutpointHash:   [32]byte{0xde},
				OutpointIndex:  0,
				AmountSat:      amt,
				BlockHeight:    800_000,
				Classification: ClassificationDeposit,
			},
		)
		require.ErrorIs(t, err, ErrInvalidMessage)
	}

	require.Empty(t, ledgerStore.getEntries())
	require.Empty(t, auditStore.getEntries())
}

// TestHandleUTXOSpent verifies that a UTXO spent event is
// recorded in the audit store with the correct fields.
func TestHandleUTXOSpent(t *testing.T) {
	t.Parallel()

	a, _, auditStore := newTestActorWithAudit(t)
	ctx := t.Context()

	msg := &UTXOSpentMsg{
		OutpointHash: [32]byte{
			0xcc,
			0xdd,
		},
		OutpointIndex:  1,
		AmountSat:      25_000,
		BlockHeight:    800_050,
		Classification: ClassificationRoundFunding,
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := auditStore.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "spent", entries[0].Event)
	require.Equal(t, "round_funding", entries[0].ClassifiedAs)
	require.Equal(t, int64(25_000), entries[0].AmountSat)
	require.Equal(t, int32(800_050), entries[0].BlockHeight)
}

// TestHandleUTXOSpentNonPositiveGuardScoped verifies that the
// non-positive amount guard only fires for the boarding-sweep-input
// classification, which is the sole branch that books a ledger leg
// (debit wallet_clearing) subject to the SQL CHECK (amount_sat > 0).
// Audit-only classifications must tolerate a zero amount (the
// wallet_utxo_log table has no positivity constraint), so they record
// the audit row instead of dead-lettering on a poison-pill TLV.
func TestHandleUTXOSpentNonPositiveGuardScoped(t *testing.T) {
	t.Parallel()

	t.Run("audit-only tolerates zero amount", func(t *testing.T) {
		t.Parallel()

		a, ledgerStore, auditStore := newTestActorWithAudit(t)

		err := run(t.Context(), a, &UTXOSpentMsg{
			OutpointHash:   [32]byte{0xab},
			OutpointIndex:  0,
			AmountSat:      0,
			BlockHeight:    800_000,
			Classification: ClassificationRoundFunding,
		})
		require.NoError(t, err)

		require.Len(t, auditStore.getEntries(), 1)
		require.Empty(
			t, ledgerStore.getEntries(),
			"audit-only spend must not book a ledger leg",
		)
	})

	t.Run("boarding sweep input rejects zero amount", func(t *testing.T) {
		t.Parallel()

		a, ledgerStore, auditStore := newTestActorWithAudit(t)

		err := run(t.Context(), a, &UTXOSpentMsg{
			OutpointHash:   [32]byte{0xcd},
			OutpointIndex:  0,
			AmountSat:      0,
			BlockHeight:    800_000,
			Classification: ClassificationBoardingSweepInput,
		})
		require.Error(t, err)
		require.ErrorIs(t, err, ErrInvalidMessage)

		require.Empty(
			t, auditStore.getEntries(),
			"rejected spend must not write an audit row",
		)
		require.Empty(t, ledgerStore.getEntries())
	})
}

// boardingSweepInput builds a SweepInput from a one-byte hash seed.
func boardingSweepInput(hashByte byte, index uint32, amt int64) SweepInput {
	return SweepInput{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				hashByte,
			},
			Index: index,
		},
		AmountSat: amt,
	}
}

// TestHandleBoardingSweepConfirmedNetsToZero verifies the consolidated
// boarding-sweep handler books the fee, per-input, and destination legs in
// one commit and that wallet_clearing nets to zero for both the
// wallet-return and external-destination paths. It also confirms the audit
// rows land alongside the balance legs.
func TestHandleBoardingSweepConfirmedNetsToZero(t *testing.T) {
	t.Parallel()

	const (
		in1       = int64(40_000)
		in2       = int64(60_000)
		total     = in1 + in2
		fee       = int64(444)
		anchor    = int64(330)
		chainCost = fee + anchor
		dest      = total - chainCost
	)

	cases := []struct {
		name         string
		external     bool
		wantAudit    int
		wantTransfer bool
	}{
		{
			name:      "wallet return",
			external:  false,
			wantAudit: 3, // two spent inputs + one created return
		},
		{
			name:         "external destination",
			external:     true,
			wantAudit:    2, // two spent inputs only
			wantTransfer: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, ledgerStore, auditStore := newTestActorWithAudit(t)

			msg := &BoardingSweepConfirmedMsg{
				Txid: [32]byte{
					0x7a,
				},
				BlockHeight:  800_800,
				ChainCostSat: chainCost,
				Inputs: []SweepInput{
					boardingSweepInput(0xa1, 0, in1),
					boardingSweepInput(0xb2, 1, in2),
				},
				DestinationSat:      dest,
				DestinationExternal: tc.external,
			}

			require.NoError(t, run(t.Context(), a, msg))

			balances := map[string]int64{}
			for _, e := range ledgerStore.getEntries() {
				balances[e.DebitAccount] += e.AmountSat
				balances[e.CreditAccount] -= e.AmountSat
			}

			require.Equal(
				t, int64(0), balances[AccountWalletClearing],
				"wallet_clearing must net to zero",
			)
			require.Equal(
				t, chainCost, balances[AccountOnchainFees],
				"onchain_fees debited by chain cost",
			)

			require.Len(t, auditStore.getEntries(), tc.wantAudit)

			if tc.wantTransfer {
				require.Equal(
					t, dest, balances[AccountTransfersOut],
					"external dest debits transfers_out",
				)
			}
		})
	}
}

// TestHandleBoardingSweepConfirmedRejectsInvalid locks in the up-front
// validation guards so a malformed message dead-letters cleanly rather than
// writing a partial leg set or hitting a SQL CHECK mid-commit.
func TestHandleBoardingSweepConfirmedRejectsInvalid(t *testing.T) {
	t.Parallel()

	base := func() *BoardingSweepConfirmedMsg {
		return &BoardingSweepConfirmedMsg{
			Txid: [32]byte{
				0x9c,
			},
			BlockHeight:  800_000,
			ChainCostSat: 774,
			Inputs: []SweepInput{
				boardingSweepInput(0xa1, 0, 100_000),
			},
			DestinationSat:      99_226,
			DestinationExternal: false,
		}
	}

	cases := []struct {
		name   string
		mutate func(*BoardingSweepConfirmedMsg)
	}{
		{
			name: "non-positive chain cost",
			mutate: func(m *BoardingSweepConfirmedMsg) {
				m.ChainCostSat = 0
			},
		},
		{
			name: "non-positive destination",
			mutate: func(m *BoardingSweepConfirmedMsg) {
				m.DestinationSat = -1
			},
		},
		{
			name: "no inputs",
			mutate: func(m *BoardingSweepConfirmedMsg) {
				m.Inputs = nil
			},
		},
		{
			name: "non-positive input amount",
			mutate: func(m *BoardingSweepConfirmedMsg) {
				m.Inputs = []SweepInput{
					boardingSweepInput(0xa1, 0, 0),
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, ledgerStore, auditStore := newTestActorWithAudit(t)

			msg := base()
			tc.mutate(msg)

			err := run(t.Context(), a, msg)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrInvalidMessage)

			require.Empty(t, ledgerStore.getEntries())
			require.Empty(t, auditStore.getEntries())
		})
	}
}

// TestBoardingRoundNetsToOpeningBalanceAndVTXO is a scenario-level
// test that walks the full boarding flow: a wallet UTXO confirms
// (booked as a deposit via handleUTXOCreated) and is then consumed
// by a round (booked via handleVTXOReceived with
// SourceRoundBoarding). It reconstructs the per-account running
// balance from the recorded legs and asserts the four invariants
// a boarding round must satisfy:
//   - wallet_balance nets to zero (deposit credit cancels round
//     debit).
//   - vtxo_balance rises by exactly the boarded amount.
//   - opening_balance rises by exactly the boarded amount
//     (representing the equity source of the funds).
//   - no other account is touched.
//
// The test leaves FeePaidMsg out so it can isolate the core
// deposit-boarding pairing. Fee rows are covered by dedicated fee
// handler tests.
func TestBoardingRoundNetsToOpeningBalanceAndVTXO(t *testing.T) {
	t.Parallel()

	a, ledgerStore, _ := newTestActorWithAudit(t)
	ctx := t.Context()

	const amount int64 = 80_000
	outpoint := [32]byte{0x11}
	roundID := [16]byte{0xaa, 0xbb}

	// Leg 1: wallet UTXO confirms.
	require.NoError(
		t,
		run(
			ctx, a, &UTXOCreatedMsg{
				OutpointHash:   outpoint,
				OutpointIndex:  3,
				AmountSat:      amount,
				BlockHeight:    800_000,
				Classification: ClassificationDeposit,
			},
		),
	)

	// Leg 2: same UTXO is spent into a round, producing an owned
	// VTXO with Source=SourceRoundBoarding.
	require.NoError(
		t,
		run(
			ctx, a, &VTXOReceivedMsg{
				OutpointHash:  [32]byte{0x22},
				OutpointIndex: 0,
				AmountSat:     amount,
				Source:        SourceRoundBoarding,
				RoundID:       roundID,
			},
		),
	)

	balances := map[string]int64{}
	for _, e := range ledgerStore.getEntries() {
		balances[e.DebitAccount] += e.AmountSat
		balances[e.CreditAccount] -= e.AmountSat
	}

	require.Equal(
		t, int64(0), balances[AccountWalletBalance],
		"deposit + boarding must cancel on wallet_balance",
	)
	require.Equal(
		t, amount, balances[AccountVTXOBalance],
		"vtxo_balance must rise by the boarded amount",
	)
	require.Equal(
		t, -amount, balances[AccountOpeningBalance], "opening_balanc"+
			"e credits rise by the boarded amount (negative "+
			"balance reflects equity-normal side)",
	)
	require.Equal(
		t, int64(0), balances[AccountTransfersIn],
		"boarding must not touch transfers_in",
	)
	require.Equal(
		t, int64(0), balances[AccountTransfersOut],
		"boarding must not touch transfers_out",
	)
	require.Equal(
		t, int64(0), balances[AccountFeesPaid],
		"isolated boarding pair should not include a fee leg",
	)
}

// TestHandleUTXOCreatedNoAuditStore verifies that UTXO created
// handling succeeds gracefully when no audit store is configured.
func TestHandleUTXOCreatedNoAuditStore(t *testing.T) {
	t.Parallel()

	a, _ := newTestActor(t)
	ctx := t.Context()

	msg := &UTXOCreatedMsg{
		OutpointHash: [32]byte{
			0xaa,
		},
		OutpointIndex:  0,
		AmountSat:      10_000,
		BlockHeight:    800_000,
		Classification: ClassificationDeposit,
	}

	// Should not error even without UTXOAuditStore.
	err := run(ctx, a, msg)
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
				RoundID: [16]byte{
					1,
					2,
					3,
				},
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
				OutpointHash: [32]byte{
					0xaa,
				},
				OutpointIndex: 42,
				AmountSat:     50_000,
				Source:        SourceOOR,
				RoundID: [16]byte{
					4,
					5,
					6,
				},
			},
			new: func() LedgerMsg {
				return &VTXOReceivedMsg{}
			},
		},
		{
			name: "VTXOSentOOR",
			msg: &VTXOSentMsg{
				SessionID: [32]byte{
					0xbb,
				},
				AmountSat: 10_000,
			},
			new: func() LedgerMsg {
				return &VTXOSentMsg{}
			},
		},
		{
			name: "VTXOSentInRound",
			msg: &VTXOSentMsg{
				RoundID: [16]byte{
					0xcc,
					0xdd,
				},
				AmountSat: 20_000,
			},
			new: func() LedgerMsg {
				return &VTXOSentMsg{}
			},
		},
		{
			name: "ExitCost",
			msg: &ExitCostMsg{
				OutpointHash: [32]byte{
					0xcc,
				},
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
				OutpointHash: [32]byte{
					0xdd,
				},
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
				OutpointHash: [32]byte{
					0xee,
				},
				OutpointIndex:  2,
				AmountSat:      45_000,
				BlockHeight:    800_300,
				Classification: ClassificationRoundFunding,
			},
			new: func() LedgerMsg {
				return &UTXOSpentMsg{}
			},
		},
		{
			name: "BoardingSweepConfirmedExternal",
			msg: &BoardingSweepConfirmedMsg{
				Txid: [32]byte{
					0xa1,
				},
				BlockHeight:  800_400,
				ChainCostSat: 774,
				Inputs: []SweepInput{
					{
						Outpoint: wire.OutPoint{
							Hash: chainhash.Hash{
								0xb2,
							},
							Index: 0,
						},
						AmountSat: 40_000,
					},
					{
						Outpoint: wire.OutPoint{
							Hash: chainhash.Hash{
								0xc3,
							},
							Index: 3,
						},
						AmountSat: 60_000,
					},
				},
				DestinationSat:      99_226,
				DestinationExternal: true,
			},
			new: func() LedgerMsg {
				return &BoardingSweepConfirmedMsg{}
			},
		},
		{
			name: "BoardingSweepConfirmedReturn",
			msg: &BoardingSweepConfirmedMsg{
				Txid: [32]byte{
					0xd4,
				},
				BlockHeight:  800_410,
				ChainCostSat: 500,
				Inputs: []SweepInput{
					{
						Outpoint: wire.OutPoint{
							Hash: chainhash.Hash{
								0xe5,
							},
							Index: 1,
						},
						AmountSat: 25_000,
					},
				},
				DestinationSat:      24_500,
				DestinationExternal: false,
			},
			new: func() LedgerMsg {
				return &BoardingSweepConfirmedMsg{}
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
