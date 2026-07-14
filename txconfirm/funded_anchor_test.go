package txconfirm

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/walletcore"
	"github.com/stretchr/testify/require"
)

// makeFundedAnchorTx builds a v2 transaction carrying a funded (non-zero) P2A
// anchor, modelling an independently-valid commitment-style parent that pays
// its own fee and reserves the anchor as a CPFP handle.
func makeFundedAnchorTx(seed byte, anchorValue int64) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{seed},
			Index: 0,
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    100_000,
		PkScript: []byte{txscript.OP_TRUE},
	})
	tx.AddTxOut(
		arkscript.AnchorOutput(
			arkscript.WithAnchorValue(anchorValue),
		),
	)

	return tx
}

// TestSubmitFundedAnchorInitialBroadcastsDirect asserts that the initial
// submission of a funded-anchor parent goes out as a plain direct broadcast:
// the parent is independently valid, so no CPFP child is built and no fee
// input is reserved until a bump is actually requested.
func TestSubmitFundedAnchorInitialBroadcastsDirect(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet: &fakeWallet{
			utxos: []*walletcore.Utxo{makeWalletUTXO(t)},
		},
	})

	result, err := b.Submit(t.Context(), 100, &BroadcastRequest{
		Tx:        makeFundedAnchorTx(7, 330),
		Label:     "funded-initial",
		IsFeeBump: false,
	})
	require.NoError(t, err)

	// A direct broadcast leaves no CPFP child and submits no package.
	require.Nil(t, result.ChildTxid)
	require.Equal(t, 1, chain.broadcastCallCount())
	require.Equal(t, 0, chain.packageCallCount())
}

// TestSubmitFundedAnchorFeeBumpBuildsChild asserts that a fee-bump submission
// of a funded-anchor parent (the path a stuck-tx bump drives) builds and
// submits a CPFP child spending the funded anchor.
func TestSubmitFundedAnchorFeeBumpBuildsChild(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet: &fakeWallet{
			utxos: []*walletcore.Utxo{makeWalletUTXO(t)},
		},
	})

	result, err := b.Submit(t.Context(), 100, &BroadcastRequest{
		Tx:        makeFundedAnchorTx(8, 330),
		Label:     "funded-bump",
		IsFeeBump: true,
		ParentFee: 500,
	})
	require.NoError(t, err)

	// A fee bump submits a parent+child package and surfaces the child.
	require.NotNil(t, result.ChildTxid)
	require.Equal(t, 1, chain.packageCallCount())
}

// TestSubmitFundedAnchorV2NotRejected asserts that a v2 funded-anchor parent is
// accepted rather than rejected by the TRUC version gate, which only applies to
// zero-value ephemeral anchors.
func TestSubmitFundedAnchorV2NotRejected(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	b := NewCPFPBroadcaster(BroadcasterConfig{ChainSource: chain})

	tx := makeFundedAnchorTx(9, 330)
	require.EqualValues(t, 2, tx.Version)

	_, err := b.Submit(t.Context(), 100, &BroadcastRequest{
		Tx:        tx,
		Label:     "funded-v2",
		IsFeeBump: false,
	})
	require.NoError(t, err)
	require.NotErrorIs(t, err, ErrNonTRUCParent)
}

// TestChildFeeFromPackageFee asserts the package fee is split so the child pays
// only the shortfall the parent does not already cover, with a positive floor.
func TestChildFeeFromPackageFee(t *testing.T) {
	t.Parallel()

	// Parent pays nothing (ephemeral): the child pays the whole package.
	require.EqualValues(
		t, 1_000, childFeeFromPackageFee(1_000, 0),
	)

	// Funded parent: the child pays the package fee minus the parent's fee.
	require.EqualValues(
		t, 700, childFeeFromPackageFee(1_000, 300),
	)

	// Parent fee already covers (or exceeds) the package target: the child
	// still pays a positive floor of one satoshi.
	require.EqualValues(
		t, 1, childFeeFromPackageFee(1_000, 1_000),
	)
	require.EqualValues(
		t, 1, childFeeFromPackageFee(1_000, 5_000),
	)
}

// TestTargetOrEstimatedFeeRate asserts an operator-supplied target overrides
// the estimator and is clamped to the configured maximum, while a non-positive
// target defers to the estimator.
func TestTargetOrEstimatedFeeRate(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	chain.feeRate = 5
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource:           chain,
		MaxFeeRateSatPerVByte: 50,
	})

	// A non-positive target defers to the estimator (5 sat/vB).
	rate, err := b.targetOrEstimatedFeeRate(t.Context(), 0)
	require.NoError(t, err)
	require.EqualValues(t, 5, rate)

	// A target within the cap is used verbatim.
	rate, err = b.targetOrEstimatedFeeRate(t.Context(), 25)
	require.NoError(t, err)
	require.EqualValues(t, 25, rate)

	// A target above the cap is clamped to the maximum.
	rate, err = b.targetOrEstimatedFeeRate(t.Context(), 999)
	require.NoError(t, err)
	require.EqualValues(t, 50, rate)
}

// TestBuildCPFPChildCreditsAnchorValue asserts the funded anchor's value is
// credited into the child's change output rather than silently burned as
// extra miner fee: the child's effective fee (inputs minus outputs) must
// equal exactly the childFee the caller asked for.
func TestBuildCPFPChildCreditsAnchorValue(t *testing.T) {
	t.Parallel()

	const (
		anchorValue = int64(330)
		feeInputVal = int64(50_000)
		childFee    = btcutil.Amount(1_000)
	)

	parent := makeFundedAnchorTx(20, anchorValue)
	anchorIdx := len(parent.TxOut) - 1
	anchorOutpoint := wire.OutPoint{
		Hash:  parent.TxHash(),
		Index: uint32(anchorIdx),
	}

	feeInput := &FeeInput{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0xfe,
			},
			Index: 0,
		},
		Output: &wire.TxOut{
			Value:    feeInputVal,
			PkScript: dummyChildP2WKHScript(),
		},
		Confirmed: true,
	}

	child, err := BuildCPFPChild(
		parent.Version, anchorOutpoint, parent.TxOut[anchorIdx],
		feeInput, dummyChildP2WKHScript(), childFee,
	)
	require.NoError(t, err)
	require.Len(t, child.TxOut, 1)

	// Effective fee = value in - value out. Both inputs carry value: the
	// funded anchor and the wallet fee input.
	valueIn := anchorValue + feeInputVal
	effectiveFee := valueIn - child.TxOut[0].Value
	require.EqualValues(
		t, childFee, effectiveFee,
		"anchor value must be credited to change, not burned as fee",
	)

	// The change explicitly includes the anchor's value.
	require.EqualValues(
		t, feeInputVal+anchorValue-int64(childFee),
		child.TxOut[0].Value,
	)
}

// dummyChildP2WKHScript returns a syntactically valid P2WKH pkScript for
// child-construction tests.
func dummyChildP2WKHScript() []byte {
	return append([]byte{txscript.OP_0, 0x14}, make([]byte, 20)...)
}

// TestChildFeeInputTarget asserts the wallet fee-input selection target is the
// child's own need: fee plus dust buffer, net of the anchor value it recovers,
// floored at one satoshi.
func TestChildFeeInputTarget(t *testing.T) {
	t.Parallel()

	// Ephemeral anchor (zero value): target is fee + dust, the historical
	// behaviour.
	require.EqualValues(
		t, 1_000+DustLimit, childFeeInputTarget(1_000, 0),
	)

	// Funded anchor: its value offsets the wallet's share.
	require.EqualValues(
		t, 1_000+DustLimit-330, childFeeInputTarget(1_000, 330),
	)

	// A large anchor covering the whole child fee still requires some
	// confirmed wallet input, so the target floors at one satoshi.
	require.EqualValues(
		t, 1, childFeeInputTarget(500, 10_000),
	)
}

// failingPkScriptWallet wraps fakeWallet so CPFP-child setup fails at the
// change-script derivation stage, the first fallback site in
// broadcastWithCPFP.
type failingPkScriptWallet struct {
	*fakeWallet
}

func (w *failingPkScriptWallet) NewWalletPkScript(_ context.Context) ([]byte,
	error) {

	return nil, fmt.Errorf("wallet says no")
}

// TestSubmitFundedBumpNoSilentFallback asserts that a fee bump of a funded
// parent whose CPFP setup fails returns the setup error instead of falling
// back to a direct re-broadcast of a parent that is already in the mempool
// (which would be swallowed as "already known" and reported as success).
func TestSubmitFundedBumpNoSilentFallback(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet: &failingPkScriptWallet{
			fakeWallet: &fakeWallet{
				utxos: []*walletcore.Utxo{makeWalletUTXO(t)},
			},
		},
	})

	_, err := b.Submit(t.Context(), 100, &BroadcastRequest{
		Tx:        makeFundedAnchorTx(10, 330),
		Label:     "funded-bump-nofallback",
		IsFeeBump: true,
		ParentFee: 500,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cpfp setup failed")

	// No hail-mary direct broadcast happened: the parent is already in a
	// mempool and re-broadcasting it would masquerade as success.
	require.Equal(t, 0, chain.broadcastCallCount())
	require.Equal(t, 0, chain.packageCallCount())
}

// TestSubmitEphemeralKeepsDirectFallback asserts the ephemeral (zero-fee TRUC)
// path retains its hail-mary direct fallback when CPFP setup fails: that
// parent's only route into a mempool is external help, so the fallback is
// still the right behaviour there.
func TestSubmitEphemeralKeepsDirectFallback(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet: &failingPkScriptWallet{
			fakeWallet: &fakeWallet{
				utxos: []*walletcore.Utxo{makeWalletUTXO(t)},
			},
		},
	})

	result, err := b.Submit(t.Context(), 100, &BroadcastRequest{
		Tx:        makeTestTx(true),
		Label:     "ephemeral-fallback",
		IsFeeBump: true,
	})
	require.NoError(t, err)
	require.Nil(t, result.ChildTxid)
	require.Equal(t, 1, chain.broadcastCallCount())
}

// TestSubmitFundedBumpParentFeeSufficient asserts the bump is refused up
// front, before any wallet round-trip, when the parent's own fee already
// covers the target package fee: the child's residual share would sit below
// min relay and be rejected by every mempool.
func TestSubmitFundedBumpParentFeeSufficient(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	wallet := &fakeWallet{utxos: []*walletcore.Utxo{makeWalletUTXO(t)}}
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet:      wallet,
	})

	_, err := b.Submit(t.Context(), 100, &BroadcastRequest{
		Tx:        makeFundedAnchorTx(11, 330),
		Label:     "funded-sufficient",
		IsFeeBump: true,
		ParentFee: 10_000_000,
	})
	require.ErrorIs(t, err, ErrParentFeeSufficient)

	// The short-circuit fires before fee-input selection: no lease was
	// taken and nothing hit the network.
	require.Empty(t, wallet.leaseCalls)
	require.Equal(t, 0, chain.broadcastCallCount())
	require.Equal(t, 0, chain.packageCallCount())
}

// newBumpTestBehavior constructs an unstarted txconfirm behavior driven
// synchronously via Receive, so tests can inspect internal tracked state
// without racing an actor goroutine.
func newBumpTestBehavior(t *testing.T, chain *fakeChainSourceRef,
	w Wallet) *TxBroadcasterActor {

	t.Helper()

	behavior := NewTxBroadcasterActor(Config{
		ChainSource: chain,
		Wallet:      w,
	})
	behavior.SetSelfRef(
		actor.NewChannelTellOnlyRef[Msg]("txconfirm-bump-test", 8),
	)

	return behavior
}

// mustReceiveEnsure drives one EnsureConfirmedReq through Receive.
func mustReceiveEnsure(t *testing.T, behavior *TxBroadcasterActor,
	req *EnsureConfirmedReq) *EnsureConfirmedResp {

	t.Helper()

	resp, err := behavior.Receive(t.Context(), req).Unpack()
	require.NoError(t, err)

	typed, ok := resp.(*EnsureConfirmedResp)
	require.True(t, ok)

	return typed
}

// mustReceiveBump drives one BumpNowReq through Receive.
func mustReceiveBump(t *testing.T, behavior *TxBroadcasterActor,
	req *BumpNowReq) *BumpNowResp {

	t.Helper()

	resp, err := behavior.Receive(t.Context(), req).Unpack()
	require.NoError(t, err)

	typed, ok := resp.(*BumpNowResp)
	require.True(t, ok)

	return typed
}

// TestBumpNowFundedParent covers the operator happy path: a funded parent in
// AwaitingConfirmation is force-bumped at a within-ceiling target, producing a
// CPFP child at exactly that rate, and the one-shot override is cleared.
func TestBumpNowFundedParent(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	wallet := &fakeWallet{utxos: []*walletcore.Utxo{makeWalletUTXO(t)}}
	behavior := newBumpTestBehavior(t, chain, wallet)

	sub := actor.NewChannelTellOnlyRef[Notification]("bump-sub", 8)
	tx := makeFundedAnchorTx(12, 330)
	txid := tx.TxHash()

	ensureResp := mustReceiveEnsure(t, behavior, &EnsureConfirmedReq{
		Tx:         tx,
		Label:      "bump-happy",
		ParentFee:  500,
		Subscriber: sub,
	})
	require.Equal(t, TxStateAwaitingConfirmation, ensureResp.State)
	require.Equal(t, 1, chain.broadcastCallCount())

	bumpResp := mustReceiveBump(t, behavior, &BumpNowReq{
		Txid:                     txid,
		TargetFeeRateSatPerVByte: 25,
	})
	require.True(t, bumpResp.Bumped)
	require.NotNil(t, bumpResp.ChildTxid)
	require.EqualValues(t, 25, bumpResp.EffectiveFeeRateSatPerVByte)
	require.False(t, bumpResp.Clamped)
	require.Equal(t, 1, chain.packageCallCount())

	// The one-shot operator override was consumed and cleared, so the next
	// interval-paced bump falls back to the estimator.
	require.Zero(t, behavior.tracked[txid].pendingTargetFeeRate)
}

// TestBumpNowClampedTarget asserts an over-ceiling operator target is clamped
// to the broadcaster maximum and the clamp is surfaced in the response rather
// than silently reported as a full success.
func TestBumpNowClampedTarget(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	wallet := &fakeWallet{utxos: []*walletcore.Utxo{makeWalletUTXO(t)}}
	behavior := newBumpTestBehavior(t, chain, wallet)

	sub := actor.NewChannelTellOnlyRef[Notification]("clamp-sub", 8)
	tx := makeFundedAnchorTx(13, 330)

	mustReceiveEnsure(t, behavior, &EnsureConfirmedReq{
		Tx:         tx,
		Label:      "bump-clamp",
		ParentFee:  500,
		Subscriber: sub,
	})

	bumpResp := mustReceiveBump(t, behavior, &BumpNowReq{
		Txid:                     tx.TxHash(),
		TargetFeeRateSatPerVByte: 999,
	})
	require.True(t, bumpResp.Bumped)
	require.EqualValues(
		t, DefaultMaxFeeRateSatPerVByte,
		bumpResp.EffectiveFeeRateSatPerVByte,
	)
	require.True(t, bumpResp.Clamped)
}

// TestBumpNowFundedBroadcastingSubmitsPackage covers the stuck-at-relay case:
// a funded parent whose direct broadcast keeps failing (mempool min fee above
// its fixed rate) sits in Broadcasting, and a forced bump submits parent+child
// as a package, which is exactly what carries a never-in-mempool parent in.
func TestBumpNowFundedBroadcastingSubmitsPackage(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	chain.broadcastErr = fmt.Errorf("min relay fee not met")
	wallet := &fakeWallet{utxos: []*walletcore.Utxo{makeWalletUTXO(t)}}
	behavior := newBumpTestBehavior(t, chain, wallet)

	sub := actor.NewChannelTellOnlyRef[Notification]("stuck-sub", 8)
	tx := makeFundedAnchorTx(14, 330)

	ensureResp := mustReceiveEnsure(t, behavior, &EnsureConfirmedReq{
		Tx:         tx,
		Label:      "bump-stuck",
		ParentFee:  200,
		Subscriber: sub,
	})
	require.Equal(t, TxStateBroadcasting, ensureResp.State)

	bumpResp := mustReceiveBump(t, behavior, &BumpNowReq{
		Txid:                     tx.TxHash(),
		TargetFeeRateSatPerVByte: 30,
	})
	require.True(t, bumpResp.Bumped)
	require.NotNil(t, bumpResp.ChildTxid)
	require.Equal(t, TxStateAwaitingConfirmation, bumpResp.State)
	require.Equal(t, 1, chain.packageCallCount())
}

// TestBumpNowEphemeralBroadcastingRefused asserts a zero-fee ephemeral parent
// that never reached a mempool still refuses a forced bump: its retry loop is
// already package-submitting on every interval, so the forced pass adds
// nothing.
func TestBumpNowEphemeralBroadcastingRefused(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	chain.packageErr = fmt.Errorf("package rejected")
	chain.broadcastErr = fmt.Errorf("min relay fee not met")
	wallet := &fakeWallet{utxos: []*walletcore.Utxo{makeWalletUTXO(t)}}
	behavior := newBumpTestBehavior(t, chain, wallet)

	sub := actor.NewChannelTellOnlyRef[Notification]("eph-sub", 8)
	tx := makeTestTx(true)

	ensureResp := mustReceiveEnsure(t, behavior, &EnsureConfirmedReq{
		Tx:         tx,
		Label:      "eph-stuck",
		Subscriber: sub,
	})
	require.Equal(t, TxStateBroadcasting, ensureResp.State)

	bumpResp := mustReceiveBump(t, behavior, &BumpNowReq{
		Txid:                     tx.TxHash(),
		TargetFeeRateSatPerVByte: 30,
	})
	require.False(t, bumpResp.Bumped)
	require.Contains(t, bumpResp.Reason, "not yet in mempool")
}

// TestBumpNowParentFeeSufficient asserts a forced bump whose target the parent
// already meets reports an honest no-op (with the sentinel reason) and leaves
// the tracked tx awaiting confirmation.
func TestBumpNowParentFeeSufficient(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	wallet := &fakeWallet{utxos: []*walletcore.Utxo{makeWalletUTXO(t)}}
	behavior := newBumpTestBehavior(t, chain, wallet)

	sub := actor.NewChannelTellOnlyRef[Notification]("suff-sub", 8)
	tx := makeFundedAnchorTx(15, 330)

	mustReceiveEnsure(t, behavior, &EnsureConfirmedReq{
		Tx:         tx,
		Label:      "bump-sufficient",
		ParentFee:  10_000_000,
		Subscriber: sub,
	})

	bumpResp := mustReceiveBump(t, behavior, &BumpNowReq{
		Txid:                     tx.TxHash(),
		TargetFeeRateSatPerVByte: 5,
	})
	require.False(t, bumpResp.Bumped)
	require.Contains(t, bumpResp.Reason, "parent fee already meets")
	require.Equal(t, TxStateAwaitingConfirmation, bumpResp.State)

	// The refused bump consumed its one-shot override too.
	require.Zero(t, behavior.tracked[tx.TxHash()].pendingTargetFeeRate)
}

// TestBumpNowUntrackedAndTerminal covers the remaining no-op guards: an
// untracked txid, and a transaction that confirmed and was evicted from
// tracking (a confirmed entry whose terminal notification is delivered is
// dropped from the map, so a late bump lands in the untracked branch).
func TestBumpNowUntrackedAndTerminal(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	wallet := &fakeWallet{utxos: []*walletcore.Utxo{makeWalletUTXO(t)}}
	behavior := newBumpTestBehavior(t, chain, wallet)

	// Untracked txid.
	bumpResp := mustReceiveBump(t, behavior, &BumpNowReq{
		Txid: chainhash.Hash{0xaa},
	})
	require.False(t, bumpResp.Bumped)
	require.Contains(t, bumpResp.Reason, "not tracked")

	// Already-confirmed transaction: the fake chain reports an immediate
	// confirmation on watch registration.
	tx := makeFundedAnchorTx(16, 330)
	chain.alreadyConfirmed[tx.TxHash()] = chainsource.ConfirmationEvent{
		Txid:        tx.TxHash(),
		BlockHeight: 99,
		NumConfs:    1,
	}

	sub := actor.NewChannelTellOnlyRef[Notification]("term-sub", 8)
	mustReceiveEnsure(t, behavior, &EnsureConfirmedReq{
		Tx:         tx,
		Label:      "confirmed",
		Subscriber: sub,
	})

	// Drain the confirmation callback the fake chain delivered to the
	// self ref so the entry reaches its terminal state and, with its
	// notification delivered, is evicted from tracking.
	selfRef, ok := behavior.selfRef.(*actor.ChannelTellOnlyRef[Msg])
	require.True(t, ok)
	for {
		msg, ok := selfRef.AwaitMessage(100 * time.Millisecond)
		if !ok {
			break
		}
		_, _ = behavior.Receive(t.Context(), msg).Unpack()
	}
	require.NotNil(t, mustAwaitNotification(t, sub))

	// A bump after eviction reports the untracked no-op: there is no
	// entry left to bump, which is the correct answer for a confirmed tx.
	bumpResp = mustReceiveBump(t, behavior, &BumpNowReq{
		Txid: tx.TxHash(),
	})
	require.False(t, bumpResp.Bumped)
	require.Contains(t, bumpResp.Reason, "not tracked")
}
