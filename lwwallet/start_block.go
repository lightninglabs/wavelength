package lwwallet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"golang.org/x/sync/errgroup"
)

// waddrmgrNamespaceKey is the walletdb namespace btcwallet stores its
// address manager under. btcwallet keeps its own copy of this constant
// unexported, so writing the wallet's start block ahead of
// btcwallet.Start requires naming the namespace here. The value is part
// of btcwallet's on-disk format and cannot change without a database
// migration, so duplicating it is safe.
var waddrmgrNamespaceKey = []byte("waddrmgr")

const (
	// externalBranch and internalBranch are the BIP44 address chains
	// under an account: receive addresses and change addresses.
	externalBranch uint32 = 0
	internalBranch uint32 = 1

	// startBlockReorgMargin is how far below the chain tip a wallet
	// with no discovered on-chain history starts scanning from. The
	// margin costs a handful of block fetches and buys tolerance for
	// a shallow reorg racing wallet creation, so a payment landing in
	// a block that is reorged out and re-mined is still seen.
	startBlockReorgMargin int32 = 6

	// defaultDiscoveryGapLimit is the address look-ahead used when the
	// wallet was configured with no recovery window. btcwallet skips
	// address discovery entirely in that mode, but the start block
	// still has to be resolved, and probing a short run of addresses
	// is what keeps a wallet with existing history from being stamped
	// at the tip.
	defaultDiscoveryGapLimit uint32 = 20

	// defaultMaxDiscoveryIndex bounds how far along a single address
	// branch discovery will walk before giving up. A branch this long
	// means either a wallet far outside the shapes this daemon creates
	// or an index feeding us nonsense; either way, falling back to
	// btcwallet's own birthday search is better than looping.
	defaultMaxDiscoveryIndex uint32 = 10_000

	// maxScriptTxPages bounds pagination over one script's confirmed
	// history. At Esplora's 25 entries per page this covers 5000
	// transactions against a single script.
	maxScriptTxPages = 200

	// scriptProbeConcurrency bounds how many index probes are in
	// flight at once. Esplora deployments are commonly rate limited,
	// and discovery is a startup burst rather than steady state, so
	// this stays deliberately modest.
	scriptProbeConcurrency = 8
)

// errHistoryTooLong is returned when one script's confirmed history
// exceeds maxScriptTxPages. Discovery cannot then name the earliest
// block that script appeared in, and guessing would risk stamping the
// wallet past its own funds, so the whole attempt is abandoned.
var errHistoryTooLong = errors.New("script history exceeds page limit")

// errDiscoveryCapped reports that one branch walk ran out of index
// budget before completing its run of unused addresses. Discovery
// cannot then claim to have honoured the gap limit, and a short walk
// finds fewer used scripts, which raises the minimum height it reports
// and moves the stamp toward the tip. That is the direction that loses
// funds, so the attempt is abandoned rather than trusted.
var errDiscoveryCapped = errors.New("branch walk hit the index cap")

// errGapLimitTooWide reports a recovery window wider than the index
// budget a single branch walk is allowed. Discovery has to look at
// least as far as btcwallet's own recovery will, so a window it cannot
// cover is refused outright instead of quietly narrowed.
var errGapLimitTooWide = errors.New("recovery window exceeds the discovery " +
	"index budget")

// scopeBranch names one address chain of one active key scope: the pair
// that a gap-limit walk runs over.
type scopeBranch struct {
	// scope is the key scope, e.g. BIP84 or BIP86.
	scope waddrmgr.KeyScope

	// branch is externalBranch or internalBranch.
	branch uint32
}

// stampStartBlock resolves where this wallet should begin scanning the
// chain and records it as btcwallet's synced-to height and verified
// birthday block.
//
// Without this, btcwallet resolves its own start block by binary
// searching block timestamps for the seed birthday
// (locateBirthdayBlock). That search has no lower bound below genesis,
// so any birthday predating the chain — an aezeed pinned to the Bitcoin
// genesis date, or a real seed older than signet itself — collapses to
// block 0, and the wallet then walks every block from genesis to the
// tip over Esplora. Recording a verified birthday block up front makes
// btcwallet's birthdaySanityCheck return it untouched and skip the
// search entirely.
//
// The height comes from the Esplora address index rather than from
// timestamps, which is the whole point: an indexing chain source can
// answer "where did this wallet first appear on chain" directly, at a
// cost set by the address gap limit instead of by the length of the
// chain.
//
// Only a wallet with no birthday block at all is stamped. Once
// btcwallet has recorded one, it owns the sync cursor and this is a
// no-op.
//
// This trades a full chain scan for trust in the index, and the trust
// is not free. A single self-consistent Esplora is fine: its tip
// endpoint reflects what it has indexed. A load-balanced public
// deployment is not, because the tip poll and a script probe can land
// on different backends, and one still catching up answers 200 with
// partial or empty history that is indistinguishable from "this script
// was never used". A restore during such a window stamps above its own
// funds, and because the stamp is written verified it is never
// revisited. Deployments that care about restore correctness should
// point EsploraURL at an endpoint they control.
func (w *Wallet) stampStartBlock(ctx context.Context) error {
	btcw := w.BtcWallet.InternalWallet()
	db := btcw.Database()
	mgr := btcw.AddrManager()

	// A birthday block that is already present is btcwallet's to
	// manage: it either came from a completed sync or from a
	// migration, and either way overwriting it could move the sync
	// cursor across blocks the wallet has already processed.
	var unstamped bool
	err := walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)

		_, _, err := mgr.BirthdayBlock(ns)
		switch {
		case waddrmgr.IsError(err, waddrmgr.ErrBirthdayBlockNotSet):
			unstamped = true

			return nil

		case err != nil:
			return err
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("read birthday block: %w", err)
	}

	if !unstamped {
		return nil
	}

	stamp, err := w.resolveStartBlock(ctx)
	if err != nil {
		return err
	}

	// Never rewind: btcwallet seeds a fresh wallet's synced-to at
	// genesis, so a start block at or below that carries no
	// information and writing it would only risk moving the cursor
	// backwards over already-processed blocks.
	if stamp.Height <= mgr.SyncedTo().Height {
		return nil
	}

	// Write order is load-bearing. waddrmgr.PutSyncedTo refuses a
	// height whose predecessor's hash is absent, but only once a
	// birthday block exists — the check is how it enforces contiguous
	// block storage after the initial sync. Recording synced-to while
	// FetchBirthdayBlock still errors sidesteps that check for this
	// one write; every later SetSyncedTo btcwallet makes builds on the
	// hash stored here.
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)

		if err := mgr.SetSyncedTo(ns, stamp); err != nil {
			return fmt.Errorf("set synced to: %w", err)
		}

		// Marking the block verified is what suppresses
		// btcwallet's timestamp binary search.
		return mgr.SetBirthdayBlock(ns, *stamp, true)
	})
	if err != nil {
		return fmt.Errorf("persist start block: %w", err)
	}

	w.Logger(ctx).InfoS(
		ctx,
		"Stamped wallet start block from Esplora index",
		slog.Int("height", int(stamp.Height)),
		slog.String("hash", stamp.Hash.String()),
	)

	return nil
}

// resolveStartBlock returns the block stamp the wallet should be marked
// synced to before btcwallet takes over. A wallet whose scripts the
// index has never seen starts just below the tip; one with history
// starts one block below its earliest appearance, because btcwallet's
// recovery resumes at synced-to plus one and would otherwise skip the
// very block being targeted.
func (w *Wallet) resolveStartBlock(ctx context.Context) (*waddrmgr.BlockStamp,
	error) {

	tipHeight, _, _ := w.tipPoller.BestBlock()

	startHeight := tipHeight - startBlockReorgMargin

	earliest, err := w.earliestActivityHeight(ctx)
	if err != nil {
		return nil, err
	}

	earliest.WhenSome(func(height int32) {
		if height-1 < startHeight {
			startHeight = height - 1
		}
	})

	if startHeight < 0 {
		startHeight = 0
	}

	hash, err := w.esplora.GetBlockHashByHeight(ctx, startHeight)
	if err != nil {
		return nil, fmt.Errorf("get start block hash at %d: %w",
			startHeight, err)
	}

	header, err := w.esplora.GetBlockHeader(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("get start block header %s: %w", hash,
			err)
	}

	return &waddrmgr.BlockStamp{
		Height:    startHeight,
		Hash:      hash,
		Timestamp: time.Unix(header.Timestamp, 0),
	}, nil
}

// earliestActivityHeight returns the lowest block height at which any
// script this wallet can derive has appeared on chain, or None when the
// index has seen none of them.
func (w *Wallet) earliestActivityHeight(ctx context.Context) (fn.Option[int32],
	error) {

	none := fn.None[int32]()

	used, err := w.usedScripts(ctx)
	if err != nil {
		return none, err
	}

	if len(used) == 0 {
		return none, nil
	}

	earliest := int32(math.MaxInt32)
	for _, pkScript := range used {
		height, err := w.earliestScriptHeight(ctx, pkScript)
		if err != nil {
			return none, err
		}

		height.WhenSome(func(h int32) {
			if h < earliest {
				earliest = h
			}
		})
	}

	// Every used script was seen only in the mempool, so there is no
	// confirmed history to anchor to.
	if earliest == math.MaxInt32 {
		return none, nil
	}

	w.Logger(ctx).InfoS(ctx, "Esplora index located wallet history",
		slog.Int("used_scripts", len(used)),
		slog.Int("earliest_height", int(earliest)),
	)

	return fn.Some(earliest), nil
}

// usedScripts walks every active key scope's receive and change branch
// under the default account and returns the pkScripts the index reports
// as used.
//
// Scopes come from btcwallet's own address manager rather than a list
// maintained here, so a wallet carrying a scope this build does not
// know about is still covered. Note that this runs before
// btcwallet.Start, which is when lnd's m/1017' chain scope is added;
// that scope holds keys used inside scripts rather than addresses paid
// to directly.
//
// Only HD-derived scripts are reachable this way. Imported taproot
// scripts, which is what boarding addresses are, encode a script tree
// rather than a derivation path and cannot be enumerated from the seed,
// so they are outside any gap-limit walk. That costs nothing here:
// BoardingBackendAdapter.ListUnspent enumerates boarding outputs by
// querying Esplora per imported address, so their visibility never
// depended on btcwallet's sync height in the first place.
// Both branches of every scope are walked. An earlier version skipped
// the change branches when no receive address anywhere had been used,
// on the reasoning that a change output requires a spend, which
// requires the wallet to have held an output, which requires something
// to have paid a receive address first. That bootstrap argument has a
// hole: a wallet-owned output need not come from a derived receive
// address at all. An aezeed seed is lnd-compatible, and an lnd wallet
// whose channels were opened from externally funded PSBTs holds its
// only on-chain coins in force-close sweeps, which lnd sends to change
// addresses. Such a seed has funds on internal branches with every
// external branch untouched, and skipping the change branches would
// stamp it at the tip.
func (w *Wallet) usedScripts(ctx context.Context) ([][]byte, error) {
	// Discovery has to look at least as far as btcwallet's recovery
	// will. A window wider than one branch walk's index budget cannot
	// be covered, and quietly narrowing it would let discovery report
	// "no history" for a wallet whose first used address sits beyond
	// our reach but inside btcwallet's, stamping above its funds.
	gapLimit := w.discoveryGapLimit()
	if gapLimit > w.maxDiscoveryIndex {
		return nil, fmt.Errorf("%w: window %d, budget %d",
			errGapLimitTooWide, gapLimit, w.maxDiscoveryIndex)
	}

	mgr := w.BtcWallet.InternalWallet().AddrManager()

	scopes := mgr.ActiveScopedKeyManagers()

	branches := make([]scopeBranch, 0, 2*len(scopes))
	for _, scoped := range scopes {
		branches = append(branches,
			scopeBranch{
				scope:  scoped.Scope(),
				branch: externalBranch,
			},
			scopeBranch{
				scope:  scoped.Scope(),
				branch: internalBranch,
			},
		)
	}

	return w.scanBranches(ctx, branches)
}

// scanBranches runs a gap-limit walk over each of the given branches and
// returns every used pkScript found across them.
func (w *Wallet) scanBranches(ctx context.Context, branches []scopeBranch) (
	[][]byte, error) {

	var used [][]byte
	for _, sb := range branches {
		found, err := w.usedBranchScripts(ctx, sb)
		if err != nil {
			return nil, fmt.Errorf("scan scope %v branch %d: %w",
				sb.scope, sb.branch, err)
		}

		used = append(used, found...)
	}

	return used, nil
}

// usedBranchScripts walks one address branch until it has seen a run of
// gapLimit consecutive unused addresses, mirroring the look-ahead rule
// btcwallet's own recovery applies, and returns the used pkScripts it
// passed along the way.
func (w *Wallet) usedBranchScripts(ctx context.Context, sb scopeBranch) (
	[][]byte, error) {

	gapLimit := w.discoveryGapLimit()

	var (
		used      [][]byte
		index     uint32
		unusedRun uint32
	)

	for unusedRun < gapLimit && index < w.maxDiscoveryIndex {
		// The batch size is what actually bounds the allocation and
		// the request burst here, not the loop guard: a whole batch
		// is derived and probed before the guard is consulted again.
		count := min(gapLimit, w.maxDiscoveryIndex-index)

		scripts, err := w.deriveBranchScripts(sb, index, count)
		if err != nil {
			return nil, err
		}

		flags, err := w.probeScripts(ctx, scripts)
		if err != nil {
			return nil, err
		}

		for i, isUsed := range flags {
			if isUsed {
				used = append(used, scripts[i])
				unusedRun = 0

				continue
			}

			unusedRun++
			if unusedRun >= gapLimit {
				break
			}
		}

		index += count
	}

	// Exiting on the index budget rather than on a full run of unused
	// addresses means the walk never established a gap limit's worth of
	// silence, so the used set it collected may be short. A short set
	// raises the minimum height discovery reports, which moves the
	// stamp up rather than down, so this cannot be treated as a
	// successful walk.
	if unusedRun < gapLimit {
		return nil, fmt.Errorf("%w at index %d", errDiscoveryCapped,
			index)
	}

	return used, nil
}

// discoveryGapLimit returns the address look-ahead discovery walks with.
// It tracks the wallet's configured recovery window so discovery and
// btcwallet's own recovery agree on how far past the last used address
// they are willing to look.
func (w *Wallet) discoveryGapLimit() uint32 {
	if w.recoveryWindow == 0 {
		return defaultDiscoveryGapLimit
	}

	return w.recoveryWindow
}

// deriveBranchScripts derives the pkScripts for indices
// [start, start+count) on one scope branch under a single read
// transaction.
//
// Derivation goes through the scoped key manager rather than
// reconstructing paths from the seed so each script gets the address
// type that scope's schema dictates — P2TR for BIP86, P2WPKH for BIP84,
// nested P2SH for BIP49 — which is exactly what btcwallet itself would
// hand out. Only public keys are touched, so this works with the wallet
// still locked.
func (w *Wallet) deriveBranchScripts(sb scopeBranch, start, count uint32) (
	[][]byte, error) {

	btcw := w.BtcWallet.InternalWallet()

	scoped, err := btcw.AddrManager().FetchScopedKeyManager(sb.scope)
	if err != nil {
		return nil, fmt.Errorf("fetch scoped manager: %w", err)
	}

	// The default account is the only one this daemon derives receive
	// and change addresses under.
	const acct = waddrmgr.DefaultAccountNum

	scripts := make([][]byte, 0, count)
	err = walletdb.View(btcw.Database(), func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)

		for i := start; i < start+count; i++ {
			addr, err := scoped.DeriveFromKeyPath(
				ns, waddrmgr.DerivationPath{
					InternalAccount: acct,
					Account:         acct,
					Branch:          sb.branch,
					Index:           i,
				},
			)
			if err != nil {
				return fmt.Errorf("derive index %d: %w", i, err)
			}

			pkScript, err := txscript.PayToAddrScript(
				addr.Address(),
			)
			if err != nil {
				return fmt.Errorf("pkScript for index %d: %w",
					i, err)
			}

			scripts = append(scripts, pkScript)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return scripts, nil
}

// probeScripts asks the index whether each script has ever been used,
// returning one flag per input script in the same order. Probes run
// concurrently under scriptProbeConcurrency because each is an
// independent round trip and a gap-limit walk issues them in batches.
func (w *Wallet) probeScripts(ctx context.Context, scripts [][]byte) ([]bool,
	error) {

	flags := make([]bool, len(scripts))

	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(scriptProbeConcurrency)

	for i, pkScript := range scripts {
		group.Go(func() error {
			stats, err := w.esplora.GetScriptStats(gctx, pkScript)
			if err != nil {
				return err
			}

			flags[i] = stats.Used()

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, fmt.Errorf("probe scripts: %w", err)
	}

	return flags, nil
}

// earliestScriptHeight returns the height of the earliest confirmed
// transaction referencing pkScript, or None when the script has only
// unconfirmed history. Esplora serves this history newest first, so
// reaching the earliest entry means paging to the end.
func (w *Wallet) earliestScriptHeight(ctx context.Context, pkScript []byte) (
	fn.Option[int32], error) {

	none := fn.None[int32]()

	var (
		lastSeen string
		earliest = int32(math.MaxInt32)
	)

	for range maxScriptTxPages {
		txs, err := w.esplora.GetScriptChainTxs(ctx, pkScript, lastSeen)
		if err != nil {
			return none, err
		}

		if len(txs) == 0 {
			if earliest == math.MaxInt32 {
				return none, nil
			}

			return fn.Some(earliest), nil
		}

		for _, tx := range txs {
			if !tx.Status.Confirmed {
				continue
			}

			if height := int32(tx.Status.BlockHeight); height <
				earliest {

				earliest = height
			}
		}

		lastSeen = txs[len(txs)-1].Txid
	}

	return none, errHistoryTooLong
}
