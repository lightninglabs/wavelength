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
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	"github.com/lightninglabs/darepo-client/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// DefaultMaxFeeRateSatPerVByte clamps fee estimates to a sane ceiling.
	// On regtest or freshly synced nodes, the estimator can return wildly
	// inflated rates.
	DefaultMaxFeeRateSatPerVByte int64 = 100

	// ChildVSizeEstimate is the estimated virtual size of a CPFP child with
	// one anchor input, one confirmed fee input, and a single change
	// output. Revisit this estimate if the child input composition changes.
	ChildVSizeEstimate = 155

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

// Wallet provides the wallet operations needed by the broadcaster for CPFP fee
// payment.
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
func (b *CPFPBroadcaster) Evict(txid chainhash.Hash) {
	delete(b.parentStates, txid)
}

// reserveFeeOutpoint records that the given parent txid is consuming the
// supplied wallet outpoint. It lazily initialises the parent's entry in
// parentStates if needed.
func (b *CPFPBroadcaster) reserveFeeOutpoint(parentTxid chainhash.Hash,
	op wire.OutPoint) {

	state := b.parentStates[parentTxid]
	if state == nil {
		state = &parentBumpState{
			UsedFeeOutpoints: make(map[wire.OutPoint]struct{}),
		}
		b.parentStates[parentTxid] = state
	}
	if state.UsedFeeOutpoints == nil {
		state.UsedFeeOutpoints = make(map[wire.OutPoint]struct{})
	}
	state.UsedFeeOutpoints[op] = struct{}{}
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

	feeRate, err := b.EstimateFeeRate(ctx)
	if err != nil {
		return nil, fmt.Errorf("estimate fee: %w", err)
	}

	totalFee, err := EstimatePackageFee(req.Tx, btcutil.Amount(feeRate))
	if err != nil {
		return nil, fmt.Errorf("estimate package fee: %w", err)
	}

	feeRate, totalFee = b.applyReplacementFloor(
		req.Tx, txid, feeRate, totalFee,
	)

	feeInput, err := b.selectFeeInput(ctx, txid, totalFee)
	if err != nil {
		return nil, fmt.Errorf("%w: %w",
			ErrCPFPFeeInputUnavailable, err,
		)
	}

	b.reserveFeeOutpoint(txid, feeInput.Outpoint)

	changePkScript, err := b.deriveChangePkScript(ctx)
	if err != nil {
		return b.fallbackDirectBroadcast(
			ctx, req, txid, "derive_change_pkscript", err,
		)
	}

	anchorOutpoint := wire.OutPoint{Hash: txid, Index: uint32(anchorIdx)}
	anchorOutput := req.Tx.TxOut[anchorIdx]

	child, err := BuildCPFPChild(
		req.Tx.Version, anchorOutpoint, anchorOutput, feeInput,
		changePkScript, totalFee,
	)
	if err != nil {
		return b.fallbackDirectBroadcast(
			ctx, req, txid, "build_cpfp_child", err,
		)
	}

	err = b.signCPFPChild(
		ctx, child, anchorOutpoint, anchorOutput, feeInput,
	)
	if err != nil {
		return b.fallbackDirectBroadcast(
			ctx, req, txid, "sign_cpfp_child", err,
		)
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
				return nil, fmt.Errorf(
					"broadcast fallback: %w",
					err,
				)
			}

		default:
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
func (b *CPFPBroadcaster) applyReplacementFloor(parent *wire.MsgTx,
	txid chainhash.Hash, feeRate int64,
	totalFee btcutil.Amount) (int64, btcutil.Amount) {

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
	packageVSize := parentVSize + int64(ChildVSizeEstimate)

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
func (b *CPFPBroadcaster) fallbackDirectBroadcast(ctx context.Context,
	req *BroadcastRequest, txid chainhash.Hash, stage string,
	err error) (*BroadcastResult, error) {

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
// the replacement actually lands on the mempool.
func (b *CPFPBroadcaster) selectFeeInput(ctx context.Context,
	parentTxid chainhash.Hash,
	minAmount btcutil.Amount) (*FeeInput, error) {

	if b.cfg.Wallet == nil {
		return nil, fmt.Errorf("wallet must be provided")
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
			return &FeeInput{
				Outpoint: best.Outpoint,
				Output: &wire.TxOut{
					Value:    int64(best.Amount),
					PkScript: best.PkScript,
				},
				Confirmed: true,
			}, nil
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

// EstimatePackageFee computes the total package fee for one parent+child
// submission at the given fee rate.
func EstimatePackageFee(parentTx *wire.MsgTx,
	feeRate btcutil.Amount) (btcutil.Amount, error) {

	if parentTx == nil {
		return 0, fmt.Errorf("parent tx cannot be nil")
	}

	if feeRate <= 0 {
		return 0, fmt.Errorf("fee rate must be positive")
	}

	parentWeight := EstimateWeight(parentTx)
	parentVSize := (parentWeight + 3) / 4
	totalFee := feeRate * btcutil.Amount(parentVSize+ChildVSizeEstimate)
	if totalFee < 1 {
		return 1, nil
	}

	return totalFee, nil
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
	childTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: feeInput.Outpoint,
		Sequence:         wire.MaxTxInSequenceNum,
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
