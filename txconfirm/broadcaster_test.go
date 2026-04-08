package txconfirm

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
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
	msg chainsource.ChainSourceMsg) actor.Future[chainsource.ChainSourceResp] {

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
	fsm.Start(context.Background())
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
		require.Equal(t, "EnsureConfirmedResp", ensureResp.MessageType())

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
		require.Equal(t, "confirmationObservedMsg", confMsg.MessageType())

		blockMsg := &blockEpochObservedMsg{}
		blockMsg.txConfirmMsgSealed()
		require.Equal(t, "blockEpochObservedMsg", blockMsg.MessageType())
	})

	t.Run("notification mapping", func(t *testing.T) {
		target := actor.NewChannelTellOnlyRef[testMappedMsg]("mapped", 1)
		mapped := MapNotification(
			target,
			func(msg Notification) testMappedMsg {
				return testMappedMsg{payload: msg.MessageType()}
			},
		)

		err := mapped.Tell(context.Background(), &TxConfirmed{})
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
				Outpoint: wire.OutPoint{Hash: chainhash.Hash{1}, Index: 1},
				Output: &wire.TxOut{
					Value: 1000,
				},
				Confirmed: true,
			},
			{
				Outpoint: wire.OutPoint{Hash: chainhash.Hash{2}, Index: 2},
				Output: &wire.TxOut{
					Value: 5000,
				},
				Confirmed: true,
			},
		}

		selected, err := SelectFeeInput(feeInputs, 2000, nil)
		require.NoError(t, err)
		require.Equal(t, int64(5000), selected.Output.Value)

		selected, err = SelectFeeInput(feeInputs, 500, map[wire.OutPoint]bool{
			feeInputs[0].Outpoint: true,
		})
		require.NoError(t, err)
		require.Equal(t, feeInputs[1].Outpoint, selected.Outpoint)

		_, err = SelectFeeInput(feeInputs, 10_000, nil)
		require.Error(t, err)
	})

	t.Run("ignorable errors", func(t *testing.T) {
		require.True(t, IsIgnorableBroadcastError(
			fmt.Errorf("already known"),
		))
		require.False(t, IsIgnorableBroadcastError(fmt.Errorf("fatal")))
		require.True(t, isPackageSubmissionUnsupported(
			fmt.Errorf("package relay not supported"),
		))
		require.False(t, isPackageSubmissionUnsupported(fmt.Errorf("fatal")))
	})
}

// TestCPFPBroadcasterFallbackAndErrors covers the lower-level generic
// broadcaster's fallback and error branches.
func TestCPFPBroadcasterFallbackAndErrors(t *testing.T) {
	t.Run("unsupported package falls back to individual broadcast", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		chain.packageErr = fmt.Errorf("package relay not supported")
		broadcaster := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
			Wallet: &fakeWallet{
				utxos: []*wallet.Utxo{makeWalletUTXO()},
			},
		})

		result, err := broadcaster.Submit(context.Background(), 100,
			&BroadcastRequest{
				Tx:    makeTestTx(true),
				Label: "anchor",
			},
		)
		require.NoError(t, err)
		require.NotNil(t, result.ChildTxid)
		require.Len(t, chain.broadcastCalls, 2)
	})

	t.Run("submit validation and fee estimate errors", func(t *testing.T) {
		broadcaster := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: newFakeChainSourceRef(100),
		})

		_, err := broadcaster.Submit(context.Background(), 100, nil)
		require.Error(t, err)

		badResp := &staticChainSourceRef{
			handler: func(_ context.Context,
				msg chainsource.ChainSourceMsg,
			) (chainsource.ChainSourceResp, error) {
				switch msg.(type) {
				case *chainsource.FeeEstimateRequest:
					return &chainsource.BestHeightResponse{}, nil
				default:
					return &chainsource.BestHeightResponse{}, nil
				}
			},
		}
		broadcaster = NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: badResp,
		})

		_, err = broadcaster.EstimateFeeRate(context.Background())
		require.Error(t, err)
	})

	t.Run("wallet error branches", func(t *testing.T) {
		chain := newFakeChainSourceRef(100)
		tx := makeTestTx(true)

		broadcaster := NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
		})
		_, err := broadcaster.selectFeeInput(context.Background(), 100)
		require.Error(t, err)

		broadcaster = NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
			Wallet: &failingWallet{
				listErr: fmt.Errorf("list failed"),
			},
		})
		_, err = broadcaster.selectFeeInput(context.Background(), 100)
		require.Error(t, err)

		broadcaster = NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
			Wallet: &failingWallet{
				utxos: []*wallet.Utxo{makeWalletUTXO()},
			},
		})
		_, err = broadcaster.deriveChangePkScript(context.Background())
		require.Error(t, err)

		broadcaster = NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource: chain,
			Wallet: &failingWallet{
				utxos:        []*wallet.Utxo{makeWalletUTXO()},
				changeScript: []byte{txscript.OP_TRUE},
				finalizeErr:  fmt.Errorf("finalize failed"),
			},
		})
		_, err = broadcaster.broadcastWithCPFP(
			context.Background(), 100, &BroadcastRequest{
				Tx: tx,
			}, tx.TxHash(), 1,
		)
		require.Error(t, err)
	})
}

// TestActorValidationAndCleanup covers actor validation, cleanup, and direct
// branch behavior that the higher-level flow tests do not hit.
func TestActorValidationAndCleanup(t *testing.T) {
	t.Run("receive validation branches", func(t *testing.T) {
		behavior := NewTxBroadcasterActor(Config{
			ChainSource: newFakeChainSourceRef(100),
		})

		_, err := behavior.handleEnsure(context.Background(), nil)
		require.Error(t, err)

		_, err = behavior.handleEnsure(context.Background(), &EnsureConfirmedReq{})
		require.Error(t, err)

		_, err = behavior.handleCancel(context.Background(), nil)
		require.Error(t, err)

		res := behavior.Receive(context.Background(), &testUnknownMsg{})
		_, err = res.Unpack()
		require.Error(t, err)
	})

	t.Run("ensure best height unexpected response", func(t *testing.T) {
		behavior := NewTxBroadcasterActor(Config{
			ChainSource: &staticChainSourceRef{
				handler: func(_ context.Context,
					msg chainsource.ChainSourceMsg,
				) (chainsource.ChainSourceResp, error) {
					if _, ok := msg.(*chainsource.BestHeightRequest); ok {
						return &chainsource.BroadcastTxResponse{}, nil
					}

					return &chainsource.SubscribeBlocksResponse{}, nil
				},
			},
		})

		err := behavior.ensureBestHeight(context.Background())
		require.Error(t, err)
	})

	t.Run("ensure block subscription error", func(t *testing.T) {
		behavior := NewTxBroadcasterActor(Config{
			ChainSource: &staticChainSourceRef{
				handler: func(_ context.Context,
					msg chainsource.ChainSourceMsg,
				) (chainsource.ChainSourceResp, error) {
					if _, ok := msg.(*chainsource.SubscribeBlocksRequest); ok {
						return nil, fmt.Errorf("subscribe failed")
					}

					return &chainsource.BestHeightResponse{}, nil
				},
			},
		})
		behavior.SetSelfRef(actor.NewChannelTellOnlyRef[Msg]("self", 1))

		err := behavior.ensureBlockSubscription(context.Background())
		require.Error(t, err)
	})

	t.Run("should fee bump helper", func(t *testing.T) {
		behavior := NewTxBroadcasterActor(Config{
			ChainSource: newFakeChainSourceRef(100),
		})
		behavior.bestHeight = 100
		entry := newTrackedTxForState(t, &trackedTxStateAwaitingConfirmation{
			trackedTxData: trackedTxData{
				Txid: chainhash.Hash{7},
			},
			trackedTxProgress: trackedTxProgress{
				LastBroadcastHeight: 99,
			},
		})
		require.False(t, behavior.shouldFeeBump(entry))

		entry = newTrackedTxForState(t, &trackedTxStateAwaitingConfirmation{
			trackedTxData: trackedTxData{
				Txid: chainhash.Hash{7},
			},
			trackedTxProgress: trackedTxProgress{
				LastBroadcastHeight: 98,
			},
		})
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
		entry := newTrackedTxForState(t, &trackedTxStateAwaitingConfirmation{
			trackedTxData: trackedTxData{
				Txid:        chainhash.Hash{9},
				TargetConfs: 1,
			},
			trackedTxProgress: trackedTxProgress{
				LastBroadcastHeight: 99,
			},
		})
		entry.subscribers["fail"] = &failingNotifyRef{}
		behavior.tracked[entry.data.Txid] = entry

		behavior.notifyOneConfirmed(
			context.Background(), &failingNotifyRef{},
			entry.data.Txid, 1, 1,
		)
		behavior.notifyOneFailed(
			context.Background(), &failingNotifyRef{}, entry.data.Txid,
			"failed",
		)

		err := behavior.OnStop(context.Background())
		require.NoError(t, err)
		require.Len(t, chain.unsubscribeBlocks, 1)
		require.Len(t, chain.unregisterConfs, 1)
	})
}
