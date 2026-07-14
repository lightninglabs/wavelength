//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// newStoreListFixture wires a history reader over a real in-memory activity
// store so the store-backed List read path can be exercised end to end.
func newStoreListFixture(t *testing.T) (*history,
	*db.ActivityPersistenceStore) {

	t.Helper()

	testDB := db.NewTestDB(t)
	store := db.NewStore(
		testDB.DB, testDB.Queries, testDB.Backend(), btclog.Disabled,
	).NewActivityStore(clock.NewDefaultClock())

	deps := &Deps{ActivityStore: store}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	return newHistory(deps, runtime), store
}

// TestListActivityPendingBoardingUsesStableDepositID verifies the live
// zero-conf overlay adopts the same address-scoped id that Deposit returns and
// the confirmed ledger projection later uses.
func TestListActivityPendingBoardingUsesStableDepositID(t *testing.T) {
	t.Parallel()

	h, store := newStoreListFixture(t)
	_, pubKey := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x01}, 32))
	address, err := btcaddr.NewAddressTaproot(
		schnorr.SerializePubKey(pubKey), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	pkScript, err := txscript.PayToAddrScript(address)
	require.NoError(t, err)

	h.deps.ChainParams = &chaincfg.RegressionNetParams
	rpc := &fakeRPCServer{
		getBalanceResp: &waverpc.GetBalanceResponse{
			BoardingUnconfirmedSat: 12_345,
		},
		activeBoardingAddrs: []string{
			address.String(),
		},
		listWalletUnspent: []*wallet.Utxo{{
			PkScript:      pkScript,
			Amount:        btcutil.Amount(12_345),
			Confirmations: 0,
		}},
	}
	h.deps.RPCServer = rpc

	page, err := h.listActivity(t.Context(), &walletdkrpc.ListRequest{
		Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, page.GetEntries(), 1)
	entry := page.GetEntries()[0]
	require.Equal(t, "deposit-"+address.String(), entry.GetId())
	require.Equal(
		t, address.String(),
		entry.GetRequest().GetOnchainAddress().GetAddress(),
	)

	// Once confirmation removes the live zero-conf balance, the durable
	// ledger projection takes over under exactly the same canonical id.
	rpc.getBalanceResp.BoardingUnconfirmedSat = 0
	stableID := "deposit-" + address.String()
	seedActivity(
		t, store, stableID, walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING, 123,
	)
	confirmed, err := h.listActivity(
		t.Context(), &walletdkrpc.ListRequest{
			Limit: 10,
		},
	)
	require.NoError(t, err)
	require.Equal(t, []string{stableID}, activityIDs(confirmed))
}

// seedActivity projects one entry into the store via the production mapping.
func seedActivity(t *testing.T, store *db.ActivityPersistenceStore, id string,
	kind walletdkrpc.EntryKind, status walletdkrpc.EntryStatus,
	created int64) {

	t.Helper()

	proj, err := entryToProjection(&walletdkrpc.WalletEntry{
		Id:            id,
		Kind:          kind,
		Status:        status,
		AmountSat:     1000,
		CreatedAtUnix: created,
		UpdatedAtUnix: created,
	})
	require.NoError(t, err)

	_, err = store.ProjectEntry(context.Background(), proj)
	require.NoError(t, err)
}

func activityIDs(list *walletdkrpc.ActivityList) []string {
	out := make([]string, 0, len(list.GetEntries()))
	for _, e := range list.GetEntries() {
		out = append(out, e.GetId())
	}

	return out
}

// TestListActivityReadsStore verifies List pages the store newest-first and
// resumes via next_cursor with a correct has_more.
func TestListActivityReadsStore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, store := newStoreListFixture(t)

	seedActivity(
		t, store, "a", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 100,
	)
	seedActivity(
		t, store, "b", walletdkrpc.EntryKind_ENTRY_KIND_RECV,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 200,
	)
	seedActivity(
		t, store, "c", walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 300,
	)

	page1, err := h.listActivity(ctx, &walletdkrpc.ListRequest{Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []string{"c", "b"}, activityIDs(page1))
	require.True(t, page1.GetHasMore())
	require.NotEmpty(t, page1.GetNextCursor())

	page2, err := h.listActivity(ctx, &walletdkrpc.ListRequest{
		Limit:  2,
		Cursor: page1.GetNextCursor(),
	})
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, activityIDs(page2))
	require.False(t, page2.GetHasMore())
	require.Empty(t, page2.GetNextCursor())
}

// TestListActivityIncludesPendingBoardingWithStore verifies the canonical
// store read path overlays the live unconfirmed boarding deposit that is not
// itself persisted.
func TestListActivityIncludesPendingBoardingWithStore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, store := newStoreListFixture(t)
	h.deps.RPCServer = &fakeRPCServer{
		getBalanceResp: &waverpc.GetBalanceResponse{
			BoardingUnconfirmedSat: 12_345,
		},
	}

	seedActivity(
		t, store, "complete", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 100,
	)

	all, err := h.listActivity(ctx, &walletdkrpc.ListRequest{Limit: 10})
	require.NoError(t, err)
	require.Equal(
		t, []string{syntheticBoardingUnconfirmedID, "complete"},
		activityIDs(all),
	)
	pending := all.GetEntries()[0]
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT, pending.GetKind(),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		pending.GetStatus(),
	)
	require.Equal(t, int64(12_345), pending.GetAmountSat())
	require.Equal(
		t, walletdkrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_CONFIRMATION,
		pending.GetProgress().GetPhase(),
	)

	depositOnly, err := h.listActivity(ctx, &walletdkrpc.ListRequest{
		Limit:       10,
		PendingOnly: true,
		Kinds: []walletdkrpc.EntryKind{
			walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		},
	})
	require.NoError(t, err)
	require.Equal(
		t, []string{syntheticBoardingUnconfirmedID},
		activityIDs(depositOnly),
	)

	sendOnly, err := h.listActivity(ctx, &walletdkrpc.ListRequest{
		Limit: 10,
		Kinds: []walletdkrpc.EntryKind{
			walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"complete"}, activityIDs(sendOnly))
}

// TestListActivityPendingBoardingCursor verifies a page containing only the
// ephemeral boarding row resumes at the newest canonical store row without
// repeating the overlay.
func TestListActivityPendingBoardingCursor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, store := newStoreListFixture(t)
	h.deps.RPCServer = &fakeRPCServer{
		getBalanceResp: &waverpc.GetBalanceResponse{
			BoardingUnconfirmedSat: 12_345,
		},
	}

	seedActivity(
		t, store, "complete", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 100,
	)

	page1, err := h.listActivity(ctx, &walletdkrpc.ListRequest{Limit: 1})
	require.NoError(t, err)
	require.Equal(
		t, []string{syntheticBoardingUnconfirmedID}, activityIDs(page1),
	)
	require.True(t, page1.GetHasMore())
	require.NotEmpty(t, page1.GetNextCursor())

	page2, err := h.listActivity(ctx, &walletdkrpc.ListRequest{
		Limit:  1,
		Cursor: page1.GetNextCursor(),
	})
	require.NoError(t, err)
	require.Equal(t, []string{"complete"}, activityIDs(page2))
	require.False(t, page2.GetHasMore())
	require.Empty(t, page2.GetNextCursor())
}

// TestListActivityStableBoardingCursor verifies the address-scoped live row
// uses the reserved overlay cursor marker. A durable row inserted in the same
// second after page one must be returned on page two even when its id sorts
// before deposit-<address>.
func TestListActivityStableBoardingCursor(t *testing.T) {
	t.Parallel()

	h, store := newStoreListFixture(t)
	_, pubKey := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x02}, 32))
	address, err := btcaddr.NewAddressTaproot(
		schnorr.SerializePubKey(pubKey), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	pkScript, err := txscript.PayToAddrScript(address)
	require.NoError(t, err)

	h.deps.ChainParams = &chaincfg.RegressionNetParams
	h.deps.RPCServer = &fakeRPCServer{
		getBalanceResp: &waverpc.GetBalanceResponse{
			BoardingUnconfirmedSat: 12_345,
		},
		activeBoardingAddrs: []string{
			address.String(),
		},
		listWalletUnspent: []*wallet.Utxo{{
			PkScript: pkScript, Amount: 12_345, Confirmations: 0,
		}},
	}
	seedActivity(
		t, store, "older", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 100,
	)

	page1, err := h.listActivity(
		t.Context(), &walletdkrpc.ListRequest{
			Limit: 1,
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, []string{"deposit-" + address.String()}, activityIDs(page1),
	)
	require.True(t, page1.GetHasMore())
	_, cursorID, err := decodeActivityCursor(page1.GetNextCursor())
	require.NoError(t, err)
	require.Equal(t, syntheticBoardingUnconfirmedID, cursorID)

	seedActivity(
		t, store, "aaa", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		page1.GetEntries()[0].GetCreatedAtUnix(),
	)
	page2, err := h.listActivity(t.Context(), &walletdkrpc.ListRequest{
		Limit:  1,
		Cursor: page1.GetNextCursor(),
	})
	require.NoError(t, err)
	require.Equal(t, []string{"aaa"}, activityIDs(page2))
}

// TestListActivityOverlayFailureReturnsStore verifies a transient live-daemon
// failure does not make the canonical cached activity feed unavailable.
func TestListActivityOverlayFailureReturnsStore(t *testing.T) {
	t.Parallel()

	h, store := newStoreListFixture(t)
	h.deps.RPCServer = &fakeRPCServer{
		getBalanceErr: context.DeadlineExceeded,
	}
	seedActivity(
		t, store, "cached", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 100,
	)

	page, err := h.listActivity(
		t.Context(), &walletdkrpc.ListRequest{
			Limit: 10,
		},
	)
	require.NoError(t, err)
	require.Equal(t, []string{"cached"}, activityIDs(page))
}

// TestCountPendingOverlayFailureReturnsStoreCount verifies wallet status keeps
// its durable pending count when the optional live overlay cannot be read.
func TestCountPendingOverlayFailureReturnsStoreCount(t *testing.T) {
	t.Parallel()

	h, store := newStoreListFixture(t)
	h.deps.RPCServer = &fakeRPCServer{
		getBalanceErr: context.DeadlineExceeded,
	}
	seedActivity(
		t, store, "pending", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING, 100,
	)

	count, err := h.countPending(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
}

// TestCountPendingReflectsFullFeed verifies countPending returns the full
// number of pending rows rather than the single-page total the paginated read
// path reports. This is the store-backed count behind the wallet status
// summary's pending count.
func TestCountPendingReflectsFullFeed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, store := newStoreListFixture(t)
	h.deps.RPCServer = &fakeRPCServer{
		getBalanceResp: &waverpc.GetBalanceResponse{
			BoardingUnconfirmedSat: 12_345,
		},
	}

	// Three pending rows plus one terminal row.
	seedActivity(
		t, store, "p1", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING, 100,
	)
	seedActivity(
		t, store, "p2", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING, 200,
	)
	seedActivity(
		t, store, "p3", walletdkrpc.EntryKind_ENTRY_KIND_RECV,
		walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING, 300,
	)
	seedActivity(
		t, store, "done", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 400,
	)

	// A single-page pending read caps its total at the page size, so it
	// cannot stand in for the pending count.
	page, err := h.listActivity(ctx, &walletdkrpc.ListRequest{
		Limit:       1,
		PendingOnly: true,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, page.GetTotal())
	require.True(t, page.GetHasMore())

	// countPending reports every durable pending row plus the live
	// unconfirmed boarding deposit, regardless of page size.
	count, err := h.countPending(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 4, count)
}

// TestListActivityStablePaginationUnderInsert verifies the #781 acceptance
// criterion: a row inserted between page fetches never causes an existing row
// to be skipped or duplicated, because the cursor is an immutable keyset.
func TestListActivityStablePaginationUnderInsert(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, store := newStoreListFixture(t)

	seedActivity(
		t, store, "a", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 100,
	)
	seedActivity(
		t, store, "b", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 200,
	)
	seedActivity(
		t, store, "c", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 300,
	)

	page1, err := h.listActivity(ctx, &walletdkrpc.ListRequest{Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []string{"c", "b"}, activityIDs(page1))

	// A new op lands between page fetches, newer than the page-1 cursor.
	seedActivity(
		t, store, "d", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 250,
	)

	page2, err := h.listActivity(ctx, &walletdkrpc.ListRequest{
		Limit:  2,
		Cursor: page1.GetNextCursor(),
	})
	require.NoError(t, err)

	// Page 2 continues strictly older than the cursor: "a" is returned
	// once, "b"/"c" are not duplicated, and the newer "d" is simply above
	// this pagination pass (a fresh read would surface it at the top).
	require.Equal(t, []string{"a"}, activityIDs(page2))
}

// TestListActivityFilters verifies pending_only and kind filters apply over the
// store keyset scan.
func TestListActivityFilters(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, store := newStoreListFixture(t)

	seedActivity(
		t, store, "send", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING, 100,
	)
	seedActivity(
		t, store, "recv", walletdkrpc.EntryKind_ENTRY_KIND_RECV,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 200,
	)
	seedActivity(
		t, store, "exit", walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
		walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING, 300,
	)

	pending, err := h.listActivity(ctx, &walletdkrpc.ListRequest{
		Limit:       10,
		PendingOnly: true,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"exit", "send"}, activityIDs(pending))

	recvOnly, err := h.listActivity(ctx, &walletdkrpc.ListRequest{
		Limit: 10,
		Kinds: []walletdkrpc.EntryKind{
			walletdkrpc.EntryKind_ENTRY_KIND_RECV,
		},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"recv"}, activityIDs(recvOnly))
}

// TestListActivityRejectsBadCursor verifies a malformed cursor is a clean
// error.
func TestListActivityRejectsBadCursor(t *testing.T) {
	t.Parallel()

	h, _ := newStoreListFixture(t)

	_, err := h.listActivity(context.Background(), &walletdkrpc.ListRequest{
		Cursor: "!!!not-base64!!!",
	})
	require.ErrorIs(t, err, errInvalidActivityCursor)
}

// TestListActivityRejectsNonPositiveCursor verifies a cursor whose timestamp is
// zero or negative is rejected rather than silently colliding with the
// return-all sentinel and restarting paging from the newest row.
func TestListActivityRejectsNonPositiveCursor(t *testing.T) {
	t.Parallel()

	h, _ := newStoreListFixture(t)

	for _, created := range []int64{0, -1} {
		cursor := encodeActivityCursor(created, "x")
		_, err := h.listActivity(
			context.Background(), &walletdkrpc.ListRequest{
				Cursor: cursor,
			},
		)
		require.ErrorIs(t, err, errInvalidActivityCursor)
	}
}

// TestListActivityBoundsFilteredScan verifies a selective filter over a large
// non-matching table does not scan the whole table in one request: the call
// stops at the scan budget and returns an empty page plus a cursor to resume,
// instead of decoding every row (the H-2 amplification cliff).
func TestListActivityBoundsFilteredScan(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, store := newStoreListFixture(t)

	// Seed far more terminal rows than one page's scan budget
	// (limit * activityScanBudgetFactor), none of which match --pending.
	const rows = 60
	for i := 0; i < rows; i++ {
		seedActivity(
			t, store, fmt.Sprintf("c%02d", i),
			walletdkrpc.EntryKind_ENTRY_KIND_SEND,
			walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
			int64(100+i),
		)
	}

	page, err := h.listActivity(ctx, &walletdkrpc.ListRequest{
		Limit:       2,
		PendingOnly: true,
	})
	require.NoError(t, err)

	// The budget-bounded scan returns no matches but signals more work with
	// a resume cursor, rather than draining the table (which would report
	// has_more=false).
	require.Empty(t, page.GetEntries())
	require.True(t, page.GetHasMore())
	require.NotEmpty(t, page.GetNextCursor())
}

// TestRowToWalletEntryRoundTrip verifies a WalletEntry survives the
// project → store → row → rowToWalletEntry round trip unchanged.
func TestRowToWalletEntryRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	_, store := newStoreListFixture(t)

	entry := sampleWalletEntry()

	proj, err := entryToProjection(entry)
	require.NoError(t, err)

	_, err = store.ProjectEntry(ctx, proj)
	require.NoError(t, err)

	row, err := store.GetEntry(ctx, entry.GetId())
	require.NoError(t, err)

	got, err := rowToWalletEntry(row)
	require.NoError(t, err)
	require.True(
		t, proto.Equal(entry, got),
		"reconstructed entry must equal the original",
	)
}
