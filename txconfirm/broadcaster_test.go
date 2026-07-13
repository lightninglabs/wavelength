package txconfirm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/chainbackends"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/walletcore"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// staticChainSourceRef is a small programmable chainsource actor ref for unit
// tests that need precise responses.
type staticChainSourceRef struct {
	handler func(context.Context,
		chainsource.ChainSourceMsg) (chainsource.ChainSourceResp, error)
}

// ID returns the fake actor ID.
func (s *staticChainSourceRef) ID() string {
	return "static-chainsource"
}

// Tell satisfies the actor.ActorRef interface.
func (s *staticChainSourceRef) Tell(_ context.Context,
	_ chainsource.ChainSourceMsg) error {

	return nil
}

// Ask handles the chainsource request synchronously and returns an already
// completed future.
func (s *staticChainSourceRef) Ask(ctx context.Context,
	msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	promise := actor.NewPromise[chainsource.ChainSourceResp]()
	resp, err := s.handler(ctx, msg)
	if err != nil {
		promise.Complete(fn.Err[chainsource.ChainSourceResp](err))
	} else {
		promise.Complete(fn.Ok(resp))
	}

	return promise.Future()
}

// failingWallet is a programmable wallet test double for broadcaster tests.
type failingWallet struct {
	listErr      error
	changeErr    error
	finalizeErr  error
	changeScript []byte
	utxos        []*walletcore.Utxo
}

// ListUnspent returns the configured result.
func (w *failingWallet) ListUnspent(_ context.Context, _, _ int32) (
	[]*walletcore.Utxo, error) {

	return w.utxos, w.listErr
}

// NewWalletPkScript returns the configured result.
func (w *failingWallet) NewWalletPkScript(_ context.Context) ([]byte, error) {
	return w.changeScript, w.changeErr
}

// FinalizePsbt returns the configured result.
func (w *failingWallet) FinalizePsbt(_ context.Context, _ []byte) (*wire.MsgTx,
	error) {

	if w.finalizeErr != nil {
		return nil, w.finalizeErr
	}

	return wire.NewMsgTx(3), nil
}

func (w *failingWallet) FundPsbt(ctx context.Context, packetBytes []byte,
	_ int64, _ walletcore.LockID, _ time.Duration) (*wire.MsgTx, error) {

	return fundTestPsbt(ctx, packetBytes, w.utxos, w.FinalizePsbt)
}

// LeaseOutput is a noop for the failing wallet test double.
func (w *failingWallet) LeaseOutput(_ context.Context, _ walletcore.LockID,
	_ wire.OutPoint, expiry time.Duration) (time.Time, error) {

	return time.Now().Add(expiry), nil
}

// ReleaseOutput is a noop for the failing wallet test double.
func (w *failingWallet) ReleaseOutput(_ context.Context, _ walletcore.LockID,
	_ wire.OutPoint) error {

	return nil
}

// rewritingWallet is a wallet test double that parses the PSBT it is
// given, attaches dummy finalized witnesses, and optionally hands the
// resulting transaction through a caller-supplied rewrite hook. It is
// used to exercise signCPFPChild's robustness to wallets that return
// finalized transactions whose input composition does not round-trip the
// requested PSBT (reordered inputs, added inputs, substituted outpoints).
type rewritingWallet struct {
	utxos        []*walletcore.Utxo
	changeScript []byte

	// rewrite, when non-nil, receives the default finalized tx and
	// returns the tx that the wallet will hand back to the caller. The
	// default finalized tx has every input's witness set to a dummy
	// 64-byte value except for inputs whose PSBT FinalScriptWitness
	// was pre-set (the anchor) which receive an empty witness.
	rewrite func(*wire.MsgTx) *wire.MsgTx

	// inspect, when non-nil, receives the parsed PSBT before dummy
	// witnesses are attached.
	inspect func(*psbt.Packet)
}

// ListUnspent returns the configured UTXOs.
func (w *rewritingWallet) ListUnspent(_ context.Context, _, _ int32) (
	[]*walletcore.Utxo, error) {

	return w.utxos, nil
}

// NewWalletPkScript returns the configured change script. Callers MUST
// set changeScript at construction time; this test double does not
// fall back to a default because returning a script from a closure that
// has no access to *testing.T would force script-builder errors to
// surface as panics rather than test failures.
func (w *rewritingWallet) NewWalletPkScript(_ context.Context) ([]byte, error) {
	if len(w.changeScript) == 0 {
		return nil, fmt.Errorf("rewritingWallet: changeScript must " +
			"be set at construction time")
	}

	return w.changeScript, nil
}

// FinalizePsbt parses the supplied PSBT, applies dummy witnesses, and
// then runs the configured rewrite hook (if any) before returning.
func (w *rewritingWallet) FinalizePsbt(_ context.Context, packetBytes []byte) (
	*wire.MsgTx, error) {

	packet, err := psbt.NewFromRawBytes(bytes.NewReader(packetBytes), false)
	if err != nil {
		return nil, err
	}

	if w.inspect != nil {
		w.inspect(packet)
	}

	tx := packet.UnsignedTx.Copy()
	for i := range tx.TxIn {
		if len(packet.Inputs[i].FinalScriptWitness) > 0 {
			tx.TxIn[i].Witness = wire.TxWitness{}
			continue
		}

		tx.TxIn[i].Witness = wire.TxWitness{make([]byte, 64)}
	}

	if w.rewrite != nil {
		tx = w.rewrite(tx)
	}

	return tx, nil
}

func (w *rewritingWallet) FundPsbt(ctx context.Context, packetBytes []byte,
	_ int64, _ walletcore.LockID, _ time.Duration) (*wire.MsgTx, error) {

	return fundTestPsbt(ctx, packetBytes, w.utxos, w.FinalizePsbt)
}

// LeaseOutput is a noop for the rewriting wallet test double.
func (w *rewritingWallet) LeaseOutput(_ context.Context, _ walletcore.LockID,
	_ wire.OutPoint, expiry time.Duration) (time.Time, error) {

	return time.Now().Add(expiry), nil
}

// ReleaseOutput is a noop for the rewriting wallet test double.
func (w *rewritingWallet) ReleaseOutput(_ context.Context, _ walletcore.LockID,
	_ wire.OutPoint) error {

	return nil
}

func fundTestPsbt(ctx context.Context, packetBytes []byte,
	utxos []*walletcore.Utxo,
	finalize func(context.Context, []byte) (*wire.MsgTx, error)) (
	*wire.MsgTx, error) {

	packet, err := psbt.NewFromRawBytes(bytes.NewReader(packetBytes), false)
	if err != nil {
		return nil, err
	}

	if len(utxos) == 0 {
		return nil, fmt.Errorf("insufficient funds")
	}

	utxo := utxos[0]
	packet.UnsignedTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: utxo.Outpoint,
	})
	packet.Inputs = append(packet.Inputs, psbt.PInput{
		WitnessUtxo: &wire.TxOut{
			Value:    int64(utxo.Amount),
			PkScript: append([]byte(nil), utxo.PkScript...),
		},
	})

	var outputTotal btcutil.Amount
	for _, txOut := range packet.UnsignedTx.TxOut {
		outputTotal += btcutil.Amount(txOut.Value)
	}
	change := utxo.Amount - outputTotal - 1_000
	if change > DustLimit {
		packet.UnsignedTx.AddTxOut(&wire.TxOut{
			Value:    int64(change),
			PkScript: []byte{txscript.OP_TRUE},
		})
		packet.Outputs = append(packet.Outputs, psbt.POutput{})
	}

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return nil, err
	}

	return finalize(ctx, buf.Bytes())
}

// blockingReleaseWallet records that ReleaseOutput started, then blocks until
// the test allows it to finish.
type blockingReleaseWallet struct {
	failingWallet

	startOnce sync.Once
	started   chan struct{}
	unblock   chan struct{}
}

// ReleaseOutput blocks until the test closes unblock.
func (w *blockingReleaseWallet) ReleaseOutput(context.Context,
	walletcore.LockID, wire.OutPoint) error {

	w.startOnce.Do(func() {
		close(w.started)
	})

	<-w.unblock

	return nil
}

// failingNotifyRef is a TellOnlyRef that always returns an error.
type failingNotifyRef struct{}

// ID returns the fake subscriber ID.
func (f *failingNotifyRef) ID() string {
	return "failing-notify"
}

// Tell always returns an error.
func (f *failingNotifyRef) Tell(_ context.Context, _ Notification) error {
	return fmt.Errorf("notify failed")
}

// testMappedMsg is a small actor message used to cover MapNotification.
type testMappedMsg struct {
	actor.BaseMessage
	payload string
}

// MessageType returns the stable message type identifier.
func (m testMappedMsg) MessageType() string {
	return "testMappedMsg"
}

// testUnknownMsg is a local message used to cover the actor's default Receive
// branch.
type testUnknownMsg struct {
	actor.BaseMessage
}

// MessageType returns the stable message type identifier.
func (m *testUnknownMsg) MessageType() string {
	return "testUnknownMsg"
}

// txConfirmMsgSealed seals testUnknownMsg into the package message surface for
// testing.
func (m *testUnknownMsg) txConfirmMsgSealed() {}

// newTrackedTxForState creates a tracked tx handle backed by the supplied FSM
// state for white-box helper tests.
func newTrackedTxForState(t *testing.T, state trackedTxState) *trackedTx {
	t.Helper()

	var data trackedTxData
	switch s := state.(type) {
	case *trackedTxStateNew:
		data = s.trackedTxData

	case *trackedTxStateBroadcasting:
		data = s.trackedTxData

	case *trackedTxStateAwaitingConfirmation:
		data = s.trackedTxData

	case *trackedTxStateFeeBumping:
		data = s.trackedTxData

	case *trackedTxStateConfirmed:
		data = s.trackedTxData

	case *trackedTxStateFailed:
		data = s.trackedTxData

	default:
		t.Fatalf("unexpected tracked tx state %T", state)
	}

	fsm := protofsm.NewStateMachine(protofsm.StateMachineCfg[
		trackedTxEvent, trackedTxOutboxEvent, *trackedTxEnvironment,
	]{
		InitialState: state,
		Logger:       btclog.Disabled,
		ErrorReporter: &trackedTxErrorReporter{
			log:  btclog.Disabled,
			txid: data.Txid,
		},
		Env: &trackedTxEnvironment{Txid: data.Txid},
	})
	fsm.Start(t.Context())
	t.Cleanup(fsm.Stop)

	return &trackedTx{
		data:        data,
		fsm:         &fsm,
		subscribers: make(map[string]trackedSubscriber),
	}
}

// TestMessageHelpers covers message helper methods and the notification mapper.
func TestMessageHelpers(t *testing.T) {
	t.Run("state strings", func(t *testing.T) {
		require.Equal(t, "new", TxStateNew.String())
		require.Equal(t, "broadcasting", TxStateBroadcasting.String())
		require.Equal(
			t, "awaiting_confirmation",
			TxStateAwaitingConfirmation.String(),
		)
		require.Equal(t, "fee_bumping", TxStateFeeBumping.String())
		require.Equal(t, "confirmed", TxStateConfirmed.String())
		require.Equal(t, "failed", TxStateFailed.String())
		require.Contains(t, TxState(99).String(), "unknown")
	})

	t.Run("message types and sealed methods", func(t *testing.T) {
		ensureReq := &EnsureConfirmedReq{}
		ensureReq.txConfirmMsgSealed()
		require.Equal(t, "EnsureConfirmedReq", ensureReq.MessageType())

		ensureResp := &EnsureConfirmedResp{}
		ensureResp.txConfirmRespSealed()
		require.Equal(
			t, "EnsureConfirmedResp", ensureResp.MessageType(),
		)

		cancelReq := &CancelInterestReq{}
		cancelReq.txConfirmMsgSealed()
		require.Equal(t, "CancelInterestReq", cancelReq.MessageType())

		cancelResp := &CancelInterestResp{}
		cancelResp.txConfirmRespSealed()
		require.Equal(t, "CancelInterestResp", cancelResp.MessageType())

		confirmed := &TxConfirmed{}
		confirmed.txConfirmNotificationSealed()
		require.Equal(t, "TxConfirmed", confirmed.MessageType())

		failed := &TxFailed{}
		failed.txConfirmNotificationSealed()
		require.Equal(t, "TxFailed", failed.MessageType())

		confMsg := &confirmationObservedMsg{}
		confMsg.txConfirmMsgSealed()
		require.Equal(
			t, "confirmationObservedMsg", confMsg.MessageType(),
		)

		blockMsg := &blockEpochObservedMsg{}
		blockMsg.txConfirmMsgSealed()
		require.Equal(
			t, "blockEpochObservedMsg", blockMsg.MessageType(),
		)
	})

	t.Run("notification mapping", func(t *testing.T) {
		target := actor.NewChannelTellOnlyRef[testMappedMsg](
			"mapped", 1,
		)
		mapped := MapNotification(
			target,
			func(msg Notification) testMappedMsg {
				return testMappedMsg{payload: msg.MessageType()}
			},
		)

		err := mapped.Tell(t.Context(), &TxConfirmed{})
		require.NoError(t, err)

		received, ok := target.AwaitMessage(testTimeout)
		require.True(t, ok)
		require.Equal(t, "TxConfirmed", received.payload)
	})
}

// TestBroadcasterHelperFunctions covers the pure helper functions used by the
// generic broadcaster.
func TestBroadcasterHelperFunctions(t *testing.T) {
	tx := makeTestTx(true)

	t.Run("estimate package fee", func(t *testing.T) {
		fee, err := EstimatePackageFee(tx, 5)
		require.NoError(t, err)
		require.Positive(t, fee)

		_, err = EstimatePackageFee(nil, 5)
		require.Error(t, err)

		_, err = EstimatePackageFee(tx, 0)
		require.Error(t, err)
	})

	t.Run("build cpfp child", func(t *testing.T) {
		feeInput := &FeeInput{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{
					3,
				},
				Index: 2,
			},
			Output: &wire.TxOut{
				Value: 10_000,
				PkScript: []byte{
					txscript.OP_TRUE,
				},
			},
			Confirmed: true,
		}

		child, err := BuildCPFPChild(
			tx.Version, wire.OutPoint{
				Hash:  tx.TxHash(),
				Index: 1,
			},
			tx.TxOut[1],
			feeInput,
			[]byte{txscript.OP_TRUE},
			500,
		)
		require.NoError(t, err)
		require.Len(t, child.TxIn, 2)
		require.Len(t, child.TxOut, 1)

		// The anchor input is anyone-can-spend with no timelock
		// semantics, so its sequence keeps the sentinel value.
		// The fee input signals BIP-125 RBF (MaxTxInSequenceNum - 2)
		// as a belt-and-suspenders for any non-TRUC caller that
		// ever slips past the Submit-time version gate.
		require.Equal(
			t, wire.MaxTxInSequenceNum, child.TxIn[0].Sequence,
		)
		require.Equal(
			t, wire.MaxTxInSequenceNum-2, child.TxIn[1].Sequence,
		)

		// A fee input whose entire value is consumed by the fee
		// leaves no room for a change output. Since the child's only
		// non-anchor output is that change, BuildCPFPChild must refuse
		// rather than emit a zero-output transaction that FinalizePsbt
		// would reject.
		_, err = BuildCPFPChild(
			tx.Version, wire.OutPoint{
				Hash:  tx.TxHash(),
				Index: 1,
			},
			tx.TxOut[1],
			feeInput,
			[]byte{txscript.OP_TRUE},
			btcutil.Amount(feeInput.Output.Value),
		)
		require.ErrorIs(t, err, ErrFeeInputProducesDust)

		// A fee input leaving change one sat below the dust limit must
		// also be refused: a sub-dust change output is not relayable.
		_, err = BuildCPFPChild(
			tx.Version, wire.OutPoint{
				Hash:  tx.TxHash(),
				Index: 1,
			},
			tx.TxOut[1],
			feeInput,
			[]byte{txscript.OP_TRUE},
			btcutil.Amount(feeInput.Output.Value)-DustLimit+1,
		)
		require.ErrorIs(t, err, ErrFeeInputProducesDust)

		// Change of exactly the dust limit is the boundary that is
		// still acceptable: the child gets its single change output.
		boundaryChild, err := BuildCPFPChild(
			tx.Version, wire.OutPoint{
				Hash:  tx.TxHash(),
				Index: 1,
			},
			tx.TxOut[1],
			feeInput,
			[]byte{txscript.OP_TRUE},
			btcutil.Amount(feeInput.Output.Value)-DustLimit,
		)
		require.NoError(t, err)
		require.Len(t, boundaryChild.TxOut, 1)
		require.Equal(
			t, int64(DustLimit), boundaryChild.TxOut[0].Value,
		)

		_, err = BuildCPFPChild(
			tx.Version, wire.OutPoint{}, tx.TxOut[1], &FeeInput{
				Confirmed: false,
			},
			nil,
			1,
		)
		require.Error(t, err)
	})

	t.Run("select fee input", func(t *testing.T) {
		feeInputs := []FeeInput{
			{
				Outpoint: wire.OutPoint{
					Hash: chainhash.Hash{
						1,
					},
					Index: 1,
				},
				Output: &wire.TxOut{
					Value: 1000,
				},
				Confirmed: true,
			},
			{
				Outpoint: wire.OutPoint{
					Hash: chainhash.Hash{
						2,
					},
					Index: 2,
				},
				Output: &wire.TxOut{
					Value: 5000,
				},
				Confirmed: true,
			},
		}

		selected, err := SelectFeeInput(feeInputs, 2000, nil)
		require.NoError(t, err)
		require.Equal(t, int64(5000), selected.Output.Value)

		excluded := map[wire.OutPoint]bool{
			feeInputs[0].Outpoint: true,
		}
		selected, err = SelectFeeInput(
			feeInputs, 500, excluded,
		)
		require.NoError(t, err)
		require.Equal(t, feeInputs[1].Outpoint, selected.Outpoint)

		_, err = SelectFeeInput(feeInputs, 10_000, nil)
		require.Error(t, err)
	})

	t.Run("ignorable errors", func(t *testing.T) {
		require.True(
			t,
			IsIgnorableBroadcastError(
				fmt.Errorf("already known"),
			),
		)
		require.False(
			t,
			IsIgnorableBroadcastError(
				fmt.Errorf("fatal"),
			),
		)
		require.True(
			t,
			isPackageSubmissionUnsupported(
				fmt.Errorf("package relay not supported"),
			),
		)
		require.False(
			t,
			isPackageSubmissionUnsupported(
				fmt.Errorf("fatal"),
			),
		)
	})

	t.Run("cpfp fee input sighash", func(t *testing.T) {
		require.Equal(
			t, txscript.SigHashDefault,
			cpfpFeeInputSighash(
				p2trTestPkScript(t),
			),
		)
		require.Equal(
			t, txscript.SigHashAll,
			cpfpFeeInputSighash(
				p2wkhTestPkScript(t),
			),
		)
	})

	t.Run("parent known child failed", func(t *testing.T) {
		var parent chainhash.Hash
		copy(parent[:], bytes.Repeat([]byte{0xab}, 32))

		var child chainhash.Hash
		copy(child[:], bytes.Repeat([]byte{0xcd}, 32))

		// Build realistic-shape per-tx package errors the same way
		// chainbackends.handlePackageResult does: one
		// *PackageTxError per per-tx result, joined via
		// errors.Join. Each entry's mapped sentinel comes from
		// rpcclient.MapRPCErr applied to the raw reason string.
		rbf := fmt.Errorf("submit package: package not accepted: "+
			"transaction failed: %w", errors.Join(
			chainbackends.NewPackageTxError(
				"W1", parent, "txn-already-known",
			),
			chainbackends.NewPackageTxError(
				"W2", child, "insufficient fee, "+
					"rejecting replacement; "+
					"new feerate 0.00004 "+
					"BTC/kvB <= old feerate "+
					"0.00207 BTC/kvB",
			),
		))
		require.True(t, isParentKnownChildFailed(parent, rbf))

		missing := fmt.Errorf("submit package: package not "+
			"accepted: %w", errors.Join(
			chainbackends.NewPackageTxError(
				"W1", parent, "txn-already-known",
			),
			chainbackends.NewPackageTxError(
				"W2", child,
				"bad-txns-inputs-missingorspent",
			),
		))
		require.True(t, isParentKnownChildFailed(parent, missing))

		// Parent failed for a non-known reason: we are NOT in the
		// "parent broadcast by someone else" situation, so the
		// helper must not steal this case.
		fatal := fmt.Errorf("submit package: package not accepted: %w",
			chainbackends.NewPackageTxError(
				"W1", parent, "bad-witness",
			))
		require.False(t, isParentKnownChildFailed(parent, fatal))

		// Deeply-confirmed parent: bitcoind has dropped the parent's
		// txid from its recent-rejects / mempool cache, so the
		// re-broadcast validates fresh and hits "inputs already spent".
		// The confirmation watch handles the existing confirmation.
		deeplyConfirmed := fmt.Errorf("submit package: package not "+
			"accepted: %w", errors.Join(
			chainbackends.NewPackageTxError(
				"W1", parent,
				"bad-txns-inputs-missingorspent",
			),
			chainbackends.NewPackageTxError(
				"W2", child,
				"bad-txns-inputs-missingorspent",
			),
		))
		require.True(
			t, isParentKnownChildFailed(parent, deeplyConfirmed),
		)

		// Parent's txid does not appear as a parent-known entry
		// in the error, but RBF rejection still implies a
		// competing tx is already in mempool — defer regardless.
		// (Some bitcoind versions only echo per-tx results for
		// failures, so an accepted parent silently disappears
		// from the error.)
		var other chainhash.Hash
		copy(other[:], bytes.Repeat([]byte{0xee}, 32))
		require.True(t, isParentKnownChildFailed(other, rbf))

		// Parent known but no child failure marker (whole
		// package accepted as no-op): not our case, the
		// higher-level switch already routes through
		// IsIgnorableBroadcastError first.
		known := chainbackends.NewPackageTxError(
			"W1", parent, "txn-already-known",
		)
		require.False(t, isParentKnownChildFailed(parent, known))

		// Parent IS echoed but with a genuinely fatal reason
		// (e.g. bad-witness), and the joined error happens to
		// also contain a "rejecting replacement" marker on some
		// other entry. The RBF fallback must NOT fire — the
		// parent is broken, not "already broadcast by someone
		// else". Without the parentSeen gate this would
		// erroneously return true and silently swallow the
		// fatal parent.
		fatalWithRBF := fmt.Errorf("submit package: package not "+
			"accepted: %w", errors.Join(
			chainbackends.NewPackageTxError(
				"W1", parent, "bad-witness",
			),
			chainbackends.NewPackageTxError(
				"W2", child, "insufficient fee, "+
					"rejecting replacement",
			),
		))
		require.False(
			t, isParentKnownChildFailed(parent, fatalWithRBF),
		)
	})
}

// p2wkhTestPkScript returns a fixed P2WKH pkScript for tests that need a
// legacy segwit wallet fee input.
func p2wkhTestPkScript(t *testing.T) []byte {
	t.Helper()

	hash := make([]byte, 20)
	for i := range hash {
		hash[i] = byte(i + 1)
	}

	script, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_0).
		AddData(hash).
		Script()
	require.NoError(t, err)

	return script
}

// TestCPFPChildSelectionAvoidsDustOutput is a regression test for the
// zero-output CPFP child bug. A wallet littered with near-dust UTXOs caused
// SelectFeeInput (which picks the smallest UTXO covering the fee) to choose an
// input whose post-fee remainder fell below the dust limit, so BuildCPFPChild
// dropped its only output and produced a transaction with no outputs that the
// wallet's FinalizePsbt rejected. The fix selects for totalFee + DustLimit so a
// spendable change output is always emitted, and BuildCPFPChild now refuses a
// sub-dust remainder rather than silently emitting a zero-output child.
func TestCPFPChildSelectionAvoidsDustOutput(t *testing.T) {
	t.Parallel()

	const totalFee = btcutil.Amount(500)

	// A wallet mirroring the production litter: a near-dust "poison" UTXO
	// that covers the fee but leaves one sat below dust as change, next to
	// a large UTXO that yields ample change.
	poison := FeeInput{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0xaa,
			}, Index: 0,
		},
		Output: &wire.TxOut{
			Value: int64(totalFee + DustLimit - 1),
		},
		Confirmed: true,
	}
	large := FeeInput{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0xbb,
			}, Index: 0,
		},
		Output: &wire.TxOut{
			Value: 5_000_000,
		},
		Confirmed: true,
	}
	inputs := []FeeInput{poison, large}

	parent := wire.NewMsgTx(3)
	parent.AddTxOut(&wire.TxOut{
		Value: 0, PkScript: []byte{txscript.OP_TRUE},
	})
	anchorOutpoint := wire.OutPoint{Hash: parent.TxHash(), Index: 0}
	changePkScript := []byte{txscript.OP_TRUE}

	// Selecting for the bare fee reproduces the original selection: the
	// smallest covering UTXO is the poison input, and building a child with
	// it now yields a sub-dust change that is refused (previously this
	// silently produced a zero-output transaction).
	buggy, err := SelectFeeInput(inputs, totalFee, nil)
	require.NoError(t, err)
	require.Equal(t, poison.Outpoint, buggy.Outpoint)

	_, err = BuildCPFPChild(
		parent.Version, anchorOutpoint, parent.TxOut[0], buggy,
		changePkScript, totalFee,
	)
	require.ErrorIs(t, err, ErrFeeInputProducesDust)

	// Selecting with the dust buffer skips the poison UTXO and lands on the
	// large input, so the child always carries a valid change output.
	fixed, err := SelectFeeInput(inputs, totalFee+DustLimit, nil)
	require.NoError(t, err)
	require.Equal(t, large.Outpoint, fixed.Outpoint)

	child, err := BuildCPFPChild(
		parent.Version, anchorOutpoint, parent.TxOut[0], fixed,
		changePkScript, totalFee,
	)
	require.NoError(t, err)
	require.Len(t, child.TxOut, 1)
	require.Equal(
		t, int64(btcutil.Amount(large.Output.Value)-totalFee),
		child.TxOut[0].Value,
	)
}

// TestCPFPReselectsAfterPreciseFeeGrowth verifies that when the precise
// per-input vsize recalculation grows the package fee enough to push the
// originally selected fee input's change below the dust limit, the
// broadcaster reselects a larger fee input instead of tripping
// ErrFeeInputProducesDust and re-picking the same too-small input on every
// retry.
func TestCPFPReselectsAfterPreciseFeeGrowth(t *testing.T) {
	t.Parallel()

	const feeRate = 5

	chain := newFakeChainSourceRef(100)
	chain.feeRate = feeRate

	parent := makeTestTx(true)

	// The change script the wallet hands out is OP_TRUE, which fee
	// estimation treats as the P2WKH fallback class. A P2PKH fee input is
	// heavier than that proxy, so selecting it forces the precise vsize
	// recalculation to grow the package fee.
	opTrue := []byte{txscript.OP_TRUE}
	changeVSize := estimateChildVSize(opTrue, opTrue)
	p2pkhScript := p2pkhTestPkScript(t)
	preciseVSize := estimateChildVSize(p2pkhScript, opTrue)
	require.Greater(
		t, preciseVSize, changeVSize,
		"P2PKH fee input must be heavier than the change proxy",
	)

	baseFee, err := computePackageFee(parent, feeRate, changeVSize)
	require.NoError(t, err)
	preciseFee, err := computePackageFee(parent, feeRate, preciseVSize)
	require.NoError(t, err)
	require.Greater(t, preciseFee, baseFee)

	// The small P2PKH UTXO clears the initial totalFee + DustLimit
	// threshold but not the grown preciseFee + DustLimit one, so it is
	// selected first and then must be abandoned.
	small := &walletcore.Utxo{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0xaa,
			},
			Index: 0,
		},
		Amount:   baseFee + DustLimit,
		PkScript: p2pkhScript,
	}
	require.Less(
		t, small.Amount, preciseFee+DustLimit,
		"small UTXO must fail the precise threshold",
	)

	// A larger UTXO that comfortably covers the precise threshold.
	large := &walletcore.Utxo{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0xbb,
			},
			Index: 0,
		},
		Amount:   preciseFee + DustLimit + 1000,
		PkScript: p2pkhScript,
	}

	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet: &fakeWallet{
			utxos: []*walletcore.Utxo{small, large},
		},
	})

	_, err = b.Submit(t.Context(), 100, &BroadcastRequest{
		Tx: parent, Label: "reselect",
	})
	require.NoError(t, err)

	// A CPFP package must have been submitted (not a direct-broadcast
	// fallback), and its child must spend the large UTXO, not the
	// abandoned small one.
	require.Equal(t, 1, chain.packageCallCount())
	require.Zero(t, chain.broadcastCallCount())

	child := chain.packageCalls[0].Child
	require.NotNil(t, child)

	var spendsLarge, spendsSmall bool
	for _, in := range child.TxIn {
		switch in.PreviousOutPoint {
		case large.Outpoint:
			spendsLarge = true

		case small.Outpoint:
			spendsSmall = true
		}
	}
	require.True(t, spendsLarge, "child must spend the reselected UTXO")
	require.False(
		t, spendsSmall, "child must not spend the abandoned UTXO",
	)
}

// makeCheckpointShapedParent builds a v3/TRUC parent that mirrors a finalized
// OOR checkpoint / fraud-response tx: its sole non-anchor input already carries
// a real (pre-signed) taproot key-spend witness, it has a taproot checkpoint
// output, and the canonical P2A anchor is the last output. This differs from a
// round commitment tx only in the parent's own input witness — the trait
// issue #509 hypothesised would break the CPFP child finalize path.
func makeCheckpointShapedParent() (*wire.MsgTx, wire.TxWitness) {
	tx := wire.NewMsgTx(3)

	// A pre-signed taproot key-spend witness (single 64-byte schnorr sig),
	// as a finalized OOR checkpoint input would carry.
	signedWitness := wire.TxWitness{bytes.Repeat([]byte{0x07}, 64)}
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0xcc},
			Index: 1,
		},
		Witness: signedWitness,
	})

	// A P2TR checkpoint output (OP_1 <32-byte key>).
	checkpointPkScript := append(
		[]byte{txscript.OP_1, txscript.OP_DATA_32},
		bytes.Repeat([]byte{0x02}, 32)...,
	)
	tx.AddTxOut(&wire.TxOut{
		Value:    50_000,
		PkScript: checkpointPkScript,
	})

	// The canonical P2A anchor, always last.
	tx.AddTxOut(arkscript.AnchorOutput())

	return tx, signedWitness
}

// TestCPFPChildFinalizesCheckpointShapedParent is a regression test for
// issue #509. It proves the CPFP child finalize path is agnostic to the
// parent's input type: a checkpoint / fraud-response parent — whose own input
// is a pre-signed taproot key-spend, unlike a round commitment tx — still
// yields a CPFP child whose canonical P2A anchor input is pre-finalized with an
// empty witness, so LND FinalizePsbt only has to sign the wallet fee input. The
// parent's signed input is preserved byte-for-byte; the broadcaster never
// re-finalizes it.
func TestCPFPChildFinalizesCheckpointShapedParent(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	chain.feeRate = 5

	parent, parentWitness := makeCheckpointShapedParent()
	parentTxid := parent.TxHash()

	anchorIdx := findAnchorOutput(parent)
	require.GreaterOrEqual(t, anchorIdx, 0, "parent must carry an anchor")
	anchorOutpoint := wire.OutPoint{
		Hash:  parentTxid,
		Index: uint32(anchorIdx),
	}

	feeUtxo := &walletcore.Utxo{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0xfe,
			},
			Index: 0,
		},
		Amount:   200_000,
		PkScript: p2pkhTestPkScript(t),
	}

	// inspect runs on the child PSBT before the wallet attaches dummy
	// witnesses. It directly asserts the mechanism #509 claimed was
	// missing: signCPFPChild pre-finalizes the anchor input (empty witness)
	// and leaves the wallet fee input for FinalizePsbt to sign.
	var inspected bool
	wlt := &rewritingWallet{
		utxos: []*walletcore.Utxo{
			feeUtxo,
		},
		changeScript: []byte{
			txscript.OP_TRUE,
		},
		inspect: func(p *psbt.Packet) {
			inspected = true
			for i, in := range p.UnsignedTx.TxIn {
				switch in.PreviousOutPoint {
				case anchorOutpoint:
					require.Equal(
						t, []byte{0x00},
						p.Inputs[i].FinalScriptWitness,
						"anchor input must be "+
							"pre-finalized empty",
					)

				case feeUtxo.Outpoint:
					require.Empty(
						t,
						p.Inputs[i].FinalScriptWitness,
						"fee input must be left "+
							"for the wallet to "+
							"sign",
					)
					require.NotNil(
						t, p.Inputs[i].WitnessUtxo,
					)
				}
			}
		},
	}

	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet:      wlt,
	})

	_, err := b.Submit(t.Context(), 100, &BroadcastRequest{
		Tx: parent, Label: "fraud-spent_leaf",
	})
	require.NoError(t, err)
	require.True(t, inspected, "FinalizePsbt inspect hook must have run")

	// A CPFP package — not a direct-broadcast fallback — must have been
	// submitted.
	require.Equal(t, 1, chain.packageCallCount())
	require.Zero(t, chain.broadcastCallCount())

	child := chain.packageCalls[0].Child
	require.NotNil(t, child)

	// The child's anchor input is spent with an empty witness (P2A is
	// anyone-can-spend), while the wallet fee input carries a signature.
	var sawAnchor, sawFee bool
	for _, in := range child.TxIn {
		switch in.PreviousOutPoint {
		case anchorOutpoint:
			sawAnchor = true
			require.Empty(
				t, in.Witness,
				"anchor input witness must be empty",
			)

		case feeUtxo.Outpoint:
			sawFee = true
			require.NotEmpty(
				t, in.Witness,
				"fee input must be signed by the wallet",
			)
		}
	}
	require.True(t, sawAnchor, "child must spend the parent anchor")
	require.True(t, sawFee, "child must spend the wallet fee input")

	// The parent in the submitted package is untouched: its pre-signed
	// checkpoint input witness is preserved byte-for-byte.
	submittedParent := chain.packageCalls[0].Parents[0]
	require.Equal(t, parentWitness, submittedParent.TxIn[0].Witness)
}

// TestCPFPBroadcasterFallbackAndErrors covers the lower-level generic
// broadcaster's fallback and error branches.
func TestCPFPBroadcasterFallbackAndErrors(t *testing.T) {
	t.Run("unsupported package falls back to "+
		"individual broadcast", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		chain.packageErr = fmt.Errorf("package relay not supported")
		broadcaster := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
			Wallet: &fakeWallet{
				utxos: []*walletcore.Utxo{makeWalletUTXO(t)},
			},
		})

		result, err := broadcaster.Submit(t.Context(), 100,
			&BroadcastRequest{
				Tx:    makeTestTx(true),
				Label: "anchor",
			},
		)
		require.NoError(t, err)
		require.NotNil(t, result.ChildTxid)
		require.Len(t, chain.broadcastCalls, 2)
	})

	t.Run("non-v3 parent rejected at Submit", func(t *testing.T) {
		broadcaster := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: newFakeChainSourceRef(100),
		})

		tx := makeTestTx(true)
		tx.Version = 2

		_, err := broadcaster.Submit(
			t.Context(), 100, &BroadcastRequest{
				Tx:    tx,
				Label: "not-truc",
			},
		)
		require.ErrorIs(t, err, ErrNonTRUCParent)
	})

	t.Run("submit validation and fee estimate errors", func(t *testing.T) {
		broadcaster := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: newFakeChainSourceRef(100),
		})

		_, err := broadcaster.Submit(t.Context(), 100, nil)
		require.Error(t, err)

		badResp := &staticChainSourceRef{
			handler: func(_ context.Context,
				msg chainsource.ChainSourceMsg) (
				chainsource.ChainSourceResp, error) {

				resp := &chainsource.BestHeightResponse{}

				switch msg := msg.(type) {
				case *chainsource.FeeEstimateRequest:
					_ = msg

					return resp, nil

				default:
					return resp, nil
				}
			},
		}
		broadcaster = NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: badResp,
		})

		_, err = broadcaster.EstimateFeeRate(t.Context())
		require.Error(t, err)
	})

	t.Run("wallet error branches", func(t *testing.T) {
		tx := makeTestTx(true)
		txid := tx.TxHash()

		chain := newFakeChainSourceRef(100)
		broadcaster := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
		})
		_, err := broadcaster.selectFeeInput(t.Context(), txid, 100)
		require.Error(t, err)

		broadcaster = NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
			Wallet: &failingWallet{
				listErr: fmt.Errorf("list failed"),
			},
		})
		_, err = broadcaster.selectFeeInput(t.Context(), txid, 100)
		require.Error(t, err)

		broadcaster = NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
			Wallet: &failingWallet{
				utxos: []*walletcore.Utxo{makeWalletUTXO(t)},
			},
		})
		_, err = broadcaster.deriveChangePkScript(t.Context())
		require.Error(t, err)

		chain = newFakeChainSourceRef(100)
		broadcaster = NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
			// A valid change script is required now that the
			// broadcaster derives it before fee selection so
			// it can size the child correctly from real-world
			// script classes. The ListUnspent failure below is
			// what should surface as the CPFP-unavailable error.
			Wallet: &failingWallet{
				changeScript: p2trTestPkScript(t),
				listErr:      fmt.Errorf("list failed"),
			},
		})
		result, err := broadcaster.broadcastWithCPFP(
			t.Context(), 100, &BroadcastRequest{
				Tx: tx,
			}, tx.TxHash(), 1,
		)
		require.ErrorIs(t, err, ErrCPFPFeeInputUnavailable)
		require.Nil(t, result)
		require.Equal(t, 0, chain.broadcastCallCount())

		chain = newFakeChainSourceRef(100)
		broadcaster = NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
			Wallet: &failingWallet{
				utxos: []*walletcore.Utxo{
					makeWalletUTXO(t),
				},
				changeScript: p2trTestPkScript(t),
				finalizeErr:  fmt.Errorf("finalize failed"),
			},
		})
		result, err = broadcaster.broadcastWithCPFP(
			t.Context(), 100, &BroadcastRequest{
				Tx: tx,
			}, tx.TxHash(), 1,
		)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Nil(t, result.ChildTxid)
		require.Equal(t, 1, chain.broadcastCallCount())
	})
}

// TestFeeOutpointReleasedOnCPFPFallback verifies that when CPFP child
// setup fails partway through (e.g. PSBT finalize rejects the wallet
// input), the fee-input reservation registered on the parent is
// released so the same UTXO is available to the next retry or a
// concurrent parent.
func TestFeeOutpointReleasedOnCPFPFallback(t *testing.T) {
	tx := makeTestTx(true)
	txid := tx.TxHash()
	utxo := makeWalletUTXO(t)

	chain := newFakeChainSourceRef(100)
	broadcaster := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet: &failingWallet{
			utxos:        []*walletcore.Utxo{utxo},
			changeScript: p2trTestPkScript(t),
			finalizeErr:  fmt.Errorf("finalize failed"),
		},
	})

	result, err := broadcaster.broadcastWithCPFP(
		t.Context(), 100, &BroadcastRequest{
			Tx: tx,
		},
		txid,
		1,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Nil(t, result.ChildTxid)

	// The finalize failure triggered fallbackDirectBroadcast, which
	// must release the tentatively-reserved fee outpoint so parent
	// state contains no stale UTXOs that would starve future retries.
	_, stillTracked := broadcaster.parentStates[txid]
	require.False(
		t, stillTracked,
		"parent state should be fully released after CPFP fallback",
	)
}

// TestCPFPFallbackDirectRejectIsRetryable verifies that a CPFP setup failure
// followed by a direct parent broadcast rejection does not surface as a hard
// broadcast failure. Anchor parents usually rely on the child package for fees,
// so the caller should keep the confirmation watch live and retry later.
func TestCPFPFallbackDirectRejectIsRetryable(t *testing.T) {
	tx := makeTestTx(true)
	txid := tx.TxHash()
	utxo := makeWalletUTXO(t)

	chain := newFakeChainSourceRef(100)
	chain.broadcastErr = fmt.Errorf("min relay fee not met")
	broadcaster := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet: &failingWallet{
			utxos:        []*walletcore.Utxo{utxo},
			changeScript: p2trTestPkScript(t),
			finalizeErr:  fmt.Errorf("finalize failed"),
		},
	})

	result, err := broadcaster.broadcastWithCPFP(
		t.Context(), 100, &BroadcastRequest{
			Tx: tx,
		}, txid, 1,
	)
	require.ErrorIs(t, err, ErrCPFPFeeInputUnavailable)
	require.Nil(t, result)
	require.Equal(t, 1, chain.broadcastCallCount())
	require.Equal(t, 0, chain.packageCallCount())

	_, stillTracked := broadcaster.parentStates[txid]
	require.False(
		t, stillTracked,
		"parent state should be released after CPFP fallback reject",
	)
}

// TestFeeOutpointReleasedOnPreflightFailure verifies that when
// TestMempoolAccept preflight rejects the package, the
// tentatively-reserved fee outpoint is released so the caller's next
// attempt can re-select freely.
func TestFeeOutpointReleasedOnPreflightFailure(t *testing.T) {
	tx := makeTestTx(true)
	txid := tx.TxHash()

	chain := newFakeChainSourceRef(100)
	chain.feeRate = 5
	chain.mempoolAcceptFn = func(txs []*wire.MsgTx) (
		[]chainsource.MempoolAcceptResult, error) {

		results := make([]chainsource.MempoolAcceptResult, len(txs))
		for i, tx := range txs {
			results[i] = chainsource.MempoolAcceptResult{
				Txid:     tx.TxHash(),
				Accepted: false,
				Reason:   "preflight reject",
			}
		}

		return results, nil
	}

	broadcaster := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet: &fakeWallet{
			utxos: []*walletcore.Utxo{makeWalletUTXO(t)},
		},
		PreSubmitTestMempoolAccept: true,
	})

	result, err := broadcaster.broadcastWithCPFP(
		t.Context(), 100, &BroadcastRequest{
			Tx: tx,
		},
		txid,
		1,
	)
	require.Error(t, err)
	require.Nil(t, result)

	_, stillTracked := broadcaster.parentStates[txid]
	require.False(
		t, stillTracked,
		"parent state should be released after preflight rejection",
	)
}

// TestWalletLeaseOutputLifecycle verifies the broadcaster leases a fee
// UTXO via Wallet.LeaseOutput on reservation and releases it via
// Wallet.ReleaseOutput on Evict. The in-memory reservation map remains
// the authoritative source of truth, but the wallet-level lease
// handshake must match so other subsystems sharing the same wallet
// cannot steal the UTXO while a CPFP child is in flight.
func TestWalletLeaseOutputLifecycle(t *testing.T) {
	tx := makeTestTx(true)
	txid := tx.TxHash()

	chain := newFakeChainSourceRef(100)
	chain.feeRate = 5
	wlt := &fakeWallet{
		utxos: []*walletcore.Utxo{
			makeWalletUTXO(t),
		},
	}

	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet:      wlt,
	})

	_, err := b.Submit(t.Context(), 100, &BroadcastRequest{
		Tx: tx, Label: "lease-lifecycle",
	})
	require.NoError(t, err)

	// A successful CPFP submission must lease exactly the fee
	// input's outpoint against the txconfirm LockID.
	leaseCalls, leaseExpiry, leaseLockID := wlt.leaseSnapshot()
	require.Len(
		t, leaseCalls, 1,
		"exactly one LeaseOutput call per CPFP submission",
	)
	require.Equal(t, makeWalletUTXO(t).Outpoint, leaseCalls[0])
	require.Equal(t, txconfirmLockID, leaseLockID)
	require.Equal(t, DefaultFeeInputLeaseExpiry, leaseExpiry)

	// Eviction drops txconfirm's in-memory reservation synchronously and
	// releases the backend wallet lease best-effort off the actor path.
	b.Evict(t.Context(), txid)
	require.Eventually(t, func() bool {
		releaseCalls, releaseLockID := wlt.releaseSnapshot()

		return len(releaseCalls) == 1 &&
			releaseCalls[0] == makeWalletUTXO(t).Outpoint &&
			releaseLockID == txconfirmLockID
	}, testTimeout, 10*time.Millisecond,
		"Evict must call ReleaseOutput for every leased outpoint")
}

// TestWalletLeaseReleaseDoesNotBlockEvict verifies terminal eviction cannot
// block the txconfirm actor behind a wallet backend's ReleaseOutput call.
func TestWalletLeaseReleaseDoesNotBlockEvict(t *testing.T) {
	op := makeWalletUTXO(t).Outpoint
	wlt := &blockingReleaseWallet{
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}

	b := NewCPFPBroadcaster(BroadcasterConfig{
		Wallet: wlt,
	})

	txid := chainhash.Hash{1}
	b.parentStates[txid] = &parentBumpState{
		UsedFeeOutpoints: map[wire.OutPoint]struct{}{
			op: {},
		},
	}

	done := make(chan struct{})
	go func() {
		b.Evict(t.Context(), txid)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("Evict blocked on wallet ReleaseOutput")
	}

	_, stillTracked := b.parentStates[txid]
	require.False(t, stillTracked)

	select {
	case <-wlt.started:
	case <-time.After(testTimeout):
		t.Fatalf("ReleaseOutput was not attempted")
	}

	close(wlt.unblock)
}

// TestActorValidationAndCleanup covers actor validation, cleanup, and direct
// branch behavior that the higher-level flow tests do not hit.
func TestActorValidationAndCleanup(t *testing.T) {
	t.Run("receive validation branches", func(t *testing.T) {
		behavior := NewTxBroadcasterActor(Config{
			ChainSource: newFakeChainSourceRef(100),
		})

		_, err := behavior.handleEnsure(t.Context(), nil)
		require.Error(t, err)

		_, err = behavior.handleEnsure(
			t.Context(), &EnsureConfirmedReq{},
		)
		require.Error(t, err)

		_, err = behavior.handleCancel(t.Context(), nil)
		require.Error(t, err)

		res := behavior.Receive(t.Context(), &testUnknownMsg{})
		_, err = res.Unpack()
		require.Error(t, err)
	})

	t.Run("ensure best height unexpected response", func(t *testing.T) {
		handler := func(_ context.Context,
			msg chainsource.ChainSourceMsg) (
			chainsource.ChainSourceResp, error) {

			if _, ok := msg.(*chainsource.BestHeightRequest); ok {
				return &chainsource.BroadcastTxResponse{}, nil
			}

			return &chainsource.SubscribeBlocksResponse{}, nil
		}
		behavior := NewTxBroadcasterActor(Config{
			ChainSource: &staticChainSourceRef{
				handler: handler,
			},
		})

		err := behavior.ensureBestHeight(t.Context())
		require.Error(t, err)
	})

	t.Run("ensure block subscription error", func(t *testing.T) {
		handler := func(_ context.Context,
			msg chainsource.ChainSourceMsg) (
			chainsource.ChainSourceResp, error) {

			_, ok := msg.(*chainsource.SubscribeBlocksRequest)
			if ok {
				return nil, fmt.Errorf("subscribe failed")
			}

			return &chainsource.BestHeightResponse{}, nil
		}
		behavior := NewTxBroadcasterActor(Config{
			ChainSource: &staticChainSourceRef{
				handler: handler,
			},
		})
		behavior.SetSelfRef(actor.NewChannelTellOnlyRef[Msg]("self", 1))

		err := behavior.ensureBlockSubscription(t.Context())
		require.Error(t, err)
	})

	t.Run("should fee bump helper", func(t *testing.T) {
		behavior := NewTxBroadcasterActor(Config{
			ChainSource: newFakeChainSourceRef(100),
		})
		behavior.bestHeight = 100
		awaitState := &trackedTxStateAwaitingConfirmation{
			trackedTxData: trackedTxData{
				Txid: chainhash.Hash{
					7,
				},
			},
			trackedTxProgress: trackedTxProgress{
				LastBroadcastHeight: fn.Some[int32](99),
			},
		}
		entry := newTrackedTxForState(t, awaitState)
		require.False(t, behavior.shouldFeeBump(entry))

		awaitState2 := &trackedTxStateAwaitingConfirmation{
			trackedTxData: trackedTxData{
				Txid: chainhash.Hash{
					7,
				},
			},
			trackedTxProgress: trackedTxProgress{
				LastBroadcastHeight: fn.Some[int32](98),
			},
		}
		entry = newTrackedTxForState(t, awaitState2)
		require.True(t, behavior.shouldFeeBump(entry))

		entry = newTrackedTxForState(t, &trackedTxStateConfirmed{
			trackedTxData: trackedTxData{
				Txid: chainhash.Hash{7},
			},
			trackedTxProgress: trackedTxProgress{
				LastBroadcastHeight: fn.Some[int32](98),
			},
			ConfirmHeight: 100,
		})
		require.False(t, behavior.shouldFeeBump(entry))
	})

	t.Run("notify error branches and cleanup", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		behavior := NewTxBroadcasterActor(Config{
			ChainSource: chain,
		})
		behavior.SetSelfRef(actor.NewChannelTellOnlyRef[Msg]("self", 1))
		behavior.blockSubscriptionActive = true
		awaitConf := &trackedTxStateAwaitingConfirmation{
			trackedTxData: trackedTxData{
				Txid: chainhash.Hash{
					9,
				},
				TargetConfs: 1,
			},
			trackedTxProgress: trackedTxProgress{
				LastBroadcastHeight: fn.Some[int32](99),
			},
		}
		entry := newTrackedTxForState(t, awaitConf)
		entry.subscribers["fail"] = trackedSubscriber{
			Ref: &failingNotifyRef{},
		}
		behavior.tracked[entry.data.Txid] = entry

		behavior.notifyOneFinalized(
			t.Context(), &failingNotifyRef{}, entry.data.Txid, 0, 0,
		)
		behavior.notifyOneFailed(
			t.Context(), &failingNotifyRef{}, entry.data.Txid,
			"failed",
		)

		err := behavior.OnStop(t.Context())
		require.NoError(t, err)
		require.Len(t, chain.unsubscribeBlocks, 1)
		require.Len(t, chain.unregisterConfs, 1)
	})
}

// TestApplyReplacementFloor exercises the pure fee-and-feerate comparator
// that CPFPBroadcaster.broadcastWithCPFP applies before every submission.
// These are white-box tests against the helper directly so the
// interaction between Rule 3 (absolute fee), Rule 4 (feerate), and the
// incrementalRelayFee term is pinned down independent of the broader
// broadcast flow.
func TestApplyReplacementFloor(t *testing.T) {
	parent := makeTestTx(true)
	txid := parent.TxHash()

	// Use a real P2TR pkScript for the child's fee input and change
	// output so the vsize arithmetic matches the shape a modern wallet
	// actually produces, not a hand-picked constant.
	taprootScript := p2trTestPkScript(t)
	childVSize := estimateChildVSize(taprootScript, taprootScript)
	require.Greater(t, childVSize, int64(0))

	parentVSize := (EstimateWeight(parent) + 3) / 4
	packageVSize := parentVSize + childVSize

	newBroadcaster := func(irf int64) *CPFPBroadcaster {
		cfg := BroadcasterConfig{
			ChainSource: newFakeChainSourceRef(100),

			IncrementalRelayFeeSatPerVByte: irf,
		}

		return NewCPFPBroadcaster(cfg)
	}

	t.Run("no prior state is a pass-through", func(t *testing.T) {
		b := newBroadcaster(1)

		feeRate, totalFee := b.applyReplacementFloor(
			parent, txid, 7, btcutil.Amount(7*packageVSize),
			childVSize,
		)
		require.Equal(t, int64(7), feeRate)
		require.Equal(t, btcutil.Amount(7*packageVSize), totalFee)
	})

	t.Run("flat estimator forces feerate +1", func(t *testing.T) {
		b := newBroadcaster(1)

		prevFeeRate := int64(5)
		prevFee := btcutil.Amount(prevFeeRate * packageVSize)
		b.parentStates[txid] = &parentBumpState{
			LastFeeRate:    prevFeeRate,
			LastPackageFee: prevFee,
		}

		feeRate, totalFee := b.applyReplacementFloor(
			parent, txid, prevFeeRate,
			btcutil.Amount(prevFeeRate*packageVSize), childVSize,
		)

		require.Equal(
			t, prevFeeRate+1, feeRate,
			"flat estimator must be floored to prev + 1 sat/vB",
		)
		require.GreaterOrEqual(
			t, int64(totalFee), int64(prevFee)+packageVSize,
			"Rule 3 requires additional-fee >= irf * packageVSize",
		)
	})

	t.Run("dip still clears prior feerate", func(t *testing.T) {
		b := newBroadcaster(1)

		prevFeeRate := int64(20)
		prevFee := btcutil.Amount(prevFeeRate * packageVSize)
		b.parentStates[txid] = &parentBumpState{
			LastFeeRate:    prevFeeRate,
			LastPackageFee: prevFee,
		}

		feeRate, totalFee := b.applyReplacementFloor(
			parent, txid, 3, btcutil.Amount(3*packageVSize),
			childVSize,
		)

		require.Equal(
			t, prevFeeRate+1, feeRate,
			"dip below prior must be ratcheted to prev + 1",
		)
		require.GreaterOrEqual(
			t, int64(totalFee), int64(prevFee)+1,
			"absolute replacement fee must strictly exceed prior",
		)
	})

	t.Run("rule 3 bumps when feerate tick alone is insufficient",
		func(t *testing.T) {
			// Incremental relay fee set high so the Rule 3
			// threshold dominates.
			irf := int64(5)
			b := newBroadcaster(irf)

			prevFeeRate := int64(10)
			prevFee := btcutil.Amount(prevFeeRate * packageVSize)
			b.parentStates[txid] = &parentBumpState{
				LastFeeRate:    prevFeeRate,
				LastPackageFee: prevFee,
			}

			// Raw feerate bump of +1 → naive new fee is
			// (prevFeeRate+1) * packageVSize. But Rule 3 requires
			// additional fee >= irf * packageVSize, which the +1
			// tick alone does not cover.
			feeRate, totalFee := b.applyReplacementFloor(
				parent, txid, prevFeeRate+1, btcutil.Amount(
					(prevFeeRate+1)*packageVSize,
				),
				childVSize,
			)

			require.Equal(t, prevFeeRate+1, feeRate)

			required := int64(prevFee) + irf*packageVSize
			require.GreaterOrEqual(
				t, int64(totalFee), required, "Rule 3 must "+
					"top up totalFee when feerate bump "+
					"alone is insufficient",
			)
		})

	t.Run("custom incrementalRelayFee is honored", func(t *testing.T) {
		irf := int64(3)
		b := newBroadcaster(irf)

		prevFeeRate := int64(8)
		prevFee := btcutil.Amount(prevFeeRate * packageVSize)
		b.parentStates[txid] = &parentBumpState{
			LastFeeRate:    prevFeeRate,
			LastPackageFee: prevFee,
		}

		_, totalFee := b.applyReplacementFloor(
			parent, txid, prevFeeRate, // flat estimator
			btcutil.Amount(prevFeeRate*packageVSize), childVSize,
		)

		minAdditional := irf * packageVSize
		require.GreaterOrEqual(
			t, int64(totalFee)-int64(prevFee), minAdditional,
			"additional fee must be at least irf * packageVSize",
		)
	})

	t.Run("caller totalFee larger than naive is preserved",
		func(t *testing.T) {
			b := newBroadcaster(1)

			prevFeeRate := int64(5)
			prevFee := btcutil.Amount(prevFeeRate * packageVSize)
			b.parentStates[txid] = &parentBumpState{
				LastFeeRate:    prevFeeRate,
				LastPackageFee: prevFee,
			}

			// Caller passed a fee larger than (prevFeeRate+1) *
			// packageVSize; the floor must not shrink it.
			large := btcutil.Amount(
				(prevFeeRate + 1) * packageVSize * 2,
			)

			_, totalFee := b.applyReplacementFloor(
				parent, txid, prevFeeRate, large, childVSize,
			)
			require.Equal(
				t, large, totalFee, "applyReplacementFloor "+
					"must never shrink a fee the "+
					"caller already chose",
			)
		})
}

// TestPreflightTestMempoolAccept covers the opt-in
// PreSubmitTestMempoolAccept path.
//
//   - The direct-broadcast path calls TestMempoolAccept with the single
//     parent tx.
//   - The CPFP path calls it with both parent and child as a package.
//   - A backend rejection aborts submission with the backend's reason.
//   - A backend "not supported" response is downgraded to a soft-miss
//     and submission proceeds.
func TestPreflightTestMempoolAccept(t *testing.T) {
	t.Run("package preflight precedes SubmitPackage", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		chain.feeRate = 5
		chain.mempoolAcceptFn = func(txs []*wire.MsgTx) (
			[]chainsource.MempoolAcceptResult, error) {

			require.Len(
				t, txs, 2, "CPFP path must preflight "+
					"parent+child together as a package",
			)

			return []chainsource.MempoolAcceptResult{
				{
					Txid:     txs[0].TxHash(),
					Accepted: true,
				},
				{
					Txid:     txs[1].TxHash(),
					Accepted: true,
				},
			}, nil
		}
		wallet := &fakeWallet{
			utxos: []*walletcore.Utxo{
				makeWalletUTXO(t),
			},
		}
		b := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource:                chain,
			Wallet:                     wallet,
			PreSubmitTestMempoolAccept: true,
		})

		_, err := b.Submit(t.Context(), 100, &BroadcastRequest{
			Tx: makeTestTx(true), Label: "anchor",
		})
		require.NoError(t, err)
		require.Len(
			t, chain.mempoolAcceptCalls, 1,
			"exactly one preflight call per Submit",
		)
		require.Equal(t, 1, chain.packageCallCount())
	})

	t.Run("direct-broadcast preflight is single-tx", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		chain.feeRate = 5
		chain.mempoolAcceptFn = func(txs []*wire.MsgTx) (
			[]chainsource.MempoolAcceptResult, error) {

			require.Len(
				t, txs, 1,
				"non-CPFP path must preflight only the tx",
			)

			return []chainsource.MempoolAcceptResult{
				{
					Txid:     txs[0].TxHash(),
					Accepted: true,
				},
			}, nil
		}
		b := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource:                chain,
			PreSubmitTestMempoolAccept: true,
		})

		_, err := b.Submit(t.Context(), 100, &BroadcastRequest{
			Tx: makeTestTx(false), Label: "no-anchor",
		})
		require.NoError(t, err)
		require.Len(t, chain.mempoolAcceptCalls, 1)
		require.Equal(t, 1, chain.broadcastCallCount())
	})

	t.Run("backend rejection aborts with reason", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		chain.mempoolAcceptFn = func(txs []*wire.MsgTx) (
			[]chainsource.MempoolAcceptResult, error) {

			return []chainsource.MempoolAcceptResult{
				{
					Txid:     txs[0].TxHash(),
					Accepted: false,
					Reason:   "missing-inputs",
				},
			}, nil
		}
		b := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource:                chain,
			PreSubmitTestMempoolAccept: true,
		})

		_, err := b.Submit(t.Context(), 100, &BroadcastRequest{
			Tx: makeTestTx(false), Label: "rejected",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing-inputs")
		require.Equal(
			t, 0, chain.broadcastCallCount(),
			"backend rejection must abort before broadcast",
		)
	})

	t.Run("unsupported backend is a soft-miss", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		// No mempoolAcceptFn → fake returns "not supported".
		b := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource:                chain,
			PreSubmitTestMempoolAccept: true,
		})

		_, err := b.Submit(t.Context(), 100, &BroadcastRequest{
			Tx: makeTestTx(false), Label: "unsupported-backend",
		})
		require.NoError(
			t, err,
			"an unsupported preflight must not block the submit",
		)
		require.Equal(t, 1, chain.broadcastCallCount())
	})

	t.Run("preflight disabled by default", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		chain.mempoolAcceptFn = func(txs []*wire.MsgTx) (
			[]chainsource.MempoolAcceptResult, error) {

			t.Fatal("preflight must not run when the flag is off")

			return nil, nil
		}
		b := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
		})

		_, err := b.Submit(t.Context(), 100, &BroadcastRequest{
			Tx: makeTestTx(false), Label: "no-preflight",
		})
		require.NoError(t, err)
		require.Empty(t, chain.mempoolAcceptCalls)
	})
}

// TestUsedFeeOutpointsKeyedByParent verifies Phase 3 of the CPFP
// correctness fixes: UTXO reservations are scoped to the parent that
// consumed them and survive block boundaries until Evict, while a second
// parent is prevented from picking a UTXO another parent has in flight.
func TestUsedFeeOutpointsKeyedByParent(t *testing.T) {
	t.Run("reservation survives a new block", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		chain.feeRate = 5
		utxo := makeWalletUTXO(t)
		b := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
			Wallet: &fakeWallet{
				utxos: []*walletcore.Utxo{utxo},
			},
		})

		parent := makeTestTx(true)
		txid := parent.TxHash()

		_, err := b.Submit(t.Context(), 100, &BroadcastRequest{
			Tx: parent, Label: "initial",
		})
		require.NoError(t, err)
		require.Contains(
			t, b.parentStates[txid].UsedFeeOutpoints, utxo.Outpoint,
			"Submit must record the chosen fee outpoint "+
				"against the parent",
		)

		// Advance to a higher block; under the previous
		// per-block-clear behavior this would have erased the
		// reservation. With per-parent keying it must persist.
		_, err = b.Submit(t.Context(), 200, &BroadcastRequest{
			Tx: parent, Label: "same-parent-later-block",
		})
		require.NoError(t, err)
		require.Contains(
			t, b.parentStates[txid].UsedFeeOutpoints, utxo.Outpoint,
			"reservation must persist across block boundaries",
		)
	})

	t.Run("second parent cannot reuse first parent's UTXO",
		func(t *testing.T) {
			chain := newFakeChainSourceRef(100)
			chain.feeRate = 5
			utxo := makeWalletUTXO(t)
			b := NewCPFPBroadcaster(BroadcasterConfig{
				ChainSource: chain,
				Wallet: &fakeWallet{
					utxos: []*walletcore.Utxo{utxo},
				},
			})

			parentA := makeTestTx(true)
			parentA.TxIn[0].PreviousOutPoint.Hash =
				chainhash.Hash{0xaa}
			_, err := b.Submit(t.Context(), 100, &BroadcastRequest{
				Tx: parentA, Label: "parent-a",
			})
			require.NoError(t, err)

			// Parent B, a different txid, must not be able to
			// claim the same fee UTXO while parent A is still
			// tracked.
			parentB := makeTestTx(true)
			parentB.TxIn[0].PreviousOutPoint.Hash =
				chainhash.Hash{0xbb}
			require.NotEqual(t, parentA.TxHash(), parentB.TxHash())

			_, err = b.Submit(t.Context(), 101, &BroadcastRequest{
				Tx: parentB, Label: "parent-b",
			})
			require.ErrorIs(
				t, err, ErrCPFPFeeInputUnavailable, "second "+
					"parent must be blocked from "+
					"reusing the first parent's "+
					"reserved fee UTXO",
			)
		})

	t.Run("evict releases reservation for other parents",
		func(t *testing.T) {
			chain := newFakeChainSourceRef(100)
			chain.feeRate = 5
			utxo := makeWalletUTXO(t)
			b := NewCPFPBroadcaster(BroadcasterConfig{
				ChainSource: chain,
				Wallet: &fakeWallet{
					utxos: []*walletcore.Utxo{utxo},
				},
			})

			parentA := makeTestTx(true)
			parentA.TxIn[0].PreviousOutPoint.Hash =
				chainhash.Hash{0xaa}
			_, err := b.Submit(t.Context(), 100, &BroadcastRequest{
				Tx: parentA, Label: "parent-a",
			})
			require.NoError(t, err)

			// Evict parent A; parent B should now be able to pick
			// the same UTXO.
			b.Evict(t.Context(), parentA.TxHash())

			parentB := makeTestTx(true)
			parentB.TxIn[0].PreviousOutPoint.Hash =
				chainhash.Hash{0xbb}
			_, err = b.Submit(t.Context(), 101, &BroadcastRequest{
				Tx: parentB, Label: "parent-b",
			})
			require.NoError(
				t, err, "Evict must free the fee UTXO for "+
					"other parents",
			)
		})

	t.Run("same parent re-picking own UTXO is allowed",
		func(t *testing.T) {
			chain := newFakeChainSourceRef(100)
			chain.feeRate = 5
			utxo := makeWalletUTXO(t)
			b := NewCPFPBroadcaster(BroadcasterConfig{
				ChainSource: chain,
				Wallet: &fakeWallet{
					utxos: []*walletcore.Utxo{utxo},
				},
			})

			parent := makeTestTx(true)
			result1, err := b.Submit(
				t.Context(), 100, &BroadcastRequest{
					Tx:    parent,
					Label: "bump-1",
				},
			)
			require.NoError(t, err)

			// Second submission for the SAME parent with no
			// other UTXOs available must succeed; per-parent
			// re-picking is how TRUC package RBF triggers
			// replacement via double-spending the fee input.
			result2, err := b.Submit(
				t.Context(), 101, &BroadcastRequest{
					Tx:    parent,
					Label: "bump-2",
				},
			)
			require.NoError(
				t, err, "a parent must be allowed to "+
					"re-pick a UTXO from its own "+
					"reserved set",
			)
			require.Greater(t, result2.FeeRate, result1.FeeRate)
		})

	t.Run("same parent reuses fee input hidden by wallet",
		func(t *testing.T) {
			chain := newFakeChainSourceRef(100)
			chain.feeRate = 5

			firstUTXO := makeWalletUTXO(t)
			secondUTXO := makeWalletUTXO(t)
			secondUTXO.Outpoint.Hash = chainhash.Hash{3}

			testWallet := &fakeWallet{
				utxos: []*walletcore.Utxo{
					firstUTXO, secondUTXO,
				},
			}
			b := NewCPFPBroadcaster(BroadcasterConfig{
				ChainSource: chain,
				Wallet:      testWallet,
			})

			parent := makeTestTx(true)
			txid := parent.TxHash()

			result1, err := b.Submit(t.Context(), 100,
				&BroadcastRequest{
					Tx: parent, Label: "bump-1",
				},
			)
			require.NoError(t, err)

			// btcwallet and lwwallet stop listing the fee
			// UTXO once the first CPFP child spends it in the
			// mempool. The next bump must still rebuild a child
			// spending that same reserved outpoint instead of
			// consuming another confirmed wallet UTXO.
			testWallet.utxos = []*walletcore.Utxo{secondUTXO}

			result2, err := b.Submit(t.Context(), 101,
				&BroadcastRequest{
					Tx: parent, Label: "bump-2",
				},
			)
			require.NoError(t, err)
			require.Greater(t, result2.FeeRate, result1.FeeRate)

			chain.mu.Lock()
			packages := append(
				[]*chainsource.SubmitPackageRequest(nil),
				chain.packageCalls...,
			)
			chain.mu.Unlock()

			require.Len(t, packages, 2)

			spendsOutpoint := func(tx *wire.MsgTx,
				op wire.OutPoint) bool {

				for _, txIn := range tx.TxIn {
					if txIn.PreviousOutPoint == op {
						return true
					}
				}

				return false
			}

			require.True(
				t, spendsOutpoint(
					packages[1].Child, firstUTXO.Outpoint,
				),
				"second child must reuse the parent's "+
					"cached fee input",
			)
			require.False(
				t, spendsOutpoint(
					packages[1].Child, secondUTXO.Outpoint,
				),
				"second child must not consume a fresh "+
					"wallet UTXO",
			)
			require.Contains(
				t, b.parentStates[txid].UsedFeeOutpoints,
				firstUTXO.Outpoint,
			)
			require.Len(t, b.parentStates[txid].UsedFeeOutpoints, 1)
		})
}

// TestCPFPBroadcasterFeeBumpReplacementFloor exercises the BIP-125 Rule 3
// and Rule 4 enforcement applied on every Submit after the first one.
//
// We submit the same parent repeatedly with controlled fee estimator
// behaviour and verify:
//
//   - A flat-fee estimator forces the replacement feerate up by at least
//     1 sat/vB so Rule 4 is satisfied.
//   - A dipping estimator still lands a replacement strictly above the
//     prior feerate.
//   - The absolute package fee grows by at least
//     IncrementalRelayFeeSatPerVByte * packageVSize on every bump so
//     Rule 3 is satisfied.
//   - Evict clears the per-parent bump history so a brand-new parent
//     starts from the estimator again.
func TestCPFPBroadcasterFeeBumpReplacementFloor(t *testing.T) {
	newBroadcaster := func(chain *fakeChainSourceRef) *CPFPBroadcaster {
		largeUTXO := &walletcore.Utxo{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{
					2,
				},
				Index: 1,
			},
			Amount:   5_000_000,
			PkScript: p2trTestPkScript(t),
		}

		return NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
			Wallet: &fakeWallet{
				utxos: []*walletcore.Utxo{largeUTXO},
			},
			IncrementalRelayFeeSatPerVByte: 1,
		})
	}

	parent := makeTestTx(true)
	txid := parent.TxHash()

	t.Run("flat estimator still ratchets feerate", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		chain.feeRate = 5
		b := newBroadcaster(chain)

		first, err := b.Submit(t.Context(), 100, &BroadcastRequest{
			Tx: parent, Label: "bump",
		})
		require.NoError(t, err)
		require.Equal(t, int64(5), first.FeeRate)

		second, err := b.Submit(t.Context(), 101, &BroadcastRequest{
			Tx: parent, Label: "bump",
		})
		require.NoError(t, err)
		require.Greater(
			t, second.FeeRate, first.FeeRate, "replacement "+
				"feerate must strictly exceed prior "+
				"feerate (BIP-125 Rule 4)",
		)

		prev := b.parentStates[txid].LastPackageFee
		require.Greater(t, int64(prev), int64(0))

		third, err := b.Submit(t.Context(), 102, &BroadcastRequest{
			Tx: parent, Label: "bump",
		})
		require.NoError(t, err)
		require.Greater(t, third.FeeRate, second.FeeRate)
		require.Greater(
			t, int64(b.parentStates[txid].LastPackageFee),
			int64(prev),
			"replacement absolute fee must strictly exceed "+
				"prior absolute fee (BIP-125 Rule 3)",
		)
	})

	t.Run("estimator dip ratchets up", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		chain.feeRate = 10
		b := newBroadcaster(chain)

		first, err := b.Submit(t.Context(), 100, &BroadcastRequest{
			Tx: parent, Label: "bump",
		})
		require.NoError(t, err)
		require.Equal(t, int64(10), first.FeeRate)

		chain.feeRate = 3 // estimator dips below prior feerate.

		second, err := b.Submit(t.Context(), 101, &BroadcastRequest{
			Tx: parent, Label: "bump",
		})
		require.NoError(t, err)
		require.Greater(
			t, second.FeeRate, first.FeeRate, "replacement "+
				"feerate must strictly exceed prior "+
				"feerate even when the estimator dips",
		)
	})

	t.Run("evict clears per-parent bump history", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		chain.feeRate = 5
		b := newBroadcaster(chain)

		_, err := b.Submit(t.Context(), 100, &BroadcastRequest{
			Tx: parent, Label: "bump",
		})
		require.NoError(t, err)
		require.NotNil(t, b.parentStates[txid])

		b.Evict(t.Context(), txid)
		require.Nil(
			t, b.parentStates[txid],
			"Evict must release the per-parent bump state",
		)

		// Follow-up submission starts from the raw estimator again.
		next, err := b.Submit(t.Context(), 101, &BroadcastRequest{
			Tx: parent, Label: "bump-after-evict",
		})
		require.NoError(t, err)
		require.Equal(
			t, int64(5), next.FeeRate, "after eviction, "+
				"feerate should come straight from the "+
				"estimator",
		)
	})
}

// TestSignCPFPChildHandlesWalletInputRewrites exercises signCPFPChild with
// wallets that return finalized transactions whose input composition does
// not exactly round-trip the requested PSBT. The positional-indexing
// implementation this test guards against would panic on a length
// mismatch or silently miswire witnesses when inputs are reordered; the
// outpoint-matched implementation must return a clean error in the first
// case and succeed transparently in the second.
func TestSignCPFPChildHandlesWalletInputRewrites(t *testing.T) {
	parent := makeTestTx(true)
	anchorIdx := findAnchorOutput(parent)
	require.GreaterOrEqual(t, anchorIdx, 0)

	anchorOutpoint := wire.OutPoint{
		Hash:  parent.TxHash(),
		Index: uint32(anchorIdx),
	}

	t.Run("reordered inputs still succeed", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		swap := &rewritingWallet{
			utxos: []*walletcore.Utxo{
				makeWalletUTXO(t),
			},
			changeScript: p2trTestPkScript(t),
			rewrite: func(tx *wire.MsgTx) *wire.MsgTx {
				out := tx.Copy()
				out.TxIn[0], out.TxIn[1] =
					out.TxIn[1], out.TxIn[0]

				return out
			},
		}
		broadcaster := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
			Wallet:      swap,
		})

		result, err := broadcaster.Submit(
			t.Context(), 100, &BroadcastRequest{
				Tx:    parent,
				Label: "anchor",
			},
		)
		require.NoError(t, err)
		require.NotNil(t, result.ChildTxid)
	})

	t.Run("wallet adding extra input fails cleanly", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		extra := &rewritingWallet{
			utxos: []*walletcore.Utxo{
				makeWalletUTXO(t),
			},
			changeScript: p2trTestPkScript(t),
			rewrite: func(tx *wire.MsgTx) *wire.MsgTx {
				out := tx.Copy()
				out.AddTxIn(&wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash:  chainhash.Hash{99},
						Index: 0,
					},
					Witness: wire.TxWitness{
						make([]byte, 64),
					},
				})

				return out
			},
		}
		broadcaster := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
			Wallet:      extra,
		})

		require.NotPanics(t, func() {
			_, err := broadcaster.Submit(
				t.Context(), 100, &BroadcastRequest{
					Tx:    parent,
					Label: "anchor",
				},
			)
			require.NoError(t, err)
		})

		// The sign error should have fallen back to direct parent
		// broadcast rather than crashing or submitting a malformed
		// package.
		require.Equal(t, 1, chain.broadcastCallCount())
		require.Equal(t, 0, chain.packageCallCount())
	})

	t.Run("substituted outpoint fails cleanly", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		replacement := wire.OutPoint{
			Hash: chainhash.Hash{
				123,
			},
			Index: 7,
		}
		rename := &rewritingWallet{
			utxos: []*walletcore.Utxo{
				makeWalletUTXO(t),
			},
			changeScript: p2trTestPkScript(t),
			rewrite: func(tx *wire.MsgTx) *wire.MsgTx {
				out := tx.Copy()
				for i := range out.TxIn {
					prev := out.TxIn[i].PreviousOutPoint
					if prev == anchorOutpoint {
						continue
					}
					out.TxIn[i].PreviousOutPoint =
						replacement
				}

				return out
			},
		}
		broadcaster := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
			Wallet:      rename,
		})

		require.NotPanics(t, func() {
			_, err := broadcaster.Submit(
				t.Context(), 100, &BroadcastRequest{
					Tx:    parent,
					Label: "anchor",
				},
			)
			require.NoError(t, err)
		})

		// signCPFPChild's missing-outpoint guard forces the fallback
		// to direct parent broadcast rather than submitting a
		// malformed package.
		require.Equal(t, 1, chain.broadcastCallCount())
		require.Equal(t, 0, chain.packageCallCount())
	})
}

// TestSignCPFPChildSetsFeeInputSighash asserts that the PSBT handed to the
// wallet carries an explicit sighash for the selected fee input. Leaving the
// value unset signs segwit v0 fee inputs with byte 0x00, which is only valid
// for taproot key spends.
func TestSignCPFPChildSetsFeeInputSighash(t *testing.T) {
	testCases := []struct {
		name     string
		pkScript []byte
		expected txscript.SigHashType
	}{
		{
			name:     "taproot default",
			pkScript: p2trTestPkScript(t),
			expected: txscript.SigHashDefault,
		},
		{
			name:     "p2wkh all",
			pkScript: p2wkhTestPkScript(t),
			expected: txscript.SigHashAll,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			parent := makeTestTx(true)
			feeUTXO := makeWalletUTXO(t)
			feeUTXO.PkScript = testCase.pkScript

			var got txscript.SigHashType
			wallet := &rewritingWallet{
				utxos: []*walletcore.Utxo{
					feeUTXO,
				},
				changeScript: p2trTestPkScript(t),
				inspect: func(p *psbt.Packet) {
					for i, txIn := range p.UnsignedTx.TxIn {
						op := txIn.PreviousOutPoint
						if op != feeUTXO.Outpoint {
							continue
						}

						got = p.Inputs[i].SighashType
					}
				},
			}

			broadcaster := NewCPFPBroadcaster(BroadcasterConfig{
				ChainSource: newFakeChainSourceRef(100),
				Wallet:      wallet,
			})

			_, err := broadcaster.Submit(
				t.Context(), 100, &BroadcastRequest{
					Tx:    parent,
					Label: "anchor",
				},
			)
			require.NoError(t, err)
			require.Equal(t, testCase.expected, got)
		})
	}
}

func TestFeeInputFanoutNoopWhenSupplyAvailable(t *testing.T) {
	t.Parallel()

	broadcasts := 0
	chain := &staticChainSourceRef{
		handler: func(context.Context, chainsource.ChainSourceMsg) (
			chainsource.ChainSourceResp, error) {

			broadcasts++

			return nil, fmt.Errorf("unexpected broadcast")
		},
	}
	wallet := &rewritingWallet{
		utxos: []*walletcore.Utxo{
			makeWalletUTXOWithAmount(20_000, 1),
			makeWalletUTXOWithAmount(20_000, 2),
		},
		changeScript: p2trTestPkScript(t),
	}
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet:      wallet,
	})
	c := newTestFeeBumpController(t, b)

	pending, err := c.EnsureSupply(
		t.Context(), []feeInputDemand{
			{parentTxid: chainhash.Hash{1}, minAmount: 10_000},
			{parentTxid: chainhash.Hash{2}, minAmount: 10_000},
		}, 5, 100, 1,
	)
	require.NoError(t, err)
	require.Nil(t, pending)
	require.Zero(t, broadcasts)
}

func TestFeeInputFanoutReservesPredictedOutputs(t *testing.T) {
	t.Parallel()

	var fanoutTxid chainhash.Hash
	chain := &staticChainSourceRef{
		handler: func(_ context.Context,
			msg chainsource.ChainSourceMsg) (
			chainsource.ChainSourceResp, error) {

			req, ok := msg.(*chainsource.BroadcastTxRequest)
			require.True(t, ok)
			fanoutTxid = req.Tx.TxHash()

			return &chainsource.BroadcastTxResponse{
				Txid: fanoutTxid,
			}, nil
		},
	}
	wallet := &rewritingWallet{
		utxos: []*walletcore.Utxo{
			makeWalletUTXOWithAmount(15_000, 9),
		},
		changeScript: p2trTestPkScript(t),
	}
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet:      wallet,
	})
	c := newTestFeeBumpController(t, b)

	parentA := chainhash.Hash{1}
	parentB := chainhash.Hash{2}
	pending, err := c.EnsureSupply(
		t.Context(), []feeInputDemand{
			{parentTxid: parentA, minAmount: 10_000},
			{parentTxid: parentB, minAmount: 20_000},
		}, 5, 100, 1,
	)
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.Equal(t, fanoutTxid, pending.txid)
	require.Nil(t, b.parentStates[parentA])
	require.Len(t, b.parentStates[parentB].PredictedFeeInputs, 1)
	require.Nil(t, b.selectReservedFeeInput(parentA, 10_000))

	c.OnFanoutConfirmed(t.Context(), fanoutTxid)
	require.Nil(t, b.selectReservedFeeInput(parentA, 10_000))
	require.NotNil(t, b.selectReservedFeeInput(parentB, 20_000))
	require.Nil(t, c.PendingFanout())
}

func TestFeeInputFanoutCanReplaceReservedInput(t *testing.T) {
	t.Parallel()

	var fanoutTx *wire.MsgTx
	chain := &staticChainSourceRef{
		handler: func(_ context.Context,
			msg chainsource.ChainSourceMsg) (
			chainsource.ChainSourceResp, error) {

			req, ok := msg.(*chainsource.BroadcastTxRequest)
			require.True(t, ok)
			fanoutTx = req.Tx.Copy()

			return &chainsource.BroadcastTxResponse{
				Txid: req.Tx.TxHash(),
			}, nil
		},
	}
	wallet := &rewritingWallet{
		changeScript: p2trTestPkScript(t),
	}
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet:      wallet,
	})
	c := newTestFeeBumpController(t, b)

	parentA := chainhash.Hash{1}
	parentB := chainhash.Hash{2}
	reserved := makeWalletUTXOWithAmount(100_000, 7)
	b.reserveFeeInput(t.Context(), parentA, &FeeInput{
		Outpoint: reserved.Outpoint,
		Output: &wire.TxOut{
			Value:    int64(reserved.Amount),
			PkScript: append([]byte(nil), reserved.PkScript...),
		},
		Confirmed: true,
	})

	pending, err := c.EnsureSupply(
		t.Context(), []feeInputDemand{
			{parentTxid: parentA, minAmount: 10_000},
			{parentTxid: parentB, minAmount: 10_000},
		}, 5, 100, 1,
	)
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.NotNil(t, fanoutTx)
	require.GreaterOrEqual(t, len(fanoutTx.TxOut), minFeeInputFanoutOutputs)
	require.Empty(t, b.parentStates[parentA].UsedFeeInputs)
	require.Len(t, b.parentStates[parentA].PredictedFeeInputs, 1)
	require.Len(t, b.parentStates[parentB].PredictedFeeInputs, 1)
}

func TestFeeInputFanoutRejectsRewrittenOutputs(t *testing.T) {
	t.Parallel()

	chain := &staticChainSourceRef{
		handler: func(context.Context, chainsource.ChainSourceMsg) (
			chainsource.ChainSourceResp, error) {

			return nil, fmt.Errorf("unexpected broadcast")
		},
	}
	wallet := &rewritingWallet{
		utxos: []*walletcore.Utxo{
			makeWalletUTXOWithAmount(100_000, 3),
		},
		changeScript: p2trTestPkScript(t),
		rewrite: func(tx *wire.MsgTx) *wire.MsgTx {
			tx = tx.Copy()
			tx.TxOut[0].Value++

			return tx
		},
	}
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet:      wallet,
	})
	c := newTestFeeBumpController(t, b)

	_, err := c.EnsureSupply(
		t.Context(), []feeInputDemand{
			{parentTxid: chainhash.Hash{1}, minAmount: 10_000},
			{parentTxid: chainhash.Hash{2}, minAmount: 10_000},
		}, 5, 100, 1,
	)
	require.ErrorContains(t, err, "fanout output 0 changed")
}

func TestFeeInputFanoutRebroadcastsPendingFanout(t *testing.T) {
	t.Parallel()

	var broadcasts []*wire.MsgTx
	chain := &staticChainSourceRef{
		handler: func(_ context.Context,
			msg chainsource.ChainSourceMsg) (
			chainsource.ChainSourceResp, error) {

			req, ok := msg.(*chainsource.BroadcastTxRequest)
			require.True(t, ok)
			broadcasts = append(broadcasts, req.Tx.Copy())

			return &chainsource.BroadcastTxResponse{
				Txid: req.Tx.TxHash(),
			}, nil
		},
	}
	wallet := &rewritingWallet{
		utxos: []*walletcore.Utxo{
			makeWalletUTXOWithAmount(50_000, 3),
		},
		changeScript: p2trTestPkScript(t),
	}
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet:      wallet,
	})
	c := newTestFeeBumpController(t, b)

	demands := []feeInputDemand{
		{
			parentTxid: chainhash.Hash{
				1,
			},
			minAmount: 10_000,
		},
		{
			parentTxid: chainhash.Hash{
				2,
			},
			minAmount: 10_000,
		},
	}
	pending, err := c.EnsureSupply(t.Context(), demands, 5, 100, 2)
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.Len(t, broadcasts, 1)

	_, err = c.EnsureSupply(t.Context(), demands, 5, 101, 2)
	require.NoError(t, err)
	require.Len(t, broadcasts, 1)

	_, err = c.EnsureSupply(t.Context(), demands, 5, 102, 2)
	require.NoError(t, err)
	require.Len(t, broadcasts, 2)
	require.Equal(t, broadcasts[0].TxHash(), broadcasts[1].TxHash())
	require.NotNil(t, c.PendingFanout())
}

func TestFeeInputFanoutClearsRejectedPendingFanout(t *testing.T) {
	t.Parallel()

	broadcasts := 0
	chain := &staticChainSourceRef{
		handler: func(_ context.Context,
			msg chainsource.ChainSourceMsg) (
			chainsource.ChainSourceResp, error) {

			req, ok := msg.(*chainsource.BroadcastTxRequest)
			require.True(t, ok)
			broadcasts++
			if broadcasts == 2 {
				return nil, fmt.Errorf("fanout rejected")
			}

			return &chainsource.BroadcastTxResponse{
				Txid: req.Tx.TxHash(),
			}, nil
		},
	}
	wallet := &rewritingWallet{
		utxos: []*walletcore.Utxo{
			makeWalletUTXOWithAmount(50_000, 4),
		},
		changeScript: p2trTestPkScript(t),
	}
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet:      wallet,
	})
	c := newTestFeeBumpController(t, b)

	parentA := chainhash.Hash{1}
	parentB := chainhash.Hash{2}
	demands := []feeInputDemand{
		{
			parentTxid: parentA,
			minAmount:  10_000,
		},
		{
			parentTxid: parentB,
			minAmount:  10_000,
		},
	}
	_, err := c.EnsureSupply(t.Context(), demands, 5, 100, 1)
	require.NoError(t, err)
	require.NotNil(t, c.PendingFanout())

	_, err = c.EnsureSupply(t.Context(), demands, 5, 101, 1)
	require.NoError(t, err)
	pending := c.PendingFanout()
	require.NotNil(t, pending)
	require.NotEmpty(t, pending.assignments)
	require.NotNil(t, b.parentStates[parentB])
	require.Equal(t, 3, broadcasts)
}

func TestFeeInputFanoutEvictPrunesPendingAssignments(t *testing.T) {
	t.Parallel()

	chain := &staticChainSourceRef{
		handler: func(_ context.Context,
			msg chainsource.ChainSourceMsg) (
			chainsource.ChainSourceResp, error) {

			req, ok := msg.(*chainsource.BroadcastTxRequest)
			require.True(t, ok)

			return &chainsource.BroadcastTxResponse{
				Txid: req.Tx.TxHash(),
			}, nil
		},
	}
	wallet := &rewritingWallet{
		utxos: []*walletcore.Utxo{
			makeWalletUTXOWithAmount(50_000, 5),
		},
		changeScript: p2trTestPkScript(t),
	}
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet:      wallet,
	})
	c := newTestFeeBumpController(t, b)

	parent := chainhash.Hash{2}
	_, err := c.EnsureSupply(
		t.Context(), []feeInputDemand{
			{parentTxid: chainhash.Hash{1}, minAmount: 10_000},
			{parentTxid: parent, minAmount: 10_000},
		}, 5, 100, 1,
	)
	require.NoError(t, err)
	pending := c.PendingFanout()
	require.NotNil(t, pending)
	require.Contains(t, pending.assignments, parent)

	b.Evict(t.Context(), parent)
	c.PruneParent(t.Context(), parent)
	require.Nil(t, c.PendingFanout())
	require.Nil(t, b.parentStates[parent])
}

func TestFeeInputFanoutPromotionSkipsEvictedParents(t *testing.T) {
	t.Parallel()

	chain := &staticChainSourceRef{
		handler: func(_ context.Context,
			msg chainsource.ChainSourceMsg) (
			chainsource.ChainSourceResp, error) {

			req, ok := msg.(*chainsource.BroadcastTxRequest)
			require.True(t, ok)

			return &chainsource.BroadcastTxResponse{
				Txid: req.Tx.TxHash(),
			}, nil
		},
	}
	wallet := &rewritingWallet{
		utxos: []*walletcore.Utxo{
			makeWalletUTXOWithAmount(50_000, 6),
		},
		changeScript: p2trTestPkScript(t),
	}
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet:      wallet,
	})
	c := newTestFeeBumpController(t, b)

	parent := chainhash.Hash{2}
	pending, err := c.EnsureSupply(
		t.Context(), []feeInputDemand{
			{parentTxid: chainhash.Hash{1}, minAmount: 10_000},
			{parentTxid: parent, minAmount: 10_000},
		}, 5, 100, 1,
	)
	require.NoError(t, err)
	require.NotNil(t, pending)

	delete(b.parentStates, parent)
	c.OnFanoutConfirmed(t.Context(), pending.txid)
	require.Nil(t, b.parentStates[parent])
	require.Nil(t, c.PendingFanout())
}

// testFeeBump is a thin test seam over the fanout FSM. It drives the FSM
// directly via AskEvent and exposes the same surface the old
// FeeBumpInputController had (EnsureSupply / PendingFanout / OnFanoutConfirmed
// / PruneParent) so the fanout tests can assert behavior without depending on
// the actor. The watch register/unregister outbox events are not actioned here
// (no chainsource conf watch in these unit tests); the tests assert on FSM
// state and the shared reservation map only.
type testFeeBump struct {
	t   *testing.T
	fsm *feeBumpStateMachine
	env *feeBumpEnvironment
}

// EnsureSupply feeds a demand observation into the FSM and returns the
// in-flight fanout (if any), surfacing the per-turn operational error the
// transition stashed rather than returned.
func (c *testFeeBump) EnsureSupply(ctx context.Context,
	demands []feeInputDemand, feeRate int64, height, retryInterval int32) (
	*pendingFeeInputFanout, error) {

	c.t.Helper()

	_, err := c.fsm.AskEvent(ctx, &feeBumpDemandsObserved{
		demands:       demands,
		feeRate:       feeRate,
		height:        height,
		retryInterval: retryInterval,
	}).Await(ctx).Unpack()
	require.NoError(c.t, err)

	if turnErr := c.env.takeLastErr(); turnErr != nil {
		return nil, turnErr
	}

	return c.PendingFanout(), nil
}

// PendingFanout returns the in-flight fanout the FSM is tracking, or nil if it
// is idle.
func (c *testFeeBump) PendingFanout() *pendingFeeInputFanout {
	c.t.Helper()

	rawState, err := c.fsm.CurrentState()
	require.NoError(c.t, err)

	state, ok := rawState.(feeBumpState)
	require.True(c.t, ok)

	return feeBumpPendingFanout(state)
}

// OnFanoutConfirmed feeds a confirmation event into the FSM.
func (c *testFeeBump) OnFanoutConfirmed(ctx context.Context,
	txid chainhash.Hash) {

	c.t.Helper()

	_, err := c.fsm.AskEvent(ctx, &feeBumpFanoutConfirmedEvent{
		txid: txid,
	}).Await(ctx).Unpack()
	require.NoError(c.t, err)
}

// PruneParent feeds a parent-eviction event into the FSM.
func (c *testFeeBump) PruneParent(ctx context.Context, parent chainhash.Hash) {
	c.t.Helper()

	_, err := c.fsm.AskEvent(ctx, &feeBumpParentEvicted{
		parentTxid: parent,
	}).Await(ctx).Unpack()
	require.NoError(c.t, err)
}

// newTestFeeBumpController builds a fanout FSM bound to the supplied
// broadcaster, starts it, and registers a cleanup to stop it when the test
// ends. It returns a thin seam exposing the legacy controller surface so the
// existing fanout tests drive the FSM directly.
func newTestFeeBumpController(t *testing.T, b *CPFPBroadcaster) *testFeeBump {
	t.Helper()

	fsm, env := newFeeBumpStateMachine(b, btclog.Disabled)
	fsm.Start(t.Context())
	t.Cleanup(fsm.Stop)

	return &testFeeBump{
		t:   t,
		fsm: fsm,
		env: env,
	}
}

func makeWalletUTXOWithAmount(amount btcutil.Amount,
	seed byte) *walletcore.Utxo {

	return &walletcore.Utxo{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				seed,
			},
			Index: 0,
		},
		PkScript: []byte{
			txscript.OP_TRUE,
		},
		Amount: amount,
	}
}

// TestSelectFeeInputDeprioritizesSuspects verifies that fee-input
// selection prefers coins with a clean signing history over coins whose
// CPFP child previously failed to sign, while still falling back to a
// suspect coin when nothing else qualifies.
func TestSelectFeeInputDeprioritizesSuspects(t *testing.T) {
	t.Parallel()

	tx := makeTestTx(true)
	txid := tx.TxHash()

	small := makeWalletUTXOWithAmount(5_000, 0x01)
	big := makeWalletUTXOWithAmount(50_000, 0x02)

	newBroadcaster := func() *CPFPBroadcaster {
		return NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: newFakeChainSourceRef(100),
			Wallet: &fakeWallet{
				utxos: []*walletcore.Utxo{small, big},
			},
		})
	}

	t.Run("clean history keeps smallest-first", func(t *testing.T) {
		b := newBroadcaster()

		feeInput, err := b.selectFeeInput(t.Context(), txid, 100)
		require.NoError(t, err)
		require.Equal(t, small.Outpoint, feeInput.Outpoint)
	})

	t.Run("suspect loses to bigger clean coin", func(t *testing.T) {
		b := newBroadcaster()
		b.suspectFeeInputs[small.Outpoint] = struct{}{}

		feeInput, err := b.selectFeeInput(t.Context(), txid, 100)
		require.NoError(t, err)
		require.Equal(t, big.Outpoint, feeInput.Outpoint)
	})

	t.Run("suspect used as last resort", func(t *testing.T) {
		b := newBroadcaster()
		b.suspectFeeInputs[small.Outpoint] = struct{}{}
		b.suspectFeeInputs[big.Outpoint] = struct{}{}

		feeInput, err := b.selectFeeInput(t.Context(), txid, 100)
		require.NoError(t, err)
		require.Equal(t, small.Outpoint, feeInput.Outpoint)
	})
}

// TestFallbackDirectBroadcastMarksSuspects verifies that only a failure
// at the CPFP child signing stage indicts the selected fee input; other
// setup stages leave the input's history clean.
func TestFallbackDirectBroadcastMarksSuspects(t *testing.T) {
	t.Parallel()

	tx := makeTestTx(true)
	txid := tx.TxHash()
	req := &BroadcastRequest{Tx: tx, Label: "suspect-test"}
	op := makeWalletUTXOWithAmount(5_000, 0x03).Outpoint

	t.Run("sign failure marks the input", func(t *testing.T) {
		b := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: newFakeChainSourceRef(100),
		})

		_, err := b.fallbackDirectBroadcast(
			t.Context(), req, txid, op, stageSignCPFPChild,
			fmt.Errorf("PSBT is not finalizable"),
		)
		require.NoError(t, err)
		require.Contains(t, b.suspectFeeInputs, op)
	})

	t.Run("other stages do not mark", func(t *testing.T) {
		b := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: newFakeChainSourceRef(100),
		})

		_, err := b.fallbackDirectBroadcast(
			t.Context(), req, txid, op, "build_cpfp_child",
			fmt.Errorf("boom"),
		)
		require.NoError(t, err)
		require.NotContains(t, b.suspectFeeInputs, op)
	})

	t.Run("zero outpoint is never marked", func(t *testing.T) {
		b := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: newFakeChainSourceRef(100),
		})

		_, err := b.fallbackDirectBroadcast(
			t.Context(), req, txid, wire.OutPoint{},
			stageSignCPFPChild, fmt.Errorf("boom"),
		)
		require.NoError(t, err)
		require.Empty(t, b.suspectFeeInputs)
	})
}

// TestSubmitClearsSuspectAfterSuccessfulSign verifies the end-to-end
// recovery path: a coin marked suspect from an earlier transient signing
// failure is still usable as a last resort, and a successful sign clears
// the suspicion.
func TestSubmitClearsSuspectAfterSuccessfulSign(t *testing.T) {
	t.Parallel()

	utxo := makeWalletUTXO(t)

	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: newFakeChainSourceRef(100),
		Wallet: &fakeWallet{
			utxos: []*walletcore.Utxo{utxo},
		},
	})

	// Simulate an earlier signing refusal of the wallet's only coin.
	b.suspectFeeInputs[utxo.Outpoint] = struct{}{}

	result, err := b.Submit(t.Context(), 100, &BroadcastRequest{
		Tx:    makeTestTx(true),
		Label: "suspect-recovery",
	})
	require.NoError(t, err)
	require.NotNil(t, result.ChildTxid)

	// The sign succeeded, so the coin's history is clean again.
	require.NotContains(t, b.suspectFeeInputs, utxo.Outpoint)
}
