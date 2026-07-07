package txconfirm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	"github.com/lightninglabs/darepo-client/walletcore"
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

	// minRelayFeeRateSatPerVByte is Bitcoin Core's default minimum relay
	// fee rate (minrelaytxfee, 1000 sat/kvB). A transaction paying less
	// than this per vbyte is rejected by every default-configured
	// mempool, so a CPFP child whose residual fee falls under
	// vsize * this rate is doomed at relay and not worth building.
	minRelayFeeRateSatPerVByte int64 = 1

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
	ErrCPFPFeeInputUnavailable = errors.New("cpfp fee input unavailable")

	// ErrFeeInputProducesDust indicates that the selected fee input
	// covers the package fee but leaves a change output below the dust
	// limit. The CPFP child's only non-anchor output is that change, so a
	// sub-dust remainder would yield a zero-output transaction that the
	// wallet cannot finalize. Callers should select a larger fee input,
	// one covering totalFee + DustLimit, instead.
	ErrFeeInputProducesDust = errors.New("cpfp fee input leaves sub-dust " +
		"change")

	errCPFPFeeInputShortfall = errors.New("confirmed cpfp fee input " +
		"shortfall")

	// ErrNonTRUCParent indicates that the caller submitted a parent
	// carrying a zero-value ephemeral anchor whose version is not v3
	// (TRUC). The ephemeral-anchor CPFP strategy relies on BIP-431
	// package semantics: without v3, package RBF replacement rules and
	// the zero-fee anchor are not policy-legal on a standard Bitcoin
	// Core mempool. We therefore reject non-v3 ephemeral-anchor parents
	// at the Submit boundary rather than silently attaching a CPFP
	// child that would never relay. Funded-anchor parents (non-zero
	// anchor value) pay their own fee and are exempt: they broadcast as
	// ordinary v2 transactions and only spend the anchor when a bump is
	// requested.
	ErrNonTRUCParent = errors.New("parent transaction must be v3 (TRUC) " +
		"for CPFP broadcast")

	// ErrParentFeeSufficient indicates that a fee-bump request resolved
	// to a target package fee the parent already pays on its own, so the
	// CPFP child's share would fall below what a mempool will relay. The
	// bump is refused as a no-op rather than burning a wallet round-trip
	// on a child that is guaranteed a min-relay-fee reject. Callers
	// should treat this as "nothing to do at this rate": either the fee
	// estimator has fallen to (or below) the parent's own rate, or an
	// operator supplied a target at or under it.
	ErrParentFeeSufficient = errors.New("parent fee already meets target " +
		"package fee; cpfp child would not relay")

	// ErrParentAlreadyBroadcast indicates that the SubmitPackage RPC
	// reported the parent transaction as already known to the network
	// while our CPFP child failed to land (e.g. RBF-replaced by a
	// higher-fee child or its anchor input was already spent). The
	// parent will confirm via whichever fee-bump won the race, so the
	// caller should keep watching for confirmation rather than treat
	// this as a terminal broadcast failure.
	ErrParentAlreadyBroadcast = errors.New("parent already broadcast by " +
		"another path; cpfp child rejected")
)

// txconfirmLockID is the package-scoped LockID used by CPFPBroadcaster
// when leasing fee-input UTXOs via the Wallet interface. It is derived
// from the ASCII string "darepo-client:txconfirm" padded to 32 bytes
// so concurrent subsystems using a different LockID prefix cannot
// release txconfirm's leases by mistake. The value is a compile-time
// constant: callers do not need to synchronise LockIDs across restarts
// because the broadcaster already rebuilds its in-memory reservation
// state from per-parent FSM progress on recovery.
var txconfirmLockID = func() walletcore.LockID {
	var id walletcore.LockID
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
	ListUnspent(ctx context.Context, minConfs,
		maxConfs int32) ([]*walletcore.Utxo, error)

	// NewWalletPkScript returns a fresh wallet-managed pkScript
	// suitable for
	// change outputs.
	NewWalletPkScript(ctx context.Context) ([]byte, error)

	// FinalizePsbt signs and finalizes a PSBT packet. The wallet signs all
	// inputs it owns and returns the finalized wire tx.
	FinalizePsbt(ctx context.Context, packet []byte) (*wire.MsgTx, error)

	// FundPsbt funds, signs, and finalizes a wallet transaction template.
	// The lock parameters let backends reserve selected inputs under the
	// same namespace txconfirm uses for CPFP fee inputs.
	FundPsbt(ctx context.Context, packet []byte, feeRateSatPerVByte int64,
		lockID walletcore.LockID,
		lockExpiry time.Duration) (*wire.MsgTx, error)

	walletcore.OutputLeaser
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

	// IsFeeBump distinguishes an initial broadcast from a fee-bump
	// re-attempt. It only matters for a funded-anchor parent: such a
	// parent is independently valid and pays its own miner fee, so its
	// initial broadcast goes out directly (no CPFP child, no fee-input
	// reservation) and a CPFP child is built only when a bump is actually
	// needed. A zero-value ephemeral-anchor parent ignores this flag: it
	// pays zero fee and must always ride a CPFP package, on the initial
	// submission and on every bump.
	IsFeeBump bool

	// ParentFee is the absolute miner fee the parent transaction already
	// pays, in satoshis. It is used only on the funded-anchor CPFP path to
	// avoid double-counting the parent's fee: the CPFP child pays the
	// package-fee target minus what the parent already contributes, so the
	// combined parent+child fee lands on the requested rate rather than
	// overshooting it by the parent's own fee. Zero (the default, and the
	// only value used by the zero-fee ephemeral path) makes the child pay
	// the full package fee, which is correct when the parent pays nothing.
	ParentFee btcutil.Amount

	// TargetFeeRateSatPerVByte, when positive, overrides the estimator and
	// forces the CPFP package to a specific fee rate, clamped to
	// MaxFeeRateSatPerVByte. It is how an operator-driven "bump now to this
	// rate" request reaches the broadcaster. Zero defers to the fee
	// estimator, the default behaviour for interval-paced bumps.
	TargetFeeRateSatPerVByte int64
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

	// PredictedFeeInputs are outputs from an in-flight txconfirm fanout
	// transaction that are already assigned to this parent. They become
	// normal used fee inputs after the fanout confirms.
	PredictedFeeInputs map[wire.OutPoint]*FeeInput
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

	// suspectFeeInputs records wallet outpoints whose CPFP child failed
	// during signing. Selection deprioritizes these so a coin the signer
	// has refused (e.g. an imported watch-only script output that leaked
	// through the backend's UTXO enumeration) cannot keep winning the
	// smallest-first pick on every retry while signable coins sit idle.
	// A suspect is still used as a last resort when no other candidate
	// qualifies, so a transient signing failure (wallet sync lag) cannot
	// permanently starve fee bumping, and the mark is cleared the moment
	// the input finalizes successfully.
	suspectFeeInputs map[wire.OutPoint]struct{}
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
		cfg:              cfg,
		log:              cfg.Log.UnwrapOr(btclog.Disabled),
		parentStates:     make(map[chainhash.Hash]*parentBumpState),
		suspectFeeInputs: make(map[wire.OutPoint]struct{}),
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
func (b *CPFPBroadcaster) Evict(ctx context.Context, txid chainhash.Hash) {
	state, ok := b.parentStates[txid]
	if !ok {
		return
	}

	outpoints := make([]wire.OutPoint, 0, len(state.UsedFeeOutpoints))
	for op := range state.UsedFeeOutpoints {
		outpoints = append(outpoints, op)
	}
	for op := range state.PredictedFeeInputs {
		outpoints = append(outpoints, op)
	}

	delete(b.parentStates, txid)
	b.releaseWalletLeasesAsync(ctx, outpoints)
}

func (b *CPFPBroadcaster) parentState(
	parentTxid chainhash.Hash) *parentBumpState {

	state := b.parentStates[parentTxid]
	if state == nil {
		state = &parentBumpState{}
		b.parentStates[parentTxid] = state
	}
	if state.UsedFeeOutpoints == nil {
		state.UsedFeeOutpoints = make(map[wire.OutPoint]struct{})
	}
	if state.UsedFeeInputs == nil {
		state.UsedFeeInputs = make(map[wire.OutPoint]*FeeInput)
	}
	if state.PredictedFeeInputs == nil {
		state.PredictedFeeInputs = make(map[wire.OutPoint]*FeeInput)
	}

	return state
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

	state := b.parentState(parentTxid)
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
	delete(state.PredictedFeeInputs, op)

	b.releaseWalletLeasesAsync(ctx, []wire.OutPoint{op})

	// If the parent has no fee history and no remaining reservations,
	// drop the empty entry entirely so parentStates does not accumulate
	// zero-value shells.
	if state.LastFeeRate == 0 && state.LastPackageFee == 0 &&
		len(state.UsedFeeOutpoints) == 0 &&
		len(state.UsedFeeInputs) == 0 &&
		len(state.PredictedFeeInputs) == 0 {

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
		for op := range state.PredictedFeeInputs {
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
// The anchor's value picks the strategy. A zero-value ephemeral anchor
// marks a BIP-431 TRUC parent that pays no fee of its own: such parents
// must be v3 (non-v3 is rejected with ErrNonTRUCParent, since package RBF
// and the zero-fee anchor are not policy-legal without TRUC semantics)
// and always ride a CPFP package. A funded (non-zero) anchor marks an
// independently valid parent that pays its own fee: it broadcasts
// directly on the initial pass regardless of version, and the anchor is
// only spent when a fee bump is requested. Anchorless transactions
// broadcast directly with no fee-bump handle at all.
func (b *CPFPBroadcaster) Submit(ctx context.Context, height int32,
	req *BroadcastRequest) (*BroadcastResult, error) {

	if req == nil || req.Tx == nil {
		return nil, fmt.Errorf("broadcast request and tx required")
	}

	txid := req.Tx.TxHash()
	anchorIdx := findAnchorOutput(req.Tx)

	switch {
	// No anchor at all: a plain transaction with no CPFP handle. Broadcast
	// it directly and let it ride; there is nothing to fee-bump.
	case anchorIdx < 0:
		return b.broadcastDirect(ctx, req, txid)

	// Funded anchor: the parent pays its own fee and is independently
	// valid, so it does not need TRUC/package semantics. On the initial
	// broadcast we send the parent directly and reserve no fee input — the
	// anchor is a spare handle we only spend when a bump is requested. On a
	// fee-bump we build the CPFP child off the funded anchor.
	case anchorIsFunded(req.Tx, anchorIdx):
		if !req.IsFeeBump {
			return b.broadcastDirect(ctx, req, txid)
		}

		return b.broadcastWithCPFP(ctx, height, req, txid, anchorIdx)

	// Zero-value ephemeral anchor: the parent pays zero fee and can only
	// relay as part of a CPFP package, so it must be v3 (TRUC) and always
	// rides a child — on the initial submission and on every bump.
	default:
		if req.Tx.Version != arktx.TxVersion {
			return nil, fmt.Errorf("%w: got version %d, want %d",
				ErrNonTRUCParent, req.Tx.Version,
				arktx.TxVersion)
		}

		return b.broadcastWithCPFP(ctx, height, req, txid, anchorIdx)
	}
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

		return fmt.Errorf("testmempoolaccept rejected %s: %s", r.Txid,
			r.Reason)
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
func (b *CPFPBroadcaster) broadcastWithCPFP(ctx context.Context, height int32,
	req *BroadcastRequest, txid chainhash.Hash, anchorIdx int) (
	*BroadcastResult, error) {

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

	// An operator-supplied target rate (a "bump now to this rate" request)
	// overrides the estimator; otherwise we use the current estimate. Both
	// are clamped to MaxFeeRateSatPerVByte inside the respective helpers so
	// a stuck-tx bump can never blow past the configured ceiling.
	feeRate, err := b.targetOrEstimatedFeeRate(
		ctx, req.TargetFeeRateSatPerVByte,
	)
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

	// A funded parent may already pay the whole package target on its own
	// (a flat-rate re-bump, or an operator target at or under the parent's
	// own rate). The child's residual share would then sit below the
	// minimum relay fee for its vsize and every mempool would reject it,
	// so refuse the bump up front as a no-op rather than deriving scripts,
	// leasing a fee input, and signing a child that is doomed at relay.
	// Zero-fee ephemeral parents (ParentFee == 0) never trip this: their
	// child always carries the full package fee.
	if req.ParentFee > 0 {
		childFee := childFeeFromPackageFee(totalFee, req.ParentFee)
		minChildFee := btcutil.Amount(
			childVSize * minRelayFeeRateSatPerVByte,
		)
		if childFee < minChildFee {
			return nil, fmt.Errorf("%w: parent pays %d of %d sat "+
				"package fee at %d sat/vB",
				ErrParentFeeSufficient, req.ParentFee, totalFee,
				feeRate)
		}
	}

	// The child's own fee is the package target minus what the parent
	// already pays; that (not the full package fee) is what the wallet
	// input must fund. The anchor's value is also credited into the
	// child's change, so it reduces the wallet's share further. Selecting
	// against the full package fee on a funded parent would reject
	// perfectly adequate UTXOs and surface a false fee-input shortage.
	anchorOutpoint := wire.OutPoint{Hash: txid, Index: uint32(anchorIdx)}
	anchorOutput := req.Tx.TxOut[anchorIdx]
	anchorValue := btcutil.Amount(anchorOutput.Value)
	childFee := childFeeFromPackageFee(totalFee, req.ParentFee)

	// Select a fee input that covers the child's fee *plus* a spendable
	// (non-dust) change output. Selecting for the fee alone can pick a
	// UTXO whose post-fee remainder is below the dust limit, which would
	// force BuildCPFPChild to drop its only output and produce a
	// zero-output child that FinalizePsbt rejects. Requiring the dust
	// buffer up front skips near-dust UTXOs and lands on one that always
	// yields a valid change output.
	feeInput, err := b.selectFeeInput(
		ctx, txid, childFeeInputTarget(childFee, anchorValue),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCPFPFeeInputUnavailable,
			err)
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
			// totalFee remains the combined package fee recorded
			// for BIP-125 replacement accounting; childFee tracks
			// the child's own share of it.
			totalFee = preciseFee
			childFee = childFeeFromPackageFee(
				totalFee, req.ParentFee,
			)

			// Growing the fee can push the already-selected
			// input's change below the dust limit. Reselect
			// against the precise threshold so we still land
			// on an input that yields a spendable change
			// output rather than tripping the dust guard and
			// re-picking the same too-small input on every
			// retry. The reservation cache keeps the current
			// input when it already covers the larger fee.
			reselected, selErr := b.selectFeeInput(
				ctx, txid,
				childFeeInputTarget(childFee, anchorValue),
			)
			if selErr != nil {
				return b.fallbackDirectBroadcast(
					ctx, req, txid, feeInput.Outpoint,
					"reselect_fee_input", selErr,
				)
			}
			if reselected.Outpoint != feeInput.Outpoint {
				feeInput = reselected
				b.reserveFeeInput(ctx, txid, feeInput)
			}
		}
	}

	child, err := BuildCPFPChild(
		req.Tx.Version, anchorOutpoint, anchorOutput, feeInput,
		changePkScript, childFee,
	)
	if err != nil {
		return b.fallbackDirectBroadcast(
			ctx, req, txid, feeInput.Outpoint, "build_cpfp_child",
			err,
		)
	}

	err = b.signCPFPChild(
		ctx, child, anchorOutpoint, anchorOutput, feeInput,
	)
	if err != nil {
		return b.fallbackDirectBroadcast(
			ctx, req, txid, feeInput.Outpoint, stageSignCPFPChild,
			err,
		)
	}

	// The input signed cleanly, so clear any suspicion recorded from an
	// earlier transient failure (e.g. wallet sync lag at first pick).
	delete(b.suspectFeeInputs, feeInput.Outpoint)

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
				ctx, "Package already known for "+
					txid.String(),
			)

		case isPackageSubmissionUnsupported(pkgErr):
			if err := b.broadcastIndividually(
				ctx, req.Tx, child, req.Label,
			); err != nil {

				b.releaseFeeOutpoint(
					ctx, txid, feeInput.Outpoint,
				)

				return nil, fmt.Errorf("broadcast fallback: %w",
					err)
			}

		case isParentKnownChildFailed(txid, pkgErr):
			// The parent is already in mempool or chain via
			// another CPFP attempt, but our child was rejected
			// (RBF replacement of a higher-fee child, or its
			// anchor input was already spent). The parent will
			// confirm via the existing path, so release our fee
			// reservation and surface a distinct sentinel — the
			// caller keeps the conf watch live rather than
			// failing the broadcast.
			b.releaseFeeOutpoint(ctx, txid, feeInput.Outpoint)

			return nil, fmt.Errorf("%w: %w",
				ErrParentAlreadyBroadcast, pkgErr)

		default:
			// The package was rejected wholesale: the child
			// did not land in the mempool, so the fee input
			// is a stale reservation that should be released.
			b.releaseFeeOutpoint(ctx, txid, feeInput.Outpoint)

			return nil, fmt.Errorf("submit package: %w", pkgErr)
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
	txid chainhash.Hash, feeRate int64, totalFee btcutil.Amount,
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

// stageSignCPFPChild names the CPFP setup stage where the child PSBT is
// signed and finalized. A failure at this stage indicts the selected fee
// input specifically (the wallet refused to sign it), which is why
// fallbackDirectBroadcast marks the input as suspect for this stage only.
const stageSignCPFPChild = "sign_cpfp_child"

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
	releaseOutpoint wire.OutPoint, stage string, err error) (
	*BroadcastResult, error) {

	b.releaseFeeOutpoint(ctx, txid, releaseOutpoint)

	// A signing failure means the wallet refused this specific input;
	// deprioritize it so the next selection prefers a coin with a clean
	// history. Other stages (fee estimation, change derivation, child
	// construction) do not indict the input.
	if stage == stageSignCPFPChild &&
		releaseOutpoint != (wire.OutPoint{}) {

		b.suspectFeeInputs[releaseOutpoint] = struct{}{}
	}

	// A fee bump of a funded-anchor parent must never take the direct
	// fallback: the parent relayed on its own fee and is (or was) already
	// in the mempool, so re-broadcasting it is answered with an ignorable
	// "already known" reject and the call would report success with no
	// child submitted. Surfacing the setup failure instead lets the caller
	// report an honest Bumped=false with the real reason. The fallback
	// remains the right hail-mary for a zero-fee ephemeral parent, whose
	// only route into a mempool is a package, and for initial funded
	// broadcasts, which are direct anyway.
	if req.IsFeeBump {
		anchorIdx := findAnchorOutput(req.Tx)
		if anchorIdx >= 0 && anchorIsFunded(req.Tx, anchorIdx) {
			return nil, fmt.Errorf("cpfp setup failed at %s: %w",
				stage, err)
		}
	}

	b.log.WarnS(ctx, "CPFP unavailable; broadcasting parent directly",
		err, "txid", txid, "stage", stage, "label", req.Label)

	result, directErr := b.broadcastDirect(ctx, req, txid)
	if directErr == nil {
		return result, nil
	}

	// Anchor parents often have zero fee and only relay once the CPFP child
	// is accepted. If the direct fallback also fails, keep the tracked tx
	// non-terminal so the caller can retry on later blocks or observe an
	// external package that won the race.
	return nil, fmt.Errorf("%w: %s: %w; direct parent broadcast: %w",
		ErrCPFPFeeInputUnavailable, stage, err, directErr)
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
		b.log.Warnf("Fee estimation failed, using fallback %d "+
			"sat/vB: %v", minFeeRateSatPerVByte, err)

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

// targetOrEstimatedFeeRate returns the fee rate to use for a CPFP package. A
// positive target (an operator-driven "bump now to this rate" request)
// overrides the estimator and is clamped to MaxFeeRateSatPerVByte so a manual
// bump can never exceed the configured ceiling; a non-positive target defers
// to the estimator, which applies the same clamp.
func (b *CPFPBroadcaster) targetOrEstimatedFeeRate(ctx context.Context,
	targetRate int64) (int64, error) {

	if targetRate <= 0 {
		return b.EstimateFeeRate(ctx)
	}

	if b.cfg.MaxFeeRateSatPerVByte > 0 &&
		targetRate > b.cfg.MaxFeeRateSatPerVByte {

		targetRate = b.cfg.MaxFeeRateSatPerVByte
	}

	return targetRate, nil
}

// childFeeFromPackageFee splits the combined package fee into the portion the
// CPFP child must pay, given the fee the parent already contributes. The
// parent's fee is subtracted so the combined parent+child fee equals the
// package target rather than overshooting it; the result is floored at one
// satoshi so the child always pays a positive fee even when the parent's fee
// already covers (or exceeds) the target, which can happen on a flat-rate
// re-bump.
func childFeeFromPackageFee(
	packageFee, parentFee btcutil.Amount) btcutil.Amount {

	childFee := packageFee - parentFee
	if childFee < 1 {
		childFee = 1
	}

	return childFee
}

// childFeeInputTarget returns the minimum wallet fee-input value needed to
// fund a CPFP child paying childFee: the fee plus a dust buffer so the change
// output is always spendable, minus the anchor's own value, which is credited
// into the change alongside the fee input. The floor of one satoshi keeps the
// selection meaningful when a large funded anchor covers the whole child fee
// on its own — the child still structurally requires a confirmed wallet input
// (it is what later RBF replacements double-spend), just not a big one.
func childFeeInputTarget(childFee, anchorValue btcutil.Amount) btcutil.Amount {
	target := childFee + DustLimit - anchorValue
	if target < 1 {
		target = 1
	}

	return target
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
	parentTxid chainhash.Hash, minAmount btcutil.Amount) (*FeeInput,
	error) {

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

	var lastSuspect *walletcore.Utxo
	for {
		utxos, err := b.cfg.Wallet.ListUnspent(ctx, 1, 9999999)
		if err != nil {
			return nil, fmt.Errorf("list unspent: %w", err)
		}

		// Track the smallest qualifying candidate in two tiers:
		// coins with a clean signing history, and coins whose CPFP
		// child previously failed to sign. A suspect only wins when
		// no clean candidate qualifies, so one unsignable output can
		// never shadow a signable coin on every retry.
		var best, bestSuspect *walletcore.Utxo
		for _, utxo := range utxos {
			if _, skip := excluded[utxo.Outpoint]; skip {
				continue
			}

			if utxo.Amount < minAmount {
				continue
			}

			_, suspect := b.suspectFeeInputs[utxo.Outpoint]
			if suspect {
				if bestSuspect == nil ||
					utxo.Amount < bestSuspect.Amount {

					bestSuspect = utxo
				}

				continue
			}

			if best == nil || utxo.Amount < best.Amount {
				best = utxo
			}
		}

		if best != nil {
			return feeInputFromWalletUTXO(best), nil
		}
		if bestSuspect != nil {
			lastSuspect = bestSuspect
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

	// No clean candidate materialized within the poll window. Settle for
	// a previously refused coin rather than giving up outright: if the
	// earlier signing failure was transient (wallet sync lag), this
	// retry recovers, and if the coin is genuinely unsignable the sign
	// step fails exactly as it would have before the deprioritization.
	if lastSuspect != nil {
		return feeInputFromWalletUTXO(lastSuspect), nil
	}

	return nil, fmt.Errorf("%w: no confirmed wallet UTXOs available (need "+
		">= %d sats)", errCPFPFeeInputShortfall, int64(minAmount))
}

// selectReservedFeeInput returns the smallest cached fee input already
// reserved for the supplied parent txid that can cover minAmount.
func (b *CPFPBroadcaster) selectReservedFeeInput(parentTxid chainhash.Hash,
	minAmount btcutil.Amount) *FeeInput {

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
func feeInputFromWalletUTXO(utxo *walletcore.Utxo) *FeeInput {
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
func (b *CPFPBroadcaster) deriveChangePkScript(ctx context.Context) ([]byte,
	error) {

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
func (b *CPFPBroadcaster) signCPFPChild(ctx context.Context, child *wire.MsgTx,
	anchorOutpoint wire.OutPoint, anchorOutput *wire.TxOut,
	feeInput *FeeInput) error {

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
			return fmt.Errorf("finalized tx missing input for "+
				"outpoint %s", child.TxIn[i].PreviousOutPoint)
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
		return fmt.Errorf("broadcast parent %s: %w", parentTxid,
			parentErr)
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

// findAnchorOutput returns the index of the P2A anchor output in the
// transaction or -1 if none is found. It matches the anchor by script
// regardless of value, so it locates both the zero-value ephemeral anchor
// used by TRUC parents and the funded anchor used by an independently-valid
// parent. Callers that need to distinguish the two forms inspect the value
// of the returned output (see anchorIsFunded).
func findAnchorOutput(tx *wire.MsgTx) int {
	for i, out := range tx.TxOut {
		if arktx.IsP2AAnchorScript(out.PkScript) {
			return i
		}
	}

	return -1
}

// anchorIsFunded reports whether the anchor at anchorIdx carries a non-zero
// value, i.e. it is the funded form whose parent pays its own fee and can
// confirm standalone, rather than the zero-value ephemeral form that relies
// on a CPFP descendant to fund the package.
func anchorIsFunded(tx *wire.MsgTx, anchorIdx int) bool {
	return arktx.IsFundedAnchorOutput(tx.TxOut[anchorIdx])
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
// output and one confirmed wallet fee input. childFee is the fee the child
// itself pays: for a zero-fee ephemeral parent that is the whole package fee,
// for a funded parent it is the package fee minus what the parent already
// contributes.
func BuildCPFPChild(parentVersion int32, anchorOutpoint wire.OutPoint,
	anchorOutput *wire.TxOut, feeInput *FeeInput, changePkScript []byte,
	childFee btcutil.Amount) (*wire.MsgTx, error) {

	if feeInput == nil || feeInput.Output == nil {
		return nil, fmt.Errorf("fee input and output required")
	}

	if anchorOutput == nil {
		return nil, fmt.Errorf("anchor output required")
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

	// Every value-bearing input must be credited into the change or it is
	// silently donated to miners as fee the caller never asked for. The
	// anchor is such an input on the funded path: a non-zero anchor value
	// recovered here is exactly how the operator gets the anchor's sats
	// back when a bump fires. For a zero-value ephemeral anchor this
	// credit is zero and the arithmetic reduces to the fee input alone.
	anchorValue := btcutil.Amount(anchorOutput.Value)
	changeValue := btcutil.Amount(feeInput.Output.Value) + anchorValue -
		childFee
	if changeValue < 0 {
		return nil, fmt.Errorf("fee input value %d plus anchor value "+
			"%d insufficient for fee %d", feeInput.Output.Value,
			int64(anchorValue), int64(childFee))
	}

	// The CPFP child's only spendable output is this change output. If the
	// remainder after fees is below the dust limit we cannot emit a valid
	// output, so refuse here rather than return a zero-output transaction
	// that the wallet's FinalizePsbt rejects ("PSBT packet must contain at
	// least one output"). Callers select fee inputs against the child fee
	// plus a dust buffer, so this is a defensive backstop against a
	// post-selection fee top-up (mixed-type wallets) eroding the margin.
	if changeValue < DustLimit {
		return nil, fmt.Errorf("%w: fee input %d + anchor %d leaves "+
			"%d change below dust limit %d",
			ErrFeeInputProducesDust, feeInput.Output.Value,
			int64(anchorValue), int64(changeValue),
			int64(DustLimit))
	}

	childTx.AddTxOut(&wire.TxOut{
		Value:    int64(changeValue),
		PkScript: append([]byte(nil), changePkScript...),
	})

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
		return nil, fmt.Errorf("no confirmed fee input with at "+
			"least %d sat", int64(minValue))
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

// parentKnownSentinels are the chain-backend RPC sentinels that signal a
// parent transaction is already in the mempool or chain. When a per-tx
// `*chainbackends.PackageTxError` for the parent unwraps to one of these,
// the parent itself is on the network even if our package as a whole was
// rejected. Sentinel matching (via `errors.Is`) is preferred over
// substring-matching because it is normalised across btcd / bitcoind
// reject-reason variants by `rpcclient.MapRPCErr`.
var parentKnownSentinels = []error{
	rpcclient.ErrTxAlreadyKnown,
	rpcclient.ErrTxAlreadyInMempool,
	rpcclient.ErrTxAlreadyConfirmed,
}

// childFailureSentinels are the RPC sentinels that signal our CPFP child
// failed to land while the parent succeeded — typically because another
// CPFP child for the same parent won the mempool race, or the parent's own
// presence makes our child's inputs / fee policy invalid.
var childFailureSentinels = []error{
	rpcclient.ErrInsufficientFee,
	rpcclient.ErrMissingInputsOrSpent,
	rpcclient.ErrMissingInputs,
	rpcclient.ErrMempoolConflict,
	rpcclient.ErrConflictingTx,
}

// parentLikelyConfirmedSentinels are RPC sentinels that signal a parent
// transaction's inputs are already spent on chain. When a per-tx
// `*chainbackends.PackageTxError` for the parent unwraps to one of
// these, the most likely explanation is that the parent itself already
// confirmed deeply enough that bitcoind has dropped its txid from the
// recent-rejects / mempool cache.
//
// The same sentinels also fire if the parent's input was double-spent
// by a *competing* transaction. In that case the parent is genuinely
// dead and the existing confirmation watch will never fire for the
// parent txid. The actor falls back to the standard
// AwaitingConfirmation timeout / external spend detection paths to
// notice this and surface TxFailed; treating the per-package error as
// "parent already broadcast" simply means we don't collapse to Failed
// on the package response itself.
var parentLikelyConfirmedSentinels = []error{
	rpcclient.ErrMissingInputsOrSpent,
	rpcclient.ErrMissingInputs,
}

// isAnySentinel reports whether `err` matches any of `sentinels` via
// `errors.Is`.
func isAnySentinel(err error, sentinels []error) bool {
	for _, s := range sentinels {
		if errors.Is(err, s) {
			return true
		}
	}

	return false
}

// isParentKnownChildFailed returns true when a `SubmitPackage` error
// indicates that the only failure is on the CPFP child, with the parent
// already broadcast by another path.
//
// The package error from btcwbackend / chainbackends.handlePackageResult
// is `errors.Join`'d from one `*chainbackends.PackageTxError` per per-tx
// result. Each entry unwraps to a `rpcclient.BitcoindRPCErr` sentinel via
// `rpcclient.MapRPCErr` (or `rpcclient.ErrUndefined` when the reason is
// unknown), which lets us classify with `errors.Is` instead of grepping
// the raw reject string.
//
// We accept this state if any of:
//
//  1. A `PackageTxError` whose `Txid` equals `parentTxid` unwraps to a
//     parent-known sentinel AND any other entry unwraps to a
//     child-failure sentinel; OR
//
//  2. A `PackageTxError` whose `Txid` equals `parentTxid` unwraps to a
//     parent-likely-confirmed sentinel AND any other entry unwraps to a
//     child-failure sentinel; OR
//
//  3. (Fallback) no entry for `parentTxid` is present at all. Some
//     bitcoind versions silently accept the already-known parent and
//     only echo the rejected child, so the parent's row disappears
//     from `tx-results`. In that case any `*PackageTxError` whose
//     reason mentions an RBF replacement reject (`rejecting
//     replacement`) is sufficient — RBF can only fire when the
//     conflicting tx already sits in mempool. This is the one place
//     where we still substring-match, because bitcoind's "rejecting
//     replacement; new feerate ... <= old feerate ..." trace string is
//     not yet mapped to a typed sentinel by the upstream chain package.
//
// The fallback is gated on `!parentSeen` rather than firing whenever
// the RBF marker shows up: if bitcoind echoed the parent with a fatal
// (non-known) reason that is not in the likely-confirmed set, the parent is
// genuinely broken and we must NOT silently swallow that as "already
// broadcast".
func isParentKnownChildFailed(parentTxid chainhash.Hash, err error) bool {
	if err == nil {
		return false
	}

	var (
		parentSeen            bool
		parentKnown           bool
		parentLikelyConfirmed bool
		childFailed           bool
		rbfReplaceOk          bool
	)

	visit := func(pte *chainbackends.PackageTxError) {
		switch {
		case pte.Txid == parentTxid:
			parentSeen = true
			switch {
			case isAnySentinel(pte, parentKnownSentinels):
				parentKnown = true

			case isAnySentinel(pte, parentLikelyConfirmedSentinels):
				parentLikelyConfirmed = true
			}

		case isAnySentinel(pte, childFailureSentinels):
			childFailed = true
		}

		if strings.Contains(pte.Reason, "rejecting replacement") {
			rbfReplaceOk = true
		}
	}
	chainbackends.WalkPackageTxErrors(err, visit)

	if parentKnown && childFailed {
		return true
	}

	if parentLikelyConfirmed && childFailed {
		return true
	}

	return !parentSeen && rbfReplaceOk
}
