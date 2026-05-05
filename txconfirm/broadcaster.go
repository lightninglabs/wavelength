package txconfirm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	"github.com/lightninglabs/darepo-client/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
)

const (
	// DefaultMaxFeeRateSatPerVByte clamps fee estimates to a sane ceiling.
	// On regtest or freshly synced nodes, the estimator can return wildly
	// inflated rates.
	DefaultMaxFeeRateSatPerVByte int64 = 100

	// DustLimit is the minimum useful value for the CPFP child change
	// output. Values below this are donated to fees.
	DustLimit = btcutil.Amount(330)

	// DefaultIncrementalRelayFeeSatPerVByte is the default per-vbyte
	// bandwidth cost a fee-bump replacement must pay in addition to
	// the original package's absolute fee. It matches Bitcoin Core's
	// default node setting and is used by the CPFP fee-bump loop to
	// enforce BIP-125 Rule 4 (replacement must pay at least its own
	// relay bandwidth on top of the replaced package's fee).
	DefaultIncrementalRelayFeeSatPerVByte int64 = 1

	// DefaultFeeInputLeaseExpiry is how long the broadcaster asks the
	// wallet to lease a CPFP fee-input UTXO. The lease is explicitly
	// released on terminal eviction and on fallback paths, but that
	// wallet call is best-effort and deliberately off the txconfirm
	// actor path. The expiry is therefore the final backstop if the
	// backend release call stalls or the owning actor disappears.
	DefaultFeeInputLeaseExpiry = time.Hour
)

var (
	// ErrCPFPFeeInputUnavailable indicates that an anchor
	// parent still needs a confirmed wallet fee input before
	// it can be broadcast safely.
	ErrCPFPFeeInputUnavailable = errors.New(
		"cpfp fee input unavailable",
	)

	// ErrNonTRUCParent indicates that the caller submitted a parent
	// transaction whose version is not v3 (TRUC). txconfirm relies on
	// BIP-431 ephemeral-anchor and TRUC-package semantics for its
	// CPFP fee-bump strategy: without v3, package RBF replacement
	// rules and the zero-fee anchor are not policy-legal on a
	// standard Bitcoin Core mempool, and anchor detection becomes
	// structurally ambiguous (a legitimate output script could match
	// the anyone-can-spend anchor pattern by accident). We therefore
	// reject non-v3 parents at the Submit boundary rather than
	// silently attaching a CPFP child that would never relay.
	ErrNonTRUCParent = errors.New(
		"parent transaction must be v3 (TRUC) for CPFP broadcast",
	)
)

// txconfirmLockID is the package-scoped LockID used by CPFPBroadcaster
// when leasing fee-input UTXOs via the Wallet interface. It is derived
// from the ASCII string "darepo-client:txconfirm" padded to 32 bytes
// so concurrent subsystems using a different LockID prefix cannot
// release txconfirm's leases by mistake. The value is a compile-time
// constant: callers do not need to synchronise LockIDs across restarts
// because the broadcaster already rebuilds its in-memory reservation
// state from per-parent FSM progress on recovery.
var txconfirmLockID = func() wallet.LockID {
	var id wallet.LockID
	copy(id[:], "darepo-client:txconfirm")

	return id
}()

// Wallet provides the wallet operations needed by the broadcaster for
// CPFP fee payment. The OutputLeaser contract (LeaseOutput /
// ReleaseOutput with an explicit caller-scoped LockID) matches the
// canonical shape exposed by btcwallet and lndclient's WalletKit, so
// concrete backends can delegate directly to their underlying wallet.
// Wallets that lack native lease support may return nil from the
// lease/release methods: the broadcaster's own per-parent reservation
// map still prevents intra-package races, and lease errors are
// treated as soft misses by the caller.
type Wallet interface {
	// ListUnspent returns confirmed wallet UTXOs usable as CPFP fee inputs.
	ListUnspent(ctx context.Context,
		minConfs, maxConfs int32) ([]*wallet.Utxo, error)

	// NewWalletPkScript returns a fresh wallet-managed pkScript
	// suitable for
	// change outputs.
	NewWalletPkScript(ctx context.Context) ([]byte, error)

	// FinalizePsbt signs and finalizes a PSBT packet. The wallet signs all
	// inputs it owns and returns the finalized wire tx.
	FinalizePsbt(ctx context.Context, packet []byte) (*wire.MsgTx, error)

	wallet.OutputLeaser
}

// FeeInput is a confirmed wallet UTXO selected for CPFP fee payment.
type FeeInput struct {
	// Outpoint identifies the wallet UTXO.
	Outpoint wire.OutPoint

	// Output is the output being spent for fees.
	Output *wire.TxOut

	// Confirmed indicates whether this UTXO is confirmed.
	Confirmed bool
}

// cloneFeeInput returns a deep copy of the supplied fee input.
func cloneFeeInput(input *FeeInput) *FeeInput {
	if input == nil {
		return nil
	}

	clone := &FeeInput{
		Outpoint:  input.Outpoint,
		Confirmed: input.Confirmed,
	}
	if input.Output != nil {
		clone.Output = &wire.TxOut{
			Value:    input.Output.Value,
			PkScript: append([]byte(nil), input.Output.PkScript...),
		}
	}

	return clone
}

// BroadcastRequest describes a signed transaction to broadcast.
type BroadcastRequest struct {
	// Tx is the fully signed parent transaction.
	Tx *wire.MsgTx

	// Label is a human-readable label for logging.
	Label string
}

// BroadcastResult describes the outcome of one broadcast attempt.
type BroadcastResult struct {
	// Txid is the parent transaction hash.
	Txid chainhash.Hash

	// ChildTxid is set when a CPFP child was built and submitted.
	ChildTxid *chainhash.Hash

	// FeeRate is the fee rate used in sat/vB.
	FeeRate int64
}

// BroadcasterConfig configures the generic CPFP broadcaster helper.
type BroadcasterConfig struct {
	// ChainSource provides fee estimation, package submission, and direct
	// transaction broadcast.
	ChainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]

	// Wallet provides UTXO selection and PSBT signing for CPFP children.
	Wallet Wallet

	// Log is an optional logger.
	Log fn.Option[btclog.Logger]

	// MaxFeeRateSatPerVByte caps fee estimates. Zero falls back to
	// DefaultMaxFeeRateSatPerVByte.
	MaxFeeRateSatPerVByte int64

	// IncrementalRelayFeeSatPerVByte is the minimum additional feerate
	// a fee-bump replacement must pay on top of the package it replaces
	// (BIP-125 Rule 4). Zero falls back to
	// DefaultIncrementalRelayFeeSatPerVByte. Operators whose Bitcoin
	// Core nodes override -incrementalrelayfee should pass the matching
	// value here so our bumps always clear the local node's policy.
	IncrementalRelayFeeSatPerVByte int64

	// PreSubmitTestMempoolAccept enables an opt-in pre-submit call to
	// ChainSource.TestMempoolAccept before every broadcast attempt.
	// When set, the broadcaster asks the backend to validate each
	// transaction (or the full parent+child package for CPFP paths)
	// against local node policy; a backend "accepted = false" result
	// aborts the submission with a clear error containing the reject
	// reason. Backends that do not implement testmempoolaccept return
	// "not supported" which is logged and treated as a soft-miss so
	// this flag is safe to leave enabled across heterogeneous
	// deployments.
	PreSubmitTestMempoolAccept bool
}

// parentBumpState records everything the broadcaster needs to enforce
// BIP-125 Rule 3 (absolute-fee) and Rule 4 (incremental-feerate) across
// successive fee-bump submissions for the same parent transaction, and
// to keep a stable set of fee-input reservations across blocks so two
// concurrent parents never race for the same wallet UTXO.
//
// A fresh parent has no entry; the first Submit establishes one when it
// selects a fee input. Subsequent submissions for the same parent txid
// read this state, floor their fee/feerate high enough to replace the
// previous package, submit, and overwrite the fee-rate/fee fields with
// the new values. The UsedFeeOutpoints set is additive: once a UTXO has
// been committed to a parent's submissions it stays reserved for that
// parent (and off-limits to other parents) until the parent is evicted.
// UsedFeeInputs mirrors that set with the full output data needed to
// rebuild a replacement child if the wallet stops listing the UTXO
// after an earlier child spends it in the mempool.
type parentBumpState struct {
	// LastFeeRate is the sat/vB feerate of the most recent successful
	// package submission for this parent.
	LastFeeRate int64

	// LastPackageFee is the absolute package fee (parent + child, in
	// sats) paid by the most recent successful submission for this
	// parent.
	LastPackageFee btcutil.Amount

	// UsedFeeOutpoints is the set of wallet UTXOs this parent's child
	// packages have consumed across the parent's submission history.
	// It survives block boundaries: a UTXO we already committed to a
	// parent's child stays reserved for that parent (and excluded from
	// other parents' selections) until Evict fires on terminal.
	//
	// For the parent itself, re-picking a UTXO from its own reserved
	// set is allowed — that is how TRUC package RBF replaces an
	// earlier child by double-spending the same fee input, which is
	// the mechanism the replacement actually lands on.
	UsedFeeOutpoints map[wire.OutPoint]struct{}

	// UsedFeeInputs contains the full selected fee-input data keyed by
	// outpoint. Some wallet backends hide mempool-spent UTXOs from
	// ListUnspent, so the broadcaster must retain the WitnessUtxo data
	// itself in order to re-pick a parent's own fee input on later
	// RBF bumps.
	UsedFeeInputs map[wire.OutPoint]*FeeInput
}

// CPFPBroadcaster broadcasts signed transactions and automatically attaches a
// CPFP child when the transaction contains an anchor output.
//
// CPFPBroadcaster is not safe for concurrent use. The outer txconfirm actor
// serializes access.
type CPFPBroadcaster struct {
	cfg BroadcasterConfig
	log btclog.Logger

	// parentStates records per-parent-txid fee-bump history (for
	// BIP-125 Rule 3/4 enforcement) and per-parent fee-input
	// reservations (so two parents never race for the same wallet
	// UTXO). Entries are populated as parents first select fee inputs
	// and released via Evict when the caller's FSM learns the parent
	// has terminally confirmed or failed.
	parentStates map[chainhash.Hash]*parentBumpState
}

// NewCPFPBroadcaster creates a new generic CPFP broadcaster helper.
func NewCPFPBroadcaster(cfg BroadcasterConfig) *CPFPBroadcaster {
	if cfg.MaxFeeRateSatPerVByte <= 0 {
		cfg.MaxFeeRateSatPerVByte = DefaultMaxFeeRateSatPerVByte
	}
	if cfg.IncrementalRelayFeeSatPerVByte <= 0 {
		cfg.IncrementalRelayFeeSatPerVByte =
			DefaultIncrementalRelayFeeSatPerVByte
	}

	return &CPFPBroadcaster{
		cfg:          cfg,
		log:          cfg.Log.UnwrapOr(btclog.Disabled),
		parentStates: make(map[chainhash.Hash]*parentBumpState),
	}
}

// Evict releases all per-parent state (fee-bump history and fee-input
// reservations) recorded for the supplied parent txid. Callers must
// invoke Evict once the tracked tx reaches a terminal state (Confirmed
// or Failed) so the broadcaster does not retain state indefinitely and
// so the parent's reserved UTXOs become available to other parents.
//
// Evict also asks the wallet to release any backend-level leases held on the
// parent's reserved UTXOs. That call is intentionally asynchronous: the
// btcwallet-backed adapters do not accept a context, and can block behind
// wallet block processing. Txconfirm's in-memory reservation map is the
// authoritative source for this actor, so terminal progress must not wait on a
// best-effort backend cleanup that also has a lease-expiry backstop.
func (b *CPFPBroadcaster) Evict(ctx context.Context,
	txid chainhash.Hash) {

	state, ok := b.parentStates[txid]
	if !ok {
		return
	}

	outpoints := make([]wire.OutPoint, 0, len(state.UsedFeeOutpoints))
	for op := range state.UsedFeeOutpoints {
		outpoints = append(outpoints, op)
	}

	delete(b.parentStates, txid)
	b.releaseWalletLeasesAsync(ctx, outpoints)
}

// reserveFeeInput records that the given parent txid is consuming the
// supplied wallet input and asks the wallet to lease it so that the
// wallet's own coin selection will not hand it to another subsystem
// while the CPFP child is in flight. A failure from the wallet-level
// lease is logged but does not abort the broadcast: the in-memory
// reservation map in parentStates is authoritative for intra-package
// coordination.
func (b *CPFPBroadcaster) reserveFeeInput(ctx context.Context,
	parentTxid chainhash.Hash, feeInput *FeeInput) {

	if feeInput == nil {
		return
	}

	state := b.parentStates[parentTxid]
	if state == nil {
		state = &parentBumpState{
			UsedFeeOutpoints: make(map[wire.OutPoint]struct{}),
			UsedFeeInputs:    make(map[wire.OutPoint]*FeeInput),
		}
		b.parentStates[parentTxid] = state
	}
	if state.UsedFeeOutpoints == nil {
		state.UsedFeeOutpoints = make(map[wire.OutPoint]struct{})
	}
	if state.UsedFeeInputs == nil {
		state.UsedFeeInputs = make(map[wire.OutPoint]*FeeInput)
	}

	op := feeInput.Outpoint

	// If we already reserved this outpoint for this parent, the
	// wallet already has a lease; re-leasing extends the expiry,
	// which is exactly what we want on a fee bump that re-picks the
	// same UTXO for TRUC RBF double-spend.
	state.UsedFeeOutpoints[op] = struct{}{}
	state.UsedFeeInputs[op] = cloneFeeInput(feeInput)

	if b.cfg.Wallet == nil {
		return
	}

	_, err := b.cfg.Wallet.LeaseOutput(
		ctx, txconfirmLockID, op, DefaultFeeInputLeaseExpiry,
	)
	if err != nil {
		b.log.WarnS(ctx, "Wallet-level lease failed; relying on "+
			"in-memory reservation only",
			err, "parent", parentTxid, "outpoint", op)
	}
}

// releaseWalletLease calls wallet.ReleaseOutput with the package-scoped
// LockID, silently ignoring "unknown output" / "not leased" class
// errors since those are a normal consequence of a wallet that is
// already past this lease (expired or garbage-collected).
func (b *CPFPBroadcaster) releaseWalletLease(ctx context.Context,
	op wire.OutPoint) {

	if b.cfg.Wallet == nil {
		return
	}

	err := b.cfg.Wallet.ReleaseOutput(ctx, txconfirmLockID, op)
	if err != nil {
		b.log.WarnS(ctx, "Wallet-level lease release failed",
			err, "outpoint", op)
	}
}

// releaseWalletLeasesAsync releases wallet-level leases without blocking the
// txconfirm actor. The in-memory reservation has already been dropped before
// this helper is called, so a slow backend release cannot affect txconfirm
// progress; the wallet lease expiry is the cleanup backstop if the backend
// never returns.
func (b *CPFPBroadcaster) releaseWalletLeasesAsync(_ context.Context,
	outpoints []wire.OutPoint) {

	if b.cfg.Wallet == nil || len(outpoints) == 0 {
		return
	}

	outpoints = append([]wire.OutPoint(nil), outpoints...)
	//nolint:contextcheck // wallet lease release is bounded independently
	go func() {
		ctx, cancel := context.WithTimeout(
			context.Background(), DefaultFeeInputLeaseExpiry,
		)
		defer cancel()

		for _, op := range outpoints {
			b.releaseWalletLease(ctx, op)
		}
	}()
}

// releaseFeeOutpoint removes the given wallet outpoint from the parent's
// reserved set and releases the wallet-level lease held on it. Called
// on fallback/failure paths where the CPFP child that would have spent
// the outpoint never actually reached the mempool, so holding the
// reservation just starves other parents without any TRUC RBF
// double-spend to protect.
func (b *CPFPBroadcaster) releaseFeeOutpoint(ctx context.Context,
	parentTxid chainhash.Hash, op wire.OutPoint) {

	state, ok := b.parentStates[parentTxid]
	if !ok || state.UsedFeeOutpoints == nil {
		return
	}

	if _, held := state.UsedFeeOutpoints[op]; !held {
		return
	}

	delete(state.UsedFeeOutpoints, op)
	delete(state.UsedFeeInputs, op)

	b.releaseWalletLeasesAsync(ctx, []wire.OutPoint{op})

	// If the parent has no fee history and no remaining reservations,
	// drop the empty entry entirely so parentStates does not accumulate
	// zero-value shells.
	if state.LastFeeRate == 0 && state.LastPackageFee == 0 &&
		len(state.UsedFeeOutpoints) == 0 &&
		len(state.UsedFeeInputs) == 0 {

		delete(b.parentStates, parentTxid)
	}
}

// excludedOutpointsForOtherParents returns the set of wallet UTXOs
// currently reserved by parents other than the supplied one. The caller
// uses this to exclude those UTXOs from fee-input selection so two
// concurrent parents can never try to spend the same UTXO.
func (b *CPFPBroadcaster) excludedOutpointsForOtherParents(
	parentTxid chainhash.Hash) map[wire.OutPoint]struct{} {

	excluded := make(map[wire.OutPoint]struct{})
	for otherTxid, state := range b.parentStates {
		if otherTxid == parentTxid {
			continue
		}
		for op := range state.UsedFeeOutpoints {
			excluded[op] = struct{}{}
		}
	}

	return excluded
}

// Submit broadcasts a signed transaction. If the transaction contains an
// anchor output, Submit constructs a CPFP child and submits the package.
//
// Submit deliberately takes a single *wire.MsgTx rather than a
// caller-assembled package. The CPFP child is always derived here from
// wallet state the caller does not (and should not) have direct access
// to: a freshly-derived change pkScript, a confirmed wallet fee input
// that has to be coordinated against the per-parent reservation map to
// avoid cross-parent UTXO races, PSBT finalization via the wallet
// interface, and BIP-125 Rule 3/4 floor arithmetic that depends on the
// previous submission for the same parent txid. Letting the caller
// pre-build a package would force those concerns to leak out of this
// package; letting the caller hand us a pre-signed child would break
// deduplication (the broadcaster must own the child so it can regenerate
// a fresh one on every fee bump). The single-parent signature therefore
// models the only contract the broadcaster is prepared to guarantee:
// "give me a signed parent, I'll handle the rest, including the CPFP
// child and its fee-bump lifecycle."
//
// Parents that are not v3 (TRUC) are rejected with ErrNonTRUCParent: the
// whole CPFP fee-bump strategy in this package assumes BIP-431 semantics
// for anchor-bearing transactions, and relying on pattern-based anchor
// detection against non-v3 parents is structurally unsafe (a coincidental
// anyone-can-spend-looking output would silently receive a CPFP child
// that the mempool then rejects, burning the caller's fee input).
func (b *CPFPBroadcaster) Submit(ctx context.Context, height int32,
	req *BroadcastRequest) (*BroadcastResult, error) {

	if req == nil || req.Tx == nil {
		return nil, fmt.Errorf("broadcast request and tx required")
	}

	if req.Tx.Version != arktx.TxVersion {
		return nil, fmt.Errorf("%w: got version %d, want %d",
			ErrNonTRUCParent, req.Tx.Version, arktx.TxVersion)
	}

	txid := req.Tx.TxHash()
	anchorIdx := findAnchorOutput(req.Tx)
	if anchorIdx < 0 {
		return b.broadcastDirect(ctx, req, txid)
	}

	return b.broadcastWithCPFP(ctx, height, req, txid, anchorIdx)
}

// broadcastDirect broadcasts a transaction without CPFP.
func (b *CPFPBroadcaster) broadcastDirect(ctx context.Context,
	req *BroadcastRequest, txid chainhash.Hash) (*BroadcastResult, error) {

	if err := b.preflightIfEnabled(ctx, req.Tx); err != nil {
		return nil, err
	}

	_, err := b.cfg.ChainSource.Ask(
		ctx, &chainsource.BroadcastTxRequest{
			Tx:    req.Tx,
			Label: req.Label,
		},
	).Await(ctx).Unpack()
	if err != nil && !IsIgnorableBroadcastError(err) {
		return nil, fmt.Errorf("broadcast %s: %w", txid, err)
	}

	return &BroadcastResult{Txid: txid}, nil
}

// Preflight asks the chain backend whether the supplied transactions
// would be accepted by the local mempool without broadcasting them.
// Multiple transactions are submitted as a package (matching Bitcoin
// Core's testmempoolaccept RPC array form).
//
// Preflight distinguishes three outcomes:
//
//   - All transactions accepted: returns nil.
//   - At least one transaction rejected: returns an error that includes
//     the backend's human-readable reject reason and the rejected
//     txid(s). Callers should treat this as a hard failure and surface
//     it to the FSM.
//   - Backend does not support testmempoolaccept (or
//     ErrPackageMempoolAcceptUnsupported on a package request): returns
//     a sentinel chainsource.ErrPackageMempoolAcceptUnsupported wrapper
//     so callers that treat preflight as best-effort can `errors.Is`-
//     check and continue.
func (b *CPFPBroadcaster) Preflight(ctx context.Context,
	txs ...*wire.MsgTx) error {

	if len(txs) == 0 {
		return fmt.Errorf("preflight requires at least one tx")
	}

	resp, err := b.cfg.ChainSource.Ask(
		ctx, &chainsource.TestMempoolAcceptRequest{Txs: txs},
	).Await(ctx).Unpack()
	if err != nil {
		return err
	}

	result, ok := resp.(*chainsource.TestMempoolAcceptResponse)
	if !ok {
		return fmt.Errorf("unexpected testmempoolaccept response %T",
			resp)
	}

	for _, r := range result.Results {
		if r.Accepted {
			continue
		}

		return fmt.Errorf(
			"testmempoolaccept rejected %s: %s", r.Txid, r.Reason,
		)
	}

	return nil
}

// preflightIfEnabled runs Preflight when the caller enabled
// PreSubmitTestMempoolAccept. Backends that report
// "not supported" (or the sentinel ErrPackageMempoolAcceptUnsupported
// for package requests) are downgraded to a warning so the flag is safe
// to set across heterogeneous deployments.
func (b *CPFPBroadcaster) preflightIfEnabled(ctx context.Context,
	txs ...*wire.MsgTx) error {

	if !b.cfg.PreSubmitTestMempoolAccept {
		return nil
	}

	err := b.Preflight(ctx, txs...)
	switch {
	case err == nil:
		return nil

	case errors.Is(err, chainsource.ErrPackageMempoolAcceptUnsupported):
		b.log.DebugS(ctx,
			"Skipping preflight: backend does not support "+
				"package testmempoolaccept", "err", err)

		return nil

	case strings.Contains(err.Error(), "not supported"):
		b.log.DebugS(ctx,
			"Skipping preflight: backend does not support "+
				"testmempoolaccept", "err", err)

		return nil
	}

	return err
}

// broadcastWithCPFP builds a CPFP child and submits the parent+child package.
//
// On the first submission for a given parent txid, the fee rate and total
// package fee come straight from EstimateFeeRate + EstimatePackageFee. On
// every subsequent submission (fee bump), we compare against the previous
// submission's feerate and absolute fee stored in parentStates:
//
//   - BIP-125 Rule 4: the new feerate must strictly exceed the previous
//     feerate, so we floor it at prev.LastFeeRate + 1 if the estimator is
//     flat or dips.
//   - BIP-125 Rule 3: the new absolute package fee must exceed the
//     previous package fee by at least IncrementalRelayFeeSatPerVByte *
//     packageVSize. If the naive feerate * packageVSize calculation
//     doesn't clear that threshold, we bump totalFee up to satisfy it.
//
// Without these, a flat-fee-estimator cycle regenerates a byte-identical
// package (rejected as "already in mempool") and a decreasing-fee-estimator
// cycle produces a BIP-125-non-compliant replacement that the mempool
// rejects outright.
func (b *CPFPBroadcaster) broadcastWithCPFP(ctx context.Context,
	height int32, req *BroadcastRequest, txid chainhash.Hash,
	anchorIdx int) (*BroadcastResult, error) {

	// Derive the change pkScript first so its script class can inform
	// the child's vsize estimate. A failure here means we cannot build
	// a CPFP child at all, so we fall straight through to broadcasting
	// the parent directly; no fee-input reservation has been made yet,
	// so there is nothing to release.
	changePkScript, err := b.deriveChangePkScript(ctx)
	if err != nil {
		return b.fallbackDirectBroadcast(
			ctx, req, txid, wire.OutPoint{},
			"derive_change_pkscript", err,
		)
	}

	// Use the change pkScript as the proxy for the fee-input's script
	// class too: wallets hand out consistent address types, so this
	// keeps the estimate accurate on P2TR-only, P2WKH-only, and
	// nested-P2WKH-only wallets without requiring a second wallet
	// round-trip to probe UTXOs before selecting one. Rare mixed-type
	// wallets will get a slight over-estimate on the non-matching
	// side, which is safer than under-estimating.
	childVSize := estimateChildVSize(changePkScript, changePkScript)

	feeRate, err := b.EstimateFeeRate(ctx)
	if err != nil {
		return nil, fmt.Errorf("estimate fee: %w", err)
	}

	totalFee, err := computePackageFee(
		req.Tx, btcutil.Amount(feeRate), childVSize,
	)
	if err != nil {
		return nil, fmt.Errorf("estimate package fee: %w", err)
	}

	feeRate, totalFee = b.applyReplacementFloor(
		req.Tx, txid, feeRate, totalFee, childVSize,
	)

	feeInput, err := b.selectFeeInput(ctx, txid, totalFee)
	if err != nil {
		return nil, fmt.Errorf("%w: %w",
			ErrCPFPFeeInputUnavailable, err,
		)
	}

	b.reserveFeeInput(ctx, txid, feeInput)

	// If the chosen fee-input's actual script class differs from the
	// change script's (wallets that genuinely mix types), recompute
	// with the real inputs and top up the fee if the new estimate is
	// larger. We never lower the fee here: the replacement-floor work
	// above is already locked in, so only a higher-than-floor fee is
	// safe.
	preciseChildVSize := estimateChildVSize(
		feeInput.Output.PkScript, changePkScript,
	)
	if preciseChildVSize > childVSize {
		preciseFee, feeErr := computePackageFee(
			req.Tx, btcutil.Amount(feeRate), preciseChildVSize,
		)
		if feeErr == nil && preciseFee > totalFee {
			totalFee = preciseFee
		}
	}

	anchorOutpoint := wire.OutPoint{Hash: txid, Index: uint32(anchorIdx)}
	anchorOutput := req.Tx.TxOut[anchorIdx]

	child, err := BuildCPFPChild(
		req.Tx.Version, anchorOutpoint, anchorOutput, feeInput,
		changePkScript, totalFee,
	)
	if err != nil {
		return b.fallbackDirectBroadcast(
			ctx, req, txid, feeInput.Outpoint,
			"build_cpfp_child", err,
		)
	}

	err = b.signCPFPChild(
		ctx, child, anchorOutpoint, anchorOutput, feeInput,
	)
	if err != nil {
		return b.fallbackDirectBroadcast(
			ctx, req, txid, feeInput.Outpoint,
			"sign_cpfp_child", err,
		)
	}

	// Preflight the package (parent + signed child) against local node
	// policy before asking the backend to relay it. A rejection here is
	// treated as a hard failure because the caller has already paid to
	// sign the child and we'd otherwise submit a package we know the
	// mempool will reject. Release the reservation so the next retry
	// (after the caller decides what to do) can re-select freely.
	if err := b.preflightIfEnabled(ctx, req.Tx, child); err != nil {
		b.releaseFeeOutpoint(ctx, txid, feeInput.Outpoint)

		return nil, fmt.Errorf("preflight package: %w", err)
	}

	_, pkgErr := b.cfg.ChainSource.Ask(
		ctx, &chainsource.SubmitPackageRequest{
			Parents: []*wire.MsgTx{req.Tx},
			Child:   child,
		},
	).Await(ctx).Unpack()
	if pkgErr != nil {
		switch {
		case IsIgnorableBroadcastError(pkgErr):
			b.log.DebugS(
				ctx,
				"Package already known for "+
					txid.String(),
			)

		case isPackageSubmissionUnsupported(pkgErr):
			if err := b.broadcastIndividually(
				ctx, req.Tx, child, req.Label,
			); err != nil {
				b.releaseFeeOutpoint(
					ctx, txid, feeInput.Outpoint,
				)

				return nil, fmt.Errorf(
					"broadcast fallback: %w",
					err,
				)
			}

		default:
			// The package was rejected wholesale: the child
			// did not land in the mempool, so the fee input
			// is a stale reservation that should be released.
			b.releaseFeeOutpoint(ctx, txid, feeInput.Outpoint)

			return nil, fmt.Errorf(
				"submit package: %w", pkgErr,
			)
		}
	}

	childTxid := child.TxHash()

	// Record the submission so the next fee bump for this parent can
	// enforce BIP-125 Rule 3/4 against it. The parentStates entry was
	// already created (or updated) by reserveFeeOutpoint above, so we
	// update the fee-history fields in place to preserve the
	// UsedFeeOutpoints reservation accumulated over all prior bumps.
	state := b.parentStates[txid]
	state.LastFeeRate = feeRate
	state.LastPackageFee = totalFee

	return &BroadcastResult{
		Txid:      txid,
		ChildTxid: &childTxid,
		FeeRate:   feeRate,
	}, nil
}

// applyReplacementFloor returns feerate and totalFee values adjusted so
// that a package submitted with them will satisfy BIP-125 Rule 3 and Rule
// 4 against whatever package was previously submitted for the same parent
// txid. A parent with no recorded prior submission is passed through
// unchanged (first submission has nothing to replace).
//
// childVSize is the caller's current estimate of the CPFP child's
// vsize; the caller supplies it because the class of the fee input and
// change output (and therefore the vsize) depends on wallet-specific
// details this helper does not have visibility into.
func (b *CPFPBroadcaster) applyReplacementFloor(parent *wire.MsgTx,
	txid chainhash.Hash, feeRate int64,
	totalFee btcutil.Amount,
	childVSize int64) (int64, btcutil.Amount) {

	prev, havePrev := b.parentStates[txid]
	if !havePrev {
		return feeRate, totalFee
	}

	// Rule 4: replacement feerate must strictly exceed the prior
	// package's feerate. If the estimator returned a flat or lower
	// value, ratchet the feerate up by one sat/vB.
	if feeRate <= prev.LastFeeRate {
		feeRate = prev.LastFeeRate + 1
	}

	// Recompute totalFee at the (possibly bumped) feerate so the
	// following Rule 3 check compares apples to apples.
	parentWeight := EstimateWeight(parent)
	parentVSize := (parentWeight + 3) / 4
	packageVSize := parentVSize + childVSize

	naiveFee := btcutil.Amount(feeRate) * btcutil.Amount(packageVSize)
	if totalFee < naiveFee {
		totalFee = naiveFee
	}

	// Rule 3: additional fee (new_total - old_total) must cover the
	// replacement's own bandwidth at the node's incremental relay
	// feerate. If the straight feerate bump doesn't clear that
	// threshold (typical when the vsize grew but the feerate only
	// ticked up by 1), pay the shortfall as a flat fee bump.
	minAdditional := btcutil.Amount(
		b.cfg.IncrementalRelayFeeSatPerVByte * packageVSize,
	)
	minRequired := prev.LastPackageFee + minAdditional
	if totalFee < minRequired {
		totalFee = minRequired
	}

	return feeRate, totalFee
}

// fallbackDirectBroadcast logs one CPFP setup failure and falls back to
// broadcasting the parent transaction directly.
//
// releaseOutpoint is the wallet outpoint that was tentatively reserved
// for the CPFP child before the setup failure. Because the child never
// reaches the mempool on this path, the reservation is stale and would
// otherwise starve concurrent parents of UTXOs until the tracked tx
// terminally evicts.
func (b *CPFPBroadcaster) fallbackDirectBroadcast(ctx context.Context,
	req *BroadcastRequest, txid chainhash.Hash,
	releaseOutpoint wire.OutPoint, stage string,
	err error) (*BroadcastResult, error) {

	b.releaseFeeOutpoint(ctx, txid, releaseOutpoint)

	b.log.WarnS(ctx, "CPFP unavailable; broadcasting parent directly",
		err, "txid", txid, "stage", stage, "label", req.Label)

	return b.broadcastDirect(ctx, req, txid)
}

// EstimateFeeRate returns the current fee rate in sat/vbyte, clamped by the
// configured maximum. On regtest (or when the chain backend has no fee
// history), estimation may fail — in that case we fall back to a minimum
// floor so the CPFP broadcast can still proceed.
func (b *CPFPBroadcaster) EstimateFeeRate(ctx context.Context) (int64, error) {
	const minFeeRateSatPerVByte int64 = 2

	resp, err := b.cfg.ChainSource.Ask(
		ctx, &chainsource.FeeEstimateRequest{
			TargetConf: 6,
		},
	).Await(ctx).Unpack()
	if err != nil {
		b.log.Warnf("Fee estimation failed, using fallback "+
			"%d sat/vB: %v", minFeeRateSatPerVByte, err)

		return minFeeRateSatPerVByte, nil
	}

	feeResp, ok := resp.(*chainsource.FeeEstimateResponse)
	if !ok {
		return 0, fmt.Errorf("unexpected fee response type %T", resp)
	}

	rate := int64(feeResp.SatPerVByte)
	if rate < minFeeRateSatPerVByte {
		rate = minFeeRateSatPerVByte
	}
	if rate > b.cfg.MaxFeeRateSatPerVByte {
		rate = b.cfg.MaxFeeRateSatPerVByte
	}

	return rate, nil
}

// selectFeeInput finds the smallest confirmed wallet UTXO that covers the
// required fee amount for the supplied parent txid.
//
// The exclusion set is built from outpoints reserved by every *other*
// parent in parentStates. UTXOs already reserved by the current parent
// are deliberately *not* excluded: TRUC package RBF relies on the new
// child double-spending the previous child's fee input, and that is how
// the replacement actually lands on the mempool. The current parent's
// already-reserved inputs are checked before ListUnspent because
// btcwallet and lwwallet stop returning a UTXO once our previous CPFP
// child spends it in the mempool, even though that same outpoint is
// exactly what the replacement child should spend again.
func (b *CPFPBroadcaster) selectFeeInput(ctx context.Context,
	parentTxid chainhash.Hash,
	minAmount btcutil.Amount) (*FeeInput, error) {

	if b.cfg.Wallet == nil {
		return nil, fmt.Errorf("wallet must be provided")
	}

	if feeInput := b.selectReservedFeeInput(
		parentTxid, minAmount,
	); feeInput != nil {
		return feeInput, nil
	}

	excluded := b.excludedOutpointsForOtherParents(parentTxid)

	deadline := time.Now().Add(2 * time.Second)

	for {
		utxos, err := b.cfg.Wallet.ListUnspent(ctx, 1, 9999999)
		if err != nil {
			return nil, fmt.Errorf("list unspent: %w", err)
		}

		var best *wallet.Utxo
		for _, utxo := range utxos {
			if _, skip := excluded[utxo.Outpoint]; skip {
				continue
			}

			if utxo.Amount < minAmount {
				continue
			}

			if best == nil || utxo.Amount < best.Amount {
				best = utxo
			}
		}

		if best != nil {
			return feeInputFromWalletUTXO(best), nil
		}

		// After one CPFP package confirms, wallet backends
		// with asynchronous chain ingestion can lag briefly
		// before the confirmed change output becomes visible.
		// Poll through that handoff instead of immediately
		// falling back to direct broadcast of a zero-fee
		// parent.
		if len(excluded) == 0 || time.Now().After(deadline) {
			break
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()

		case <-time.After(100 * time.Millisecond):
		}
	}

	return nil, fmt.Errorf("no confirmed wallet UTXOs available "+
		"(need >= %d sats)", int64(minAmount))
}

// selectReservedFeeInput returns the smallest cached fee input already
// reserved for the supplied parent txid that can cover minAmount.
func (b *CPFPBroadcaster) selectReservedFeeInput(
	parentTxid chainhash.Hash, minAmount btcutil.Amount) *FeeInput {

	state := b.parentStates[parentTxid]
	if state == nil {
		return nil
	}

	var best *FeeInput
	for _, feeInput := range state.UsedFeeInputs {
		if feeInput == nil || feeInput.Output == nil {
			continue
		}

		if !feeInput.Confirmed {
			continue
		}

		amount := btcutil.Amount(feeInput.Output.Value)
		if amount < minAmount {
			continue
		}

		if best == nil || amount < btcutil.Amount(best.Output.Value) {
			best = feeInput
		}
	}

	return cloneFeeInput(best)
}

// feeInputFromWalletUTXO converts a wallet UTXO into a broadcaster fee input.
func feeInputFromWalletUTXO(utxo *wallet.Utxo) *FeeInput {
	return &FeeInput{
		Outpoint: utxo.Outpoint,
		Output: &wire.TxOut{
			Value:    int64(utxo.Amount),
			PkScript: append([]byte(nil), utxo.PkScript...),
		},
		Confirmed: true,
	}
}

// deriveChangePkScript obtains a fresh wallet-managed pkScript for use as a
// CPFP child change output.
func (b *CPFPBroadcaster) deriveChangePkScript(
	ctx context.Context) ([]byte, error) {

	if b.cfg.Wallet == nil {
		return nil, fmt.Errorf("wallet must be provided")
	}

	pkScript, err := b.cfg.Wallet.NewWalletPkScript(ctx)
	if err != nil {
		return nil, fmt.Errorf("new wallet pkscript: %w", err)
	}

	if len(pkScript) == 0 {
		return nil, fmt.Errorf("wallet returned empty pkscript")
	}

	return append([]byte(nil), pkScript...), nil
}

// signCPFPChild signs the CPFP child via PSBT.
//
// The child is a caller-constructed transaction whose inputs always include
// the parent's anchor output (which is anyone-can-spend and needs a
// pre-finalized empty witness) and at least one wallet-owned fee input
// (which the wallet finalizes during FinalizePsbt).
//
// We match inputs by outpoint — both when attaching WitnessUtxo metadata to
// the PSBT before finalization and when copying finalized witnesses back
// into the child — rather than by positional index. This makes the
// function robust to two classes of wallet behaviour that positional
// indexing does not survive:
//
//   - The wallet returning the finalized transaction with inputs in a
//     different order than the PSBT presented (some backends reorder
//     inputs by BIP 69 or internal heuristics).
//   - A future caller adding a second fee input or rearranging child
//     construction — positional `packet.Inputs[0]` / `[1]` would silently
//     miswire WitnessUtxo values across the wrong inputs.
//
// Failures on either side of finalization return a clean error instead of
// panicking on an out-of-bounds index.
func (b *CPFPBroadcaster) signCPFPChild(ctx context.Context,
	child *wire.MsgTx, anchorOutpoint wire.OutPoint,
	anchorOutput *wire.TxOut, feeInput *FeeInput) error {

	if b.cfg.Wallet == nil {
		return fmt.Errorf("wallet must be provided")
	}

	if feeInput == nil || feeInput.Output == nil {
		return fmt.Errorf("fee input and output required")
	}

	inputs := make([]*wire.OutPoint, len(child.TxIn))
	sequences := make([]uint32, len(child.TxIn))
	for i, txIn := range child.TxIn {
		op := txIn.PreviousOutPoint
		inputs[i] = &op
		sequences[i] = txIn.Sequence
	}

	// Locate the anchor and fee inputs by outpoint. We deliberately do
	// not trust positional assumptions: BuildCPFPChild today places the
	// anchor at index 0 and fee at index 1, but refactors that change
	// the order must not silently corrupt the PSBT metadata.
	anchorIdx, feeIdx := -1, -1
	for i, op := range inputs {
		switch *op {
		case anchorOutpoint:
			anchorIdx = i

		case feeInput.Outpoint:
			feeIdx = i
		}
	}
	if anchorIdx < 0 {
		return fmt.Errorf("child is missing anchor input %s",
			anchorOutpoint)
	}
	if feeIdx < 0 {
		return fmt.Errorf("child is missing fee input %s",
			feeInput.Outpoint)
	}

	packet, err := psbt.New(
		inputs, child.TxOut, child.Version, child.LockTime, sequences,
	)
	if err != nil {
		return fmt.Errorf("create PSBT: %w", err)
	}

	packet.Inputs[anchorIdx].WitnessUtxo = anchorOutput
	packet.Inputs[anchorIdx].FinalScriptWitness = []byte{0x00}
	packet.Inputs[feeIdx].WitnessUtxo = feeInput.Output
	packet.Inputs[feeIdx].SighashType = cpfpFeeInputSighash(
		feeInput.Output.PkScript,
	)

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return fmt.Errorf("serialize PSBT: %w", err)
	}

	finalTx, err := b.cfg.Wallet.FinalizePsbt(ctx, buf.Bytes())
	if err != nil {
		return fmt.Errorf("finalize PSBT: %w", err)
	}

	// Copy witnesses back into the child by matching outpoints, not
	// positions. A length mismatch (wallet added or dropped inputs) or
	// a missing outpoint (wallet replaced an input we requested) is a
	// hard error: we cannot safely broadcast a package whose witnesses
	// do not correspond to the PSBT we asked the wallet to sign.
	if len(finalTx.TxIn) != len(child.TxIn) {
		return fmt.Errorf("finalized tx has %d inputs, expected %d",
			len(finalTx.TxIn), len(child.TxIn))
	}

	witnesses := make(map[wire.OutPoint]wire.TxWitness, len(finalTx.TxIn))
	for _, txIn := range finalTx.TxIn {
		witnesses[txIn.PreviousOutPoint] = txIn.Witness
	}

	for i := range child.TxIn {
		w, ok := witnesses[child.TxIn[i].PreviousOutPoint]
		if !ok {
			return fmt.Errorf(
				"finalized tx missing input for outpoint %s",
				child.TxIn[i].PreviousOutPoint,
			)
		}
		child.TxIn[i].Witness = w
	}

	return nil
}

// cpfpFeeInputSighash returns the explicit sighash type the wallet should use
// for the selected CPFP fee input.
func cpfpFeeInputSighash(pkScript []byte) txscript.SigHashType {
	if txscript.IsPayToTaproot(pkScript) {
		return txscript.SigHashDefault
	}

	return txscript.SigHashAll
}

// broadcastIndividually broadcasts the parent and child transactions one at a
// time as a fallback for backends without package relay.
func (b *CPFPBroadcaster) broadcastIndividually(ctx context.Context,
	parent, child *wire.MsgTx, label string) error {

	parentTxid := parent.TxHash()
	_, parentErr := b.cfg.ChainSource.Ask(
		ctx, &chainsource.BroadcastTxRequest{
			Tx:    parent,
			Label: label + "-parent",
		},
	).Await(ctx).Unpack()
	if parentErr != nil && !IsIgnorableBroadcastError(parentErr) {
		return fmt.Errorf("broadcast parent %s: %w",
			parentTxid, parentErr)
	}

	childTxid := child.TxHash()
	_, childErr := b.cfg.ChainSource.Ask(
		ctx, &chainsource.BroadcastTxRequest{
			Tx:    child,
			Label: label + "-child",
		},
	).Await(ctx).Unpack()
	if childErr != nil && !IsIgnorableBroadcastError(childErr) {
		return fmt.Errorf("broadcast child %s: %w", childTxid, childErr)
	}

	return nil
}

// findAnchorOutput returns the index of the anchor output in the transaction
// or -1 if none is found.
func findAnchorOutput(tx *wire.MsgTx) int {
	for i, out := range tx.TxOut {
		if arktx.IsAnchorOutput(out) {
			return i
		}
	}

	return -1
}

// estimateChildVSize returns the vbyte size of the CPFP child this
// package constructs: one ephemeral BIP-431 anchor input, one
// confirmed wallet fee input, and one wallet change output. The
// witness/output sizes for the fee input and change output are
// inferred from the actual pkScripts via txscript.GetScriptClass so
// wallets that hand out taproot, nested-p2wkh, or legacy p2wkh
// outputs all produce an accurate estimate (and therefore correct
// fee-bump rule 3/4 arithmetic) without the caller having to guess a
// constant.
//
// Unknown / non-standard script classes fall back to P2WKH on the
// input side and to a generic pkScript-length-based accounting on the
// output side (via AddOutput), which over-estimates rather than
// under-estimates and keeps the caller inside relay policy.
func estimateChildVSize(feeInputPkScript, changePkScript []byte) int64 {
	var est input.TxWeightEstimator

	// Ephemeral BIP-431 anchor: zero-value P2A output spent with an
	// empty witness. AddWitnessInput(0) accounts for the base input
	// bytes (outpoint + sequence + empty scriptSig) without any
	// witness items.
	est.AddWitnessInput(0)

	addInputForScript(&est, feeInputPkScript)
	addOutputForScript(&est, changePkScript)

	return int64(est.VSize())
}

// addInputForScript adds an input of the appropriate witness/script
// class to the supplied estimator based on the pkScript being spent.
// Unrecognised scripts fall through to a P2WKH-sized input so that the
// estimate is never smaller than a realistic wallet input — the goal
// of this helper is to never under-estimate the child's vsize, which
// would violate BIP-125 Rule 4 on the next fee bump.
func addInputForScript(est *input.TxWeightEstimator, pkScript []byte) {
	switch txscript.GetScriptClass(pkScript) {
	case txscript.WitnessV0PubKeyHashTy:
		est.AddP2WKHInput()

	case txscript.WitnessV1TaprootTy:
		// Key-spend path with SIGHASH_DEFAULT: 64-byte Schnorr
		// signature, no control-block / tapleaf data.
		est.AddTaprootKeySpendInput(txscript.SigHashDefault)

	case txscript.ScriptHashTy:
		// Assume the nested form most wallets use.
		est.AddNestedP2WKHInput()

	case txscript.PubKeyHashTy:
		est.AddP2PKHInput()

	default:
		est.AddP2WKHInput()
	}
}

// addOutputForScript adds an output of the appropriate class to the
// supplied estimator. When the pkScript is non-empty, AddOutput sizes
// it from the actual pkScript length, which is correct for any
// recognised or unrecognised class. When no script is available (e.g.
// a callers passing nil to get a pre-derivation estimate), fall back
// to a P2WKH-sized output so we never under-count and break the Rule
// 3 floor arithmetic.
func addOutputForScript(est *input.TxWeightEstimator, pkScript []byte) {
	if len(pkScript) == 0 {
		est.AddP2WKHOutput()

		return
	}

	est.AddOutput(pkScript)
}

// computePackageFee computes the total package fee for one parent+child
// submission at the given fee rate and caller-supplied child vsize.
// The child vsize is injected because it depends on wallet-specific
// script classes (P2TR, P2WKH, nested-P2WKH, …) that this helper has
// no visibility into.
func computePackageFee(parentTx *wire.MsgTx, feeRate btcutil.Amount,
	childVSize int64) (btcutil.Amount, error) {

	if parentTx == nil {
		return 0, fmt.Errorf("parent tx cannot be nil")
	}

	if feeRate <= 0 {
		return 0, fmt.Errorf("fee rate must be positive")
	}

	if childVSize <= 0 {
		return 0, fmt.Errorf("child vsize must be positive")
	}

	parentWeight := EstimateWeight(parentTx)
	parentVSize := (parentWeight + 3) / 4
	totalFee := feeRate * btcutil.Amount(parentVSize+childVSize)
	if totalFee < 1 {
		return 1, nil
	}

	return totalFee, nil
}

// EstimatePackageFee computes a total package fee for a parent+child
// submission at the given fee rate, using a default child shape
// (ephemeral anchor + P2WKH wallet input + P2WKH change) for the child
// vsize. Callers inside CPFPBroadcaster pass the actual script-derived
// vsize; this exported form exists for tests and callers that only
// need a rough pre-submission estimate.
func EstimatePackageFee(parentTx *wire.MsgTx,
	feeRate btcutil.Amount) (btcutil.Amount, error) {

	defaultChildVSize := estimateChildVSize(nil, nil)

	return computePackageFee(parentTx, feeRate, defaultChildVSize)
}

// BuildCPFPChild constructs an unsigned CPFP child that spends an anchor
// output and one confirmed wallet fee input.
func BuildCPFPChild(parentVersion int32,
	anchorOutpoint wire.OutPoint, anchorOutput *wire.TxOut,
	feeInput *FeeInput, changePkScript []byte,
	totalFee btcutil.Amount) (*wire.MsgTx, error) {

	if feeInput == nil || feeInput.Output == nil {
		return nil, fmt.Errorf("fee input and output required")
	}

	if !feeInput.Confirmed {
		return nil, fmt.Errorf("fee input must be confirmed")
	}

	childTx := wire.NewMsgTx(parentVersion)
	childTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: anchorOutpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})

	// The fee input signals RBF (sequence MaxTxInSequenceNum - 2 =
	// 0xfffffffd). For a v3 / TRUC parent this is redundant — v3
	// itself implies replaceability — but it acts as a belt-and-
	// suspenders for any future caller that somehow slips past the
	// Submit-time version gate with a v2 parent: the child would
	// still signal BIP-125 replacement so the next fee bump could
	// RBF it on non-TRUC relays. The anchor input keeps its sentinel
	// sequence because the anchor is anyone-can-spend with no
	// timelock semantics, so its sequence value is not load-bearing.
	childTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: feeInput.Outpoint,
		Sequence:         wire.MaxTxInSequenceNum - 2,
	})

	changeValue := btcutil.Amount(feeInput.Output.Value) - totalFee
	if changeValue < 0 {
		return nil, fmt.Errorf("fee input value %d insufficient for "+
			"fee %d", feeInput.Output.Value, int64(totalFee))
	}

	if changeValue >= DustLimit {
		childTx.AddTxOut(&wire.TxOut{
			Value:    int64(changeValue),
			PkScript: append([]byte(nil), changePkScript...),
		})
	}

	return childTx, nil
}

// EstimateWeight computes the transaction weight including witness data.
func EstimateWeight(tx *wire.MsgTx) int64 {
	baseSize := int64(tx.SerializeSizeStripped())
	totalSize := int64(tx.SerializeSize())

	return baseSize*3 + totalSize
}

// SelectFeeInput selects the smallest confirmed fee input that meets the
// minimum value, excluding any outpoints in the exclude set.
func SelectFeeInput(inputs []FeeInput, minValue btcutil.Amount,
	exclude map[wire.OutPoint]bool) (*FeeInput, error) {

	var best *FeeInput
	for i := range inputs {
		input := &inputs[i]
		if !input.Confirmed || input.Output == nil {
			continue
		}

		if exclude != nil && exclude[input.Outpoint] {
			continue
		}

		if btcutil.Amount(input.Output.Value) < minValue {
			continue
		}

		if best == nil || input.Output.Value < best.Output.Value {
			cp := *input
			best = &cp
		}
	}

	if best == nil {
		return nil, fmt.Errorf("no confirmed fee input "+
			"with at least %d sat", int64(minValue))
	}

	return best, nil
}

// IsIgnorableBroadcastError returns true for errors that indicate the
// transaction is already known to the network.
func IsIgnorableBroadcastError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()
	ignorable := []string{
		"already in block chain",
		"already known",
		"txn-already-in-mempool",
		"transaction already exists",
	}

	for _, pattern := range ignorable {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}

	return false
}

// isPackageSubmissionUnsupported returns true if the error indicates the chain
// backend does not support atomic package submission.
func isPackageSubmissionUnsupported(err error) bool {
	if err == nil {
		return false
	}

	return strings.Contains(err.Error(), "not supported")
}
