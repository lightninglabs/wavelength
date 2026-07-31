package lwwallet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// indexEsplora is a stub Esplora server backed by a synthetic chain of a
// configurable height, with a settable set of "used" script hashes and
// the confirmation heights their transactions landed at.
type indexEsplora struct {
	*httptest.Server

	mu sync.Mutex

	// tipHeight is the height the synthetic chain reports.
	tipHeight int32

	// usedScripts maps a script hash to the confirmation heights of
	// the transactions referencing it.
	usedScripts map[string][]int32

	// counts tallies requests per coarse route class.
	counts map[string]int

	// t is retained so handler-side failures surface as test errors.
	t *testing.T
}

// newIndexEsplora starts a stub Esplora index over a synthetic chain of
// the given height.
func newIndexEsplora(t *testing.T, tipHeight int32) *indexEsplora {
	t.Helper()

	e := &indexEsplora{
		t:           t,
		tipHeight:   tipHeight,
		usedScripts: make(map[string][]int32),
		counts:      make(map[string]int),
	}

	e.Server = httptest.NewServer(http.HandlerFunc(e.serve))
	t.Cleanup(e.Server.Close)

	return e
}

// markUsed records that the given pkScript was seen at the given
// confirmation heights.
func (e *indexEsplora) markUsed(pkScript []byte, heights ...int32) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.usedScripts[scriptHashHex(pkScript)] = heights
}

// count returns the number of requests seen for a route class.
func (e *indexEsplora) count(class string) int {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.counts[class]
}

// blockHashAt derives a deterministic, unique block hash for a height so
// the stub chain is self-consistent across endpoints.
func blockHashAt(height int32) string {
	return fmt.Sprintf("%064x", height+1)
}

// serve implements the subset of the Esplora API that start-block
// discovery and wallet startup exercise.
func (e *indexEsplora) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	e.mu.Lock()
	tip := e.tipHeight
	e.mu.Unlock()

	switch {
	case path == "/blocks/tip/height":
		e.tally("tip")

		_, err := fmt.Fprintf(w, "%d", tip)
		require.NoError(e.t, err)

	case strings.HasPrefix(path, "/block-height/"):
		e.tally("block-height")

		height, err := strconv.ParseInt(
			strings.TrimPrefix(path, "/block-height/"), 10, 32,
		)
		if err != nil || int32(height) > tip || height < 0 {
			http.NotFound(w, r)

			return
		}

		_, err = fmt.Fprint(w, blockHashAt(int32(height)))
		require.NoError(e.t, err)

	case strings.HasPrefix(path, "/scripthash/"):
		e.serveScriptHash(w, r, path)

	case strings.HasPrefix(path, "/block/"):
		e.tally("block-json")
		e.serveBlock(w, r, path)

	default:
		http.NotFound(w, r)
	}
}

// serveBlock answers /block/:hash with a minimal block document whose
// height is recovered from the deterministic hash encoding.
func (e *indexEsplora) serveBlock(w http.ResponseWriter, r *http.Request,
	path string) {

	rest := strings.TrimPrefix(path, "/block/")
	if strings.Contains(rest, "/") {
		http.NotFound(w, r)

		return
	}

	raw, err := strconv.ParseInt(strings.TrimLeft(rest, "0"), 16, 64)
	if err != nil {
		// A hash of all zeroes decodes to an empty string, which is
		// height 0 under this encoding.
		raw = 1
	}

	height := int32(raw) - 1

	err = json.NewEncoder(w).Encode(esploraBlock{
		ID:        rest,
		Height:    height,
		Timestamp: int64(1_600_000_000 + height),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// serveScriptHash answers both the stats probe and the paginated
// confirmed-history endpoint.
func (e *indexEsplora) serveScriptHash(w http.ResponseWriter, r *http.Request,
	path string) {

	rest := strings.TrimPrefix(path, "/scripthash/")
	parts := strings.SplitN(rest, "/", 2)
	hash := parts[0]

	e.mu.Lock()
	heights := e.usedScripts[hash]
	e.mu.Unlock()

	// The stats probe: /scripthash/:hash with nothing after it.
	if len(parts) == 1 {
		e.tally("scripthash-stats")

		err := json.NewEncoder(w).Encode(esploraScriptStats{
			ChainStats: esploraScriptStatSet{
				TxCount: len(heights),
			},
		})
		if err != nil {
			http.Error(
				w, err.Error(), http.StatusInternalServerError,
			)
		}

		return
	}

	// The history endpoint. Pagination is not simulated: the stub
	// returns the whole history on the first page and an empty page
	// for any continuation, which is the shape the caller loops on.
	if !strings.HasPrefix(parts[1], "txs/chain") {
		http.NotFound(w, r)

		return
	}

	e.tally("scripthash-txs")

	txs := []esploraScriptTx{}
	if parts[1] == "txs/chain" {
		for i, h := range heights {
			txs = append(txs, esploraScriptTx{
				Txid: fmt.Sprintf("%064x", i+1),
				Status: esploraStatus{
					Confirmed:   true,
					BlockHeight: int64(h),
				},
			})
		}
	}

	if err := json.NewEncoder(w).Encode(txs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// tally records one request against a route class.
func (e *indexEsplora) tally(class string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.counts[class]++
}

// newIndexTestWallet builds an unstarted wallet against the stub index,
// with its tip poller running so start-block resolution can read a tip.
func newIndexTestWallet(t *testing.T, esplora *indexEsplora,
	recoveryWindow uint32) *Wallet {

	t.Helper()

	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 3)
	}

	w, err := New(Config{
		Seed:           seed[:],
		WalletPassword: []byte("start-block-password"),
		EsploraURL:     esplora.URL,
		ChainParams:    &chaincfg.RegressionNetParams,
		PollInterval:   time.Hour,
		RecoveryWindow: recoveryWindow,
		DBDir:          t.TempDir(),
		Log:            fn.None[btclog.Logger](),
	})
	require.NoError(t, err)

	require.NoError(t, w.tipPoller.Start())
	t.Cleanup(w.tipPoller.Stop)

	t.Cleanup(func() {
		err := w.BtcWallet.InternalWallet().Database().Close()
		require.NoError(t, err)
	})

	return w
}

// readStamp returns the wallet's persisted synced-to stamp and birthday
// block verification flag.
func readStamp(t *testing.T, w *Wallet) (waddrmgr.BlockStamp, bool) {
	t.Helper()

	btcw := w.BtcWallet.InternalWallet()

	var (
		block    waddrmgr.BlockStamp
		verified bool
	)
	err := walletdb.View(btcw.Database(), func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)

		var err error
		block, verified, err = btcw.AddrManager().BirthdayBlock(ns)

		return err
	})
	require.NoError(t, err)

	return block, verified
}

// TestStampStartBlockFreshWallet checks that a wallet the index has
// never seen is stamped just below the tip rather than at genesis, and
// that the stamp is marked verified so btcwallet's timestamp search is
// suppressed.
func TestStampStartBlockFreshWallet(t *testing.T) {
	t.Parallel()

	const tipHeight = 250_000

	esplora := newIndexEsplora(t, tipHeight)
	w := newIndexTestWallet(t, esplora, 20)

	require.NoError(t, w.stampStartBlock(t.Context()))

	block, verified := readStamp(t, w)
	require.True(t, verified, "birthday block must be marked verified")
	require.Equal(
		t, int32(tipHeight)-startBlockReorgMargin, block.Height,
		"a wallet with no history should start just below the tip",
	)

	require.Equal(
		t, block, w.BtcWallet.InternalWallet().SyncedTo(),
		"synced-to must match the stamped birthday block",
	)

	// Discovery cost must not scale with the chain. The tip poller
	// seeds its reorg hash cache with DefaultHashHistorySize heights
	// on Start regardless of any of this, so the bound allows for
	// that plus a small constant, and still fails loudly if anything
	// starts walking the 250k block chain.
	require.Less(
		t, esplora.count("block-height"), DefaultHashHistorySize+10,
		"block metadata fetches must be bounded by the tip "+
			"poller's history seed, not by the chain height",
	)

	// Nothing was found, so no script history was ever fetched.
	require.Zero(t, esplora.count("scripthash-txs"))
}

// TestStampStartBlockFindsHistory checks that a used address moves the
// stamp back to one block below its earliest confirmation, since
// btcwallet's recovery resumes at synced-to plus one and would
// otherwise skip the funding block.
func TestStampStartBlockFindsHistory(t *testing.T) {
	t.Parallel()

	const (
		tipHeight   = 250_000
		fundedAt    = 120_000
		laterSpend  = 180_000
		gapLimit    = 20
		usedAddrIdx = 3
	)

	esplora := newIndexEsplora(t, tipHeight)
	w := newIndexTestWallet(t, esplora, gapLimit)

	// Mark one receive address of the wallet's default taproot scope
	// as used, with its earliest appearance well below the tip.
	scripts, err := w.deriveBranchScripts(scopeBranch{
		scope:  waddrmgr.KeyScopeBIP0086,
		branch: externalBranch,
	}, 0, gapLimit)
	require.NoError(t, err)

	esplora.markUsed(scripts[usedAddrIdx], laterSpend, fundedAt)

	require.NoError(t, w.stampStartBlock(t.Context()))

	block, verified := readStamp(t, w)
	require.True(t, verified)
	require.Equal(
		t, int32(fundedAt-1), block.Height,
		"stamp must sit one block below the earliest appearance",
	)
}

// TestStampStartBlockRespectsGapLimit checks that an address beyond the
// configured look-ahead is not discovered, matching the bound
// btcwallet's own recovery applies, while one just inside it is.
func TestStampStartBlockRespectsGapLimit(t *testing.T) {
	t.Parallel()

	const (
		tipHeight = 250_000
		fundedAt  = 90_000
		gapLimit  = 20
	)

	branch := scopeBranch{
		scope:  waddrmgr.KeyScopeBIP0086,
		branch: externalBranch,
	}

	// An address past the gap limit is invisible, so the wallet is
	// stamped as though it had no history.
	beyond := newIndexEsplora(t, tipHeight)
	wBeyond := newIndexTestWallet(t, beyond, gapLimit)

	scripts, err := wBeyond.deriveBranchScripts(branch, 0, gapLimit+5)
	require.NoError(t, err)

	beyond.markUsed(scripts[gapLimit+2], fundedAt)

	require.NoError(t, wBeyond.stampStartBlock(t.Context()))

	block, _ := readStamp(t, wBeyond)
	require.Equal(t, int32(tipHeight)-startBlockReorgMargin, block.Height)

	// The last address inside the window is found.
	within := newIndexEsplora(t, tipHeight)
	wWithin := newIndexTestWallet(t, within, gapLimit)

	scripts, err = wWithin.deriveBranchScripts(branch, 0, gapLimit)
	require.NoError(t, err)

	within.markUsed(scripts[gapLimit-1], fundedAt)

	require.NoError(t, wWithin.stampStartBlock(t.Context()))

	block, _ = readStamp(t, wWithin)
	require.Equal(t, int32(fundedAt-1), block.Height)
}

// TestStampStartBlockChangeOnlyScope checks that change addresses are
// still discovered once any receive address has been used, including
// when the change lives in a different key scope than the receive.
func TestStampStartBlockChangeOnlyScope(t *testing.T) {
	t.Parallel()

	const (
		tipHeight  = 250_000
		receivedAt = 200_000
		changeAt   = 150_000
		gapLimit   = 20
	)

	esplora := newIndexEsplora(t, tipHeight)
	w := newIndexTestWallet(t, esplora, gapLimit)

	recv, err := w.deriveBranchScripts(scopeBranch{
		scope:  waddrmgr.KeyScopeBIP0086,
		branch: externalBranch,
	}, 0, gapLimit)
	require.NoError(t, err)

	change, err := w.deriveBranchScripts(scopeBranch{
		scope:  waddrmgr.KeyScopeBIP0084,
		branch: internalBranch,
	}, 0, gapLimit)
	require.NoError(t, err)

	esplora.markUsed(recv[0], receivedAt)
	esplora.markUsed(change[1], changeAt)

	require.NoError(t, w.stampStartBlock(t.Context()))

	block, _ := readStamp(t, w)
	require.Equal(
		t, int32(changeAt-1), block.Height,
		"the earliest height across both branches must win",
	)
}

// TestStampStartBlockSkipsWhenAlreadySet checks that a wallet which
// already carries a birthday block is left alone: once btcwallet owns
// the sync cursor, overwriting it could move the wallet across blocks
// it has already processed.
func TestStampStartBlockSkipsWhenAlreadySet(t *testing.T) {
	t.Parallel()

	const tipHeight = 250_000

	esplora := newIndexEsplora(t, tipHeight)
	w := newIndexTestWallet(t, esplora, 20)

	require.NoError(t, w.stampStartBlock(t.Context()))

	first, _ := readStamp(t, w)

	// Grow the chain and re-run: the stamp must not move.
	esplora.mu.Lock()
	esplora.tipHeight = tipHeight + 5_000
	esplora.mu.Unlock()

	require.NoError(t, w.stampStartBlock(t.Context()))

	second, _ := readStamp(t, w)
	require.Equal(t, first, second)
}

// TestStampStartBlockIndexFailure checks that an unreachable index is
// not fatal. Discovery reports the error, the caller logs it, and
// btcwallet falls back to its own birthday search.
func TestStampStartBlockIndexFailure(t *testing.T) {
	t.Parallel()

	esplora := newIndexEsplora(t, 1_000)
	w := newIndexTestWallet(t, esplora, 20)

	// Close the index out from under the wallet so every probe fails.
	esplora.Server.Close()

	require.Error(t, w.stampStartBlock(t.Context()))

	// A failed discovery must leave the birthday block unset so
	// btcwallet still resolves a start block itself.
	requireUnstamped(t, w)
}

// TestDiscoveryCoversWalletAddresses checks the assumption the whole
// fix rests on: that the scripts start-block discovery probes are the
// same ones the wallet actually hands out. If NewAddress derives from a
// scope or account discovery does not walk, a restore would be stamped
// past its own funds.
func TestDiscoveryCoversWalletAddresses(t *testing.T) {
	t.Parallel()

	const gapLimit = 20

	esplora := newIndexEsplora(t, 1_000)
	w := newIndexTestWallet(t, esplora, gapLimit)

	// Gather the probe set BEFORE starting btcwallet, because that is
	// when discovery runs in production. Enumerating scopes after Start
	// would include any scope Start itself registers, which is exactly
	// the drift this test exists to catch.
	mgr := w.BtcWallet.InternalWallet().AddrManager()

	var probed [][]byte
	for _, scoped := range mgr.ActiveScopedKeyManagers() {
		for _, branch := range []uint32{
			externalBranch,
			internalBranch,
		} {
			scripts, dErr := w.deriveBranchScripts(scopeBranch{
				scope:  scoped.Scope(),
				branch: branch,
			}, 0, gapLimit)
			require.NoError(t, dErr)

			probed = append(probed, scripts...)
		}

		t.Logf("discovery walks scope %v", scoped.Scope())
	}

	require.NoError(t, w.BtcWallet.Start())
	t.Cleanup(func() { _ = w.BtcWallet.Stop() })

	addr, err := w.NewAddress(t.Context())
	require.NoError(t, err)

	want, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)

	t.Logf("NewAddress handed out %s", addr.String())

	found := false
	for _, s := range probed {
		if bytes.Equal(s, want) {
			found = true

			break
		}
	}

	require.True(
		t, found, "NewAddress produced a script outside every "+
			"scope and branch discovery probes, so a restore "+
			"would miss its funds",
	)
}

// TestStampStartBlockChangeOnlyWallet covers a wallet whose only
// on-chain history sits on a change branch, with every receive branch
// untouched. An aezeed seed is lnd-compatible, and an lnd wallet whose
// channels were opened from externally funded PSBTs holds its coins in
// force-close sweeps, which land on change addresses. Discovery must
// still find that history rather than stamping the wallet at the tip.
func TestStampStartBlockChangeOnlyWallet(t *testing.T) {
	t.Parallel()

	const (
		tipHeight = 250_000
		sweptAt   = 90_000
		gapLimit  = 20
	)

	esplora := newIndexEsplora(t, tipHeight)
	w := newIndexTestWallet(t, esplora, gapLimit)

	change, err := w.deriveBranchScripts(scopeBranch{
		scope:  waddrmgr.KeyScopeBIP0086,
		branch: internalBranch,
	}, 0, gapLimit)
	require.NoError(t, err)

	esplora.markUsed(change[2], sweptAt)

	require.NoError(t, w.stampStartBlock(t.Context()))

	block, _ := readStamp(t, w)
	require.Equal(
		t, int32(sweptAt-1), block.Height, "a wallet funded only "+
			"on a change branch must still be stamped below "+
			"its funds",
	)
}

// TestStampStartBlockRefusesWideRecoveryWindow checks that a recovery
// window wider than one branch walk's index budget is refused outright.
// Quietly narrowing it would let discovery report no history for a
// wallet whose first used address sits beyond our reach but inside the
// horizon btcwallet's own recovery would expand to, and the resulting
// stamp would sit above the wallet's funds.
func TestStampStartBlockRefusesWideRecoveryWindow(t *testing.T) {
	t.Parallel()

	esplora := newIndexEsplora(t, 250_000)
	w := newIndexTestWallet(t, esplora, 40)
	w.maxDiscoveryIndex = 20

	err := w.stampStartBlock(t.Context())
	require.ErrorIs(t, err, errGapLimitTooWide)

	// Refusing must leave the birthday block unset so btcwallet still
	// resolves a start block itself.
	requireUnstamped(t, w)
}

// TestStampStartBlockRefusesTruncatedWalk checks that a branch walk
// which runs out of index budget before establishing a full run of
// unused addresses abandons the whole attempt. A short walk finds fewer
// used scripts, which raises the minimum height discovery reports and
// moves the stamp toward the tip, so a truncated result must not be
// trusted.
func TestStampStartBlockRefusesTruncatedWalk(t *testing.T) {
	t.Parallel()

	const (
		gapLimit = 10
		budget   = 40
	)

	esplora := newIndexEsplora(t, 250_000)
	w := newIndexTestWallet(t, esplora, gapLimit)
	w.maxDiscoveryIndex = budget

	// Mark every address inside the budget as used, so no run of
	// gapLimit consecutive unused addresses can ever form.
	scripts, err := w.deriveBranchScripts(scopeBranch{
		scope:  waddrmgr.KeyScopeBIP0049Plus,
		branch: externalBranch,
	}, 0, budget)
	require.NoError(t, err)

	for i, pkScript := range scripts {
		esplora.markUsed(pkScript, int32(100_000+i))
	}

	err = w.stampStartBlock(t.Context())
	require.ErrorIs(t, err, errDiscoveryCapped)

	requireUnstamped(t, w)
}

// requireUnstamped asserts the wallet carries no birthday block, which
// is what leaves btcwallet free to resolve a start block itself.
func requireUnstamped(t *testing.T, w *Wallet) {
	t.Helper()

	btcw := w.BtcWallet.InternalWallet()

	var readErr error
	err := walletdb.View(btcw.Database(), func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		_, _, readErr = btcw.AddrManager().BirthdayBlock(ns)

		return nil
	})
	require.NoError(t, err)

	require.True(
		t, waddrmgr.IsError(readErr, waddrmgr.ErrBirthdayBlockNotSet),
		"a refused discovery must leave the birthday block unset",
	)
}

// TestStampStartBlockRefusesRealWideWindow exercises the wide-window
// refusal against the production index budget rather than a shrunken
// test value, so the shipped threshold itself is covered. The refusal
// lands before any address is derived, so this costs nothing.
func TestStampStartBlockRefusesRealWideWindow(t *testing.T) {
	t.Parallel()

	esplora := newIndexEsplora(t, 250_000)
	w := newIndexTestWallet(t, esplora, defaultMaxDiscoveryIndex+1)

	require.Equal(
		t, defaultMaxDiscoveryIndex, w.maxDiscoveryIndex,
		"this test must run against the production budget",
	)

	err := w.stampStartBlock(t.Context())
	require.ErrorIs(t, err, errGapLimitTooWide)

	requireUnstamped(t, w)

	// Nothing should have been probed: the guard runs before any
	// derivation or index traffic.
	require.Zero(t, esplora.count("scripthash-stats"))
}
