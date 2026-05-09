package darepo

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	clientlnd "github.com/lightninglabs/darepo-client/lndbackend"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/fraud"
	"github.com/lightninglabs/darepo/oor"
	"github.com/lightninglabs/darepo/rounds"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// setupFraudResponder wires the server fraud responder to txconfirm.
func (s *Server) setupFraudResponder(roundStore *db.RoundStoreDB,
	vtxoStore *db.VTXOStoreDB, sessionStore *oor.DBSessionStore,
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
			ForfeitLookup: &serverForfeitPlanner{
				roundStore:  roundStore,
				vtxoStore:   vtxoStore,
				operatorKey: operatorKey,
				signer:      s.walletController,
			},
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

// serverForfeitPlanner builds forfeit response packages from persisted round
// metadata. The connector tree's branching factor is read from the
// descriptor stamped at finalization (rounds.ConnectorTreeDescriptor.Radix)
// rather than the runtime config so an operator who rotates TreeRadix can
// still respond to fraud against in-flight forfeited VTXOs from rounds
// built under the previous radix.
type serverForfeitPlanner struct {
	roundStore  *db.RoundStoreDB
	vtxoStore   *db.VTXOStoreDB
	operatorKey keychain.KeyDescriptor
	signer      rounds.WalletController
}

// PlanForfeit returns the connector ancestors plus stored forfeit tx needed
// to claim a forfeited VTXO that has appeared on-chain.
func (p *serverForfeitPlanner) PlanForfeit(ctx context.Context,
	outpoint wire.OutPoint) (*fraud.ResponsePlan, error) {

	if p == nil {
		return nil, fmt.Errorf("forfeit planner is nil")
	}
	if p.roundStore == nil {
		return nil, fmt.Errorf("round store is nil")
	}
	if p.vtxoStore == nil {
		return nil, fmt.Errorf("vtxo store is nil")
	}
	if p.signer == nil {
		return nil, fmt.Errorf("signer is nil")
	}
	if p.operatorKey.PubKey == nil {
		return nil, fmt.Errorf("operator key is nil")
	}

	info, err := p.vtxoStore.GetForfeitInfo(ctx, outpoint)
	if err != nil {
		return nil, fmt.Errorf("load forfeit info: %w", err)
	}
	if info == nil || info.ForfeitTx == nil {
		return nil, fmt.Errorf("forfeit tx missing for %s", outpoint)
	}

	// Bind the persisted forfeit tx to the operator's BIP86 forfeit
	// script before we sign and broadcast the connector path. The sweep
	// path runs the same check after confirmation, but by then we have
	// already moved the on-chain VTXO value via the forfeit broadcast —
	// catching a tampered or malformed penalty pkScript here keeps the
	// failure local rather than producing an unsweepable confirmed
	// output.
	expectedScript, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootKeyNoScript(p.operatorKey.PubKey),
	)
	if err != nil {
		return nil, fmt.Errorf("derive operator forfeit script: %w",
			err)
	}
	if len(info.ForfeitTx.TxOut) < 1 ||
		info.ForfeitTx.TxOut[0] == nil ||
		!bytes.Equal(info.ForfeitTx.TxOut[0].PkScript, expectedScript) {

		return nil, fmt.Errorf(
			"forfeit penalty pkScript does not match operator "+
				"BIP86 for %s", outpoint,
		)
	}

	round, err := p.roundStore.GetConfirmedRound(ctx, info.RoundID)
	if err != nil {
		return nil, fmt.Errorf("load forfeit round %s: %w",
			info.RoundID, err)
	}

	ancestors, err := p.connectorAncestors(round, info)
	if err != nil {
		return nil, err
	}

	return &fraud.ResponsePlan{
		Ancestors:  ancestors,
		ResponseTx: info.ForfeitTx,
		Label:      fraud.ForfeitLabel,
	}, nil
}

// connectorAncestors reconstructs and signs the connector path consumed by
// the stored forfeit transaction.
func (p *serverForfeitPlanner) connectorAncestors(round *rounds.Round,
	info *rounds.ForfeitInfo) ([]*wire.MsgTx, error) {

	if round == nil || round.FinalTx == nil {
		return nil, fmt.Errorf("forfeit round is missing final tx")
	}

	var descriptor *rounds.ConnectorTreeDescriptor
	for _, desc := range round.ConnectorDescriptors {
		if desc.OutputIndex != info.ConnectorOutputIndex {
			continue
		}

		descriptor = desc

		break
	}
	if descriptor == nil {
		return nil, fmt.Errorf("connector descriptor %d missing",
			info.ConnectorOutputIndex)
	}

	if descriptor.Radix < 2 {
		return nil, fmt.Errorf(
			"connector descriptor %d has invalid radix %d",
			info.ConnectorOutputIndex, descriptor.Radix,
		)
	}
	connectorTree, err := rounds.BuildConnectorTreeFromDescriptor(
		round.FinalTx, descriptor, p.operatorKey.PubKey,
		descriptor.Radix,
	)
	if err != nil {
		return nil, fmt.Errorf("build connector tree: %w", err)
	}

	path, err := connectorTree.ExtractPathForIndices(info.LeafIndex)
	if err != nil {
		return nil, fmt.Errorf("extract connector path: %w", err)
	}

	prevOutFetcher, err := path.Root.PrevOutputFetcher(path.BatchOutput)
	if err != nil {
		return nil, fmt.Errorf("connector prevout fetcher: %w", err)
	}

	ancestors := make([]*wire.MsgTx, 0, path.NumTx())
	err = path.Root.ForEach(func(node *tree.Node) error {
		tx, err := node.ToTx()
		if err != nil {
			return err
		}

		prevOut := prevOutFetcher.FetchPrevOutput(
			tx.TxIn[0].PreviousOutPoint,
		)
		if prevOut == nil {
			return fmt.Errorf("missing connector prevout %s",
				tx.TxIn[0].PreviousOutPoint)
		}

		// Connector leaves commit to a P2TR keyspend by the
		// operator key (no MuSig2 aggregation, no script paths), so
		// a BIP86 keyspend with the raw operator key validates
		// against the parent prevout. End-to-end coverage:
		// TestFraudResponseForfeitedVTXO.
		sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)
		signMethod := input.TaprootKeySpendBIP0086SignMethod
		signDesc := &input.SignDescriptor{
			KeyDesc:           p.operatorKey,
			Output:            prevOut,
			HashType:          txscript.SigHashDefault,
			InputIndex:        0,
			SignMethod:        signMethod,
			SigHashes:         sigHashes,
			PrevOutputFetcher: prevOutFetcher,
			TapTweak:          []byte{},
		}

		sig, err := p.signer.SignOutputRaw(tx, signDesc)
		if err != nil {
			return fmt.Errorf("sign connector tx %s: %w",
				tx.TxHash(), err)
		}
		schnorrSig, ok := sig.(*schnorr.Signature)
		if !ok {
			return fmt.Errorf("connector tx %s signature type %T",
				tx.TxHash(), sig)
		}

		node.AddSignature(schnorrSig)

		signedTx, err := node.ToSignedTx()
		if err != nil {
			return err
		}

		if err := verifyConnectorAncestor(
			signedTx, prevOut, prevOutFetcher,
		); err != nil {
			return fmt.Errorf("verify connector tx %s: %w",
				tx.TxHash(), err)
		}

		ancestors = append(ancestors, signedTx)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("serialize connector ancestors: %w", err)
	}

	leaf, err := connectorLeafOutpoint(path.Root)
	if err != nil {
		return nil, err
	}
	if info.ForfeitTx.TxIn[1].PreviousOutPoint != leaf {
		return nil, fmt.Errorf(
			"forfeit connector input spends %s, want %s",
			info.ForfeitTx.TxIn[1].PreviousOutPoint, leaf,
		)
	}

	return ancestors, nil
}

// connectorLeafOutpoint returns the single connector leaf outpoint in an
// extracted connector path.
func connectorLeafOutpoint(root *tree.Node) (wire.OutPoint, error) {
	var leafOutpoint *wire.OutPoint
	err := root.ForEachLeaf(func(node *tree.Node) error {
		outpoint, err := node.GetNonAnchorOutpoint()
		if err != nil {
			return err
		}
		if leafOutpoint != nil {
			return fmt.Errorf("connector path has multiple leaves")
		}

		leafOutpoint = outpoint

		return nil
	})
	if err != nil {
		return wire.OutPoint{}, err
	}
	if leafOutpoint == nil {
		return wire.OutPoint{}, fmt.Errorf("connector path has no leaf")
	}

	return *leafOutpoint, nil
}

// verifyConnectorAncestor validates a signed connector ancestor before it is
// handed to txconfirm.
func verifyConnectorAncestor(tx *wire.MsgTx, prevOut *wire.TxOut,
	prevOutFetcher txscript.PrevOutputFetcher) error {

	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)
	engine, err := txscript.NewEngine(
		prevOut.PkScript, tx, 0, txscript.StandardVerifyFlags, nil,
		sigHashes, prevOut.Value, prevOutFetcher,
	)
	if err != nil {
		return fmt.Errorf("create script engine: %w", err)
	}

	return engine.Execute()
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
