package darepo

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	clientlnd "github.com/lightninglabs/darepo-client/lndbackend"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/fraud"
	"github.com/lightninglabs/darepo/oor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// setupFraudResponder wires the server fraud responder to txconfirm.
func (s *Server) setupFraudResponder(vtxoStore *db.VTXOStoreDB,
	sessionStore *oor.DBSessionStore,
	operatorKey keychain.KeyDescriptor,
	checkpointPolicy arkscript.CheckpointPolicy) (
	actor.TellOnlyRef[batchwatcher.FraudDetectorMsg], error) {

	feeWallet := &serverTxConfirmWallet{
		boardingBackend: clientlnd.NewBoardingBackend(
			s.lnd.WalletKit, s.lnd.ChainKit,
		),
	}

	txConfirm := txconfirm.NewTxBroadcasterActor(txconfirm.Config{
		ChainSource: s.chainSourceRef,
		Wallet:      feeWallet,
		Log: fn.Some(subLogger(
			s.cfg.Loggers, txConfirmSubsystem,
		)),
		MaxFeeRateSatPerVByte: s.cfg.Fraud.MaxResponseFeeRate(),
	})
	txConfirmKey := actor.NewServiceKey[
		txconfirm.Msg, txconfirm.Resp,
	]("server-txconfirm")
	txConfirmRef := actor.RegisterWithSystem(
		s.actorSystem, "server-txconfirm", txConfirmKey, txConfirm,
	)
	txConfirm.SetSelfRef(txConfirmRef)

	checkpointSweepStore := newFraudCheckpointSweepStore(sessionStore)
	responder, err := fraud.NewActor(fraud.Config{
		TxConfirmRef: txConfirmRef,
		Planner:      fraud.DefaultPlanner{},
		CheckpointPlanner: &fraud.CheckpointPlanner{
			VTXOStore: newBatchWatcherSpendRecoveryStore(
				vtxoStore,
			),
			CheckpointLookup: newBatchWatcherCheckpointLookup(
				sessionStore,
			),
			CheckpointSweepStore: checkpointSweepStore,
			CheckpointPolicy:     checkpointPolicy,
		},
		CheckpointSweepStore: checkpointSweepStore,
		CheckpointPolicy:     checkpointPolicy,
		OperatorKey:          operatorKey,
		Signer:               s.walletController,
		NewSweepPkScript:     feeWallet.NewWalletPkScript,
		Log: fn.Some(subLogger(
			s.cfg.Loggers, fraud.Subsystem,
		)),
	})
	if err != nil {
		return nil, fmt.Errorf("create fraud actor: %w", err)
	}
	responderKey := actor.NewServiceKey[
		actor.Message, actor.Message,
	](fraud.ServiceKeyName)
	responderRef := actor.RegisterWithSystem(
		s.actorSystem, fraud.ServiceKeyName, responderKey, responder,
	)

	txNotifyRef := actor.NewMapInputRef[
		txconfirm.Notification, actor.Message,
	](responderRef, func(msg txconfirm.Notification) actor.Message {
		return msg
	})
	responder.SetNotificationRef(txNotifyRef)

	fraudRef := actor.NewMapInputRef[
		batchwatcher.FraudDetectorMsg, actor.Message,
	](responderRef, func(msg batchwatcher.FraudDetectorMsg) actor.Message {
		return msg
	})

	return fraudRef, nil
}

type fraudCheckpointSweepStore struct {
	store interface {
		LoadCheckpointSweepInfoByInput(context.Context,
			wire.OutPoint) (*oor.CheckpointSweepInfo, bool, error)
	}
}

func newFraudCheckpointSweepStore(store interface {
	LoadCheckpointSweepInfoByInput(context.Context,
		wire.OutPoint) (*oor.CheckpointSweepInfo, bool, error)
}) fraud.CheckpointSweepStore {

	return &fraudCheckpointSweepStore{store: store}
}

// LoadCheckpointSweepInfoByInput adapts OOR checkpoint sweep metadata to the
// fraud package's narrow projection.
func (s *fraudCheckpointSweepStore) LoadCheckpointSweepInfoByInput(
	ctx context.Context, input wire.OutPoint) (*fraud.CheckpointSweepInfo,
	bool, error) {

	info, found, err := s.store.LoadCheckpointSweepInfoByInput(ctx, input)
	if err != nil {
		return nil, false, fmt.Errorf(
			"load checkpoint sweep info: %w", err,
		)
	}
	if !found {
		return nil, false, nil
	}

	return &fraud.CheckpointSweepInfo{
		InputOutpoint:         info.InputOutpoint,
		CheckpointTx:          info.CheckpointTx,
		CheckpointOutputIndex: info.CheckpointOutputIndex,
		CheckpointOutput:      info.CheckpointOutput,
		TapTreeEncoded:        info.TapTreeEncoded,
	}, true, nil
}

// serverTxConfirmWallet adapts the operator LND wallet to txconfirm.Wallet.
type serverTxConfirmWallet struct {
	boardingBackend *clientlnd.BoardingBackend
}

// ListUnspent delegates to the operator wallet's UTXO set.
func (w *serverTxConfirmWallet) ListUnspent(ctx context.Context,
	minConfs, maxConfs int32) ([]*wallet.Utxo, error) {

	return w.boardingBackend.ListUnspent(ctx, minConfs, maxConfs)
}

// NewWalletPkScript returns a fresh wallet-managed taproot pkScript.
func (w *serverTxConfirmWallet) NewWalletPkScript(ctx context.Context) (
	[]byte, error) {

	addr, err := w.boardingBackend.WalletKit().NextAddr(
		ctx, lnwallet.DefaultAccountName,
		walletrpc.AddressType_TAPROOT_PUBKEY, true,
	)
	if err != nil {
		return nil, fmt.Errorf("LND NextAddr: %w", err)
	}

	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, fmt.Errorf("pay to addr script: %w", err)
	}

	return pkScript, nil
}

// FinalizePsbt signs and finalizes a PSBT with the operator wallet.
func (w *serverTxConfirmWallet) FinalizePsbt(ctx context.Context,
	packetBytes []byte) (*wire.MsgTx, error) {

	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(packetBytes), false,
	)
	if err != nil {
		return nil, fmt.Errorf("parse PSBT: %w", err)
	}

	_, finalTx, err := w.boardingBackend.WalletKit().FinalizePsbt(
		ctx, packet, "",
	)
	if err != nil {
		return nil, fmt.Errorf("LND FinalizePsbt: %w", err)
	}

	return finalTx, nil
}

// LeaseOutput forwards CPFP fee-input leases to LND.
func (w *serverTxConfirmWallet) LeaseOutput(ctx context.Context,
	id wallet.LockID, op wire.OutPoint, expiry time.Duration) (
	time.Time, error) {

	return w.boardingBackend.WalletKit().LeaseOutput(
		ctx, wtxmgr.LockID(id), op, expiry,
	)
}

// ReleaseOutput releases a CPFP fee-input lease in LND.
func (w *serverTxConfirmWallet) ReleaseOutput(ctx context.Context,
	id wallet.LockID, op wire.OutPoint) error {

	return w.boardingBackend.WalletKit().ReleaseOutput(
		ctx, wtxmgr.LockID(id), op,
	)
}

var _ txconfirm.Wallet = (*serverTxConfirmWallet)(nil)
