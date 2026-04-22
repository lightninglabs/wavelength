package txconfirm

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/wallet"
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
	utxos        []*wallet.Utxo
}

// ListUnspent returns the configured result.
func (w *failingWallet) ListUnspent(_ context.Context,
	_, _ int32) ([]*wallet.Utxo, error) {

	return w.utxos, w.listErr
}

// NewWalletPkScript returns the configured result.
func (w *failingWallet) NewWalletPkScript(_ context.Context) ([]byte, error) {
	return w.changeScript, w.changeErr
}

// FinalizePsbt returns the configured result.
func (w *failingWallet) FinalizePsbt(_ context.Context,
	_ []byte) (*wire.MsgTx, error) {

	if w.finalizeErr != nil {
		return nil, w.finalizeErr
	}

	return wire.NewMsgTx(3), nil
}

// rewritingWallet is a wallet test double that parses the PSBT it is
// given, attaches dummy finalized witnesses, and optionally hands the
// resulting transaction through a caller-supplied rewrite hook. It is
// used to exercise signCPFPChild's robustness to wallets that return
// finalized transactions whose input composition does not round-trip the
// requested PSBT (reordered inputs, added inputs, substituted outpoints).
type rewritingWallet struct {
	utxos        []*wallet.Utxo
	changeScript []byte

	// rewrite, when non-nil, receives the default finalized tx and
	// returns the tx that the wallet will hand back to the caller. The
	// default finalized tx has every input's witness set to a dummy
	// 64-byte value except for inputs whose PSBT FinalScriptWitness
	// was pre-set (the anchor) which receive an empty witness.
	rewrite func(*wire.MsgTx) *wire.MsgTx
}

// ListUnspent returns the configured UTXOs.
func (w *rewritingWallet) ListUnspent(_ context.Context,
	_, _ int32) ([]*wallet.Utxo, error) {

	return w.utxos, nil
}

// NewWalletPkScript returns the configured change script.
func (w *rewritingWallet) NewWalletPkScript(
	_ context.Context) ([]byte, error) {

	if len(w.changeScript) == 0 {
		return []byte{txscript.OP_TRUE}, nil
	}

	return w.changeScript, nil
}

// FinalizePsbt parses the supplied PSBT, applies dummy witnesses, and
// then runs the configured rewrite hook (if any) before returning.
func (w *rewritingWallet) FinalizePsbt(_ context.Context,
	packetBytes []byte) (*wire.MsgTx, error) {

	packet, err := psbt.NewFromRawBytes(bytes.NewReader(packetBytes), false)
	if err != nil {
		return nil, err
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
		subscribers: make(map[string]actor.TellOnlyRef[Notification]),
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
			t, "EnsureConfirmedResp",
			ensureResp.MessageType(),
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
			t, "confirmationObservedMsg",
			confMsg.MessageType(),
		)

		blockMsg := &blockEpochObservedMsg{}
		blockMsg.txConfirmMsgSealed()
		require.Equal(
			t, "blockEpochObservedMsg",
			blockMsg.MessageType(),
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
				Hash:  chainhash.Hash{3},
				Index: 2,
			},
			Output: &wire.TxOut{
				Value:    10_000,
				PkScript: []byte{txscript.OP_TRUE},
			},
			Confirmed: true,
		}

		child, err := BuildCPFPChild(
			tx.Version,
			wire.OutPoint{Hash: tx.TxHash(), Index: 1},
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
		require.Equal(t,
			wire.MaxTxInSequenceNum, child.TxIn[0].Sequence)
		require.Equal(t,
			wire.MaxTxInSequenceNum-2, child.TxIn[1].Sequence)

		dustChild, err := BuildCPFPChild(
			tx.Version,
			wire.OutPoint{Hash: tx.TxHash(), Index: 1},
			tx.TxOut[1],
			feeInput,
			[]byte{txscript.OP_TRUE},
			btcutil.Amount(feeInput.Output.Value),
		)
		require.NoError(t, err)
		require.Empty(t, dustChild.TxOut)

		_, err = BuildCPFPChild(
			tx.Version,
			wire.OutPoint{},
			tx.TxOut[1],
			&FeeInput{Confirmed: false},
			nil,
			1,
		)
		require.Error(t, err)
	})

	t.Run("select fee input", func(t *testing.T) {
		feeInputs := []FeeInput{
			{
				Outpoint: wire.OutPoint{
					Hash:  chainhash.Hash{1},
					Index: 1,
				},
				Output: &wire.TxOut{
					Value: 1000,
				},
				Confirmed: true,
			},
			{
				Outpoint: wire.OutPoint{
					Hash:  chainhash.Hash{2},
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
		require.True(t, IsIgnorableBroadcastError(
			fmt.Errorf("already known"),
		))
		require.False(t, IsIgnorableBroadcastError(
			fmt.Errorf("fatal"),
		))
		require.True(t, isPackageSubmissionUnsupported(
			fmt.Errorf("package relay not supported"),
		))
		require.False(t, isPackageSubmissionUnsupported(
			fmt.Errorf("fatal"),
		))
	})
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
				utxos: []*wallet.Utxo{makeWalletUTXO()},
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

		_, err := broadcaster.Submit(t.Context(), 100,
			&BroadcastRequest{Tx: tx, Label: "not-truc"},
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
				msg chainsource.ChainSourceMsg,
			) (chainsource.ChainSourceResp, error) {

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
				utxos: []*wallet.Utxo{makeWalletUTXO()},
			},
		})
		_, err = broadcaster.deriveChangePkScript(t.Context())
		require.Error(t, err)

		chain = newFakeChainSourceRef(100)
		broadcaster = NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
			Wallet: &failingWallet{
				listErr: fmt.Errorf("list failed"),
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
				utxos:        []*wallet.Utxo{makeWalletUTXO()},
				changeScript: []byte{txscript.OP_TRUE},
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
			msg chainsource.ChainSourceMsg,
		) (chainsource.ChainSourceResp, error) {

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
			msg chainsource.ChainSourceMsg,
		) (chainsource.ChainSourceResp, error) {

			_, ok := msg.(*chainsource.SubscribeBlocksRequest)
			if ok {
				return nil, fmt.Errorf(
					"subscribe failed",
				)
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
				Txid: chainhash.Hash{7},
			},
			trackedTxProgress: trackedTxProgress{
				LastBroadcastHeight: 99,
			},
		}
		entry := newTrackedTxForState(t, awaitState)
		require.False(t, behavior.shouldFeeBump(entry))

		awaitState2 := &trackedTxStateAwaitingConfirmation{
			trackedTxData: trackedTxData{
				Txid: chainhash.Hash{7},
			},
			trackedTxProgress: trackedTxProgress{
				LastBroadcastHeight: 98,
			},
		}
		entry = newTrackedTxForState(t, awaitState2)
		require.True(t, behavior.shouldFeeBump(entry))

		entry = newTrackedTxForState(t, &trackedTxStateConfirmed{
			trackedTxData: trackedTxData{
				Txid: chainhash.Hash{7},
			},
			trackedTxProgress: trackedTxProgress{
				LastBroadcastHeight: 98,
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
				Txid:        chainhash.Hash{9},
				TargetConfs: 1,
			},
			trackedTxProgress: trackedTxProgress{
				LastBroadcastHeight: 99,
			},
		}
		entry := newTrackedTxForState(t, awaitConf)
		entry.subscribers["fail"] = &failingNotifyRef{}
		behavior.tracked[entry.data.Txid] = entry

		behavior.notifyOneConfirmed(
			t.Context(), &failingNotifyRef{},
			entry.data.Txid, 1, 1,
		)
		behavior.notifyOneFailed(
			t.Context(), &failingNotifyRef{},
			entry.data.Txid, "failed",
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

	parentVSize := (EstimateWeight(parent) + 3) / 4
	packageVSize := parentVSize + int64(ChildVSizeEstimate)

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
			btcutil.Amount(prevFeeRate*packageVSize),
		)

		require.Equal(t, prevFeeRate+1, feeRate,
			"flat estimator must be floored to prev + 1 sat/vB")
		require.GreaterOrEqual(t, int64(totalFee),
			int64(prevFee)+packageVSize,
			"Rule 3 requires additional-fee >= irf * packageVSize")
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
		)

		require.Equal(t, prevFeeRate+1, feeRate,
			"dip below prior must be ratcheted to prev + 1")
		require.GreaterOrEqual(t, int64(totalFee), int64(prevFee)+1,
			"absolute replacement fee must strictly exceed prior")
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
				parent, txid, prevFeeRate+1,
				btcutil.Amount((prevFeeRate+1)*packageVSize),
			)

			require.Equal(t, prevFeeRate+1, feeRate)

			required := int64(prevFee) + irf*packageVSize
			require.GreaterOrEqual(t, int64(totalFee), required,
				"Rule 3 must top up totalFee when feerate "+
					"bump alone is insufficient")
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
			btcutil.Amount(prevFeeRate*packageVSize),
		)

		minAdditional := irf * packageVSize
		require.GreaterOrEqual(t,
			int64(totalFee)-int64(prevFee), minAdditional,
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
				parent, txid, prevFeeRate, large,
			)
			require.Equal(t, large, totalFee,
				"applyReplacementFloor must never shrink "+
					"a fee the caller already chose")
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
		chain.mempoolAcceptFn = func(
			txs []*wire.MsgTx,
		) ([]chainsource.MempoolAcceptResult, error) {

			require.Len(t, txs, 2,
				"CPFP path must preflight parent+child "+
					"together as a package")

			return []chainsource.MempoolAcceptResult{
				{Txid: txs[0].TxHash(), Accepted: true},
				{Txid: txs[1].TxHash(), Accepted: true},
			}, nil
		}
		wallet := &fakeWallet{
			utxos: []*wallet.Utxo{makeWalletUTXO()},
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
		require.Len(t, chain.mempoolAcceptCalls, 1,
			"exactly one preflight call per Submit")
		require.Equal(t, 1, chain.packageCallCount())
	})

	t.Run("direct-broadcast preflight is single-tx", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		chain.feeRate = 5
		chain.mempoolAcceptFn = func(
			txs []*wire.MsgTx,
		) ([]chainsource.MempoolAcceptResult, error) {

			require.Len(t, txs, 1,
				"non-CPFP path must preflight only the tx")

			return []chainsource.MempoolAcceptResult{
				{Txid: txs[0].TxHash(), Accepted: true},
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
		chain.mempoolAcceptFn = func(
			txs []*wire.MsgTx,
		) ([]chainsource.MempoolAcceptResult, error) {

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
		require.Equal(t, 0, chain.broadcastCallCount(),
			"backend rejection must abort before broadcast")
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
		require.NoError(t, err,
			"an unsupported preflight must not block the submit")
		require.Equal(t, 1, chain.broadcastCallCount())
	})

	t.Run("preflight disabled by default", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		chain.mempoolAcceptFn = func(
			txs []*wire.MsgTx,
		) ([]chainsource.MempoolAcceptResult, error) {

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
		utxo := makeWalletUTXO()
		b := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
			Wallet:      &fakeWallet{utxos: []*wallet.Utxo{utxo}},
		})

		parent := makeTestTx(true)
		txid := parent.TxHash()

		_, err := b.Submit(t.Context(), 100, &BroadcastRequest{
			Tx: parent, Label: "initial",
		})
		require.NoError(t, err)
		require.Contains(t,
			b.parentStates[txid].UsedFeeOutpoints, utxo.Outpoint,
			"Submit must record the chosen fee outpoint against "+
				"the parent")

		// Advance to a higher block; under the previous
		// per-block-clear behavior this would have erased the
		// reservation. With per-parent keying it must persist.
		_, err = b.Submit(t.Context(), 200, &BroadcastRequest{
			Tx: parent, Label: "same-parent-later-block",
		})
		require.NoError(t, err)
		require.Contains(t,
			b.parentStates[txid].UsedFeeOutpoints, utxo.Outpoint,
			"reservation must persist across block boundaries")
	})

	t.Run("second parent cannot reuse first parent's UTXO",
		func(t *testing.T) {
			chain := newFakeChainSourceRef(100)
			chain.feeRate = 5
			utxo := makeWalletUTXO()
			b := NewCPFPBroadcaster(BroadcasterConfig{
				ChainSource: chain,
				Wallet: &fakeWallet{
					utxos: []*wallet.Utxo{utxo},
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
			require.ErrorIs(t, err, ErrCPFPFeeInputUnavailable,
				"second parent must be blocked from reusing "+
					"the first parent's reserved fee UTXO")
		})

	t.Run("evict releases reservation for other parents",
		func(t *testing.T) {
			chain := newFakeChainSourceRef(100)
			chain.feeRate = 5
			utxo := makeWalletUTXO()
			b := NewCPFPBroadcaster(BroadcasterConfig{
				ChainSource: chain,
				Wallet: &fakeWallet{
					utxos: []*wallet.Utxo{utxo},
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
			b.Evict(parentA.TxHash())

			parentB := makeTestTx(true)
			parentB.TxIn[0].PreviousOutPoint.Hash =
				chainhash.Hash{0xbb}
			_, err = b.Submit(t.Context(), 101, &BroadcastRequest{
				Tx: parentB, Label: "parent-b",
			})
			require.NoError(t, err,
				"Evict must free the fee UTXO for other "+
					"parents")
		})

	t.Run("same parent re-picking own UTXO is allowed",
		func(t *testing.T) {
			chain := newFakeChainSourceRef(100)
			chain.feeRate = 5
			utxo := makeWalletUTXO()
			b := NewCPFPBroadcaster(BroadcasterConfig{
				ChainSource: chain,
				Wallet: &fakeWallet{
					utxos: []*wallet.Utxo{utxo},
				},
			})

			parent := makeTestTx(true)
			result1, err := b.Submit(t.Context(), 100,
				&BroadcastRequest{Tx: parent, Label: "bump-1"},
			)
			require.NoError(t, err)

			// Second submission for the SAME parent with no
			// other UTXOs available must succeed; per-parent
			// re-picking is how TRUC package RBF triggers
			// replacement via double-spending the fee input.
			result2, err := b.Submit(t.Context(), 101,
				&BroadcastRequest{Tx: parent, Label: "bump-2"},
			)
			require.NoError(t, err,
				"a parent must be allowed to re-pick a UTXO "+
					"from its own reserved set")
			require.Greater(t, result2.FeeRate, result1.FeeRate)
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
		largeUTXO := &wallet.Utxo{
			Outpoint: wire.OutPoint{
				Hash:  chainhash.Hash{2},
				Index: 1,
			},
			Amount:   5_000_000,
			PkScript: []byte{txscript.OP_TRUE},
		}

		return NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
			Wallet: &fakeWallet{
				utxos: []*wallet.Utxo{largeUTXO},
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
		require.Greater(t, second.FeeRate, first.FeeRate,
			"replacement feerate must strictly exceed prior "+
				"feerate (BIP-125 Rule 4)")

		prev := b.parentStates[txid].LastPackageFee
		require.Greater(t, int64(prev), int64(0))

		third, err := b.Submit(t.Context(), 102, &BroadcastRequest{
			Tx: parent, Label: "bump",
		})
		require.NoError(t, err)
		require.Greater(t, third.FeeRate, second.FeeRate)
		require.Greater(t, int64(b.parentStates[txid].LastPackageFee),
			int64(prev),
			"replacement absolute fee must strictly exceed prior "+
				"absolute fee (BIP-125 Rule 3)")
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
		require.Greater(t, second.FeeRate, first.FeeRate,
			"replacement feerate must strictly exceed prior "+
				"feerate even when the estimator dips")
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

		b.Evict(txid)
		require.Nil(t, b.parentStates[txid],
			"Evict must release the per-parent bump state")

		// Follow-up submission starts from the raw estimator again.
		next, err := b.Submit(t.Context(), 101, &BroadcastRequest{
			Tx: parent, Label: "bump-after-evict",
		})
		require.NoError(t, err)
		require.Equal(t, int64(5), next.FeeRate,
			"after eviction, feerate should come straight from "+
				"the estimator")
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
			utxos: []*wallet.Utxo{makeWalletUTXO()},
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

		result, err := broadcaster.Submit(t.Context(), 100,
			&BroadcastRequest{Tx: parent, Label: "anchor"},
		)
		require.NoError(t, err)
		require.NotNil(t, result.ChildTxid)
	})

	t.Run("wallet adding extra input fails cleanly", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		extra := &rewritingWallet{
			utxos: []*wallet.Utxo{makeWalletUTXO()},
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
			_, err := broadcaster.Submit(t.Context(), 100,
				&BroadcastRequest{Tx: parent, Label: "anchor"},
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
			Hash:  chainhash.Hash{123},
			Index: 7,
		}
		rename := &rewritingWallet{
			utxos: []*wallet.Utxo{makeWalletUTXO()},
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
			_, err := broadcaster.Submit(t.Context(), 100,
				&BroadcastRequest{Tx: parent, Label: "anchor"},
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
