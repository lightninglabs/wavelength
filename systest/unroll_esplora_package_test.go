//go:build systest

package systest

import (
	"bytes"
	"context"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo-client/chainbackends/esplorarpc"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	"github.com/lightninglabs/darepo-client/lndbackend"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo-client/walletcore"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// fundedUTXOAmount is the value funded into each LND wallet UTXO used by the
// package-relay test. It is comfortably larger than any plausible CPFP child
// fee so the broadcaster's fee-input selection always succeeds.
const fundedUTXOAmount = btcutil.Amount(1_000_000)

// lndPackageWallet adapts the harness LND services to txconfirm.Wallet. It
// mirrors the production lndUnrollWallet (darepod/server.go): UTXO
// enumeration and leasing come from the boarding backend, fresh change
// scripts and PSBT finalization from the WalletKit.
type lndPackageWallet struct {
	*lndbackend.BoardingBackend
}

// NewWalletPkScript returns a fresh wallet-managed taproot pkScript.
func (w *lndPackageWallet) NewWalletPkScript(ctx context.Context) ([]byte,
	error) {

	addr, err := w.WalletKit().NextAddr(
		ctx, lnwallet.DefaultAccountName,
		walletrpc.AddressType_TAPROOT_PUBKEY, true,
	)
	if err != nil {
		return nil, err
	}

	return txscript.PayToAddrScript(addr)
}

// FinalizePsbt signs and finalizes a PSBT via LND's WalletKit.
func (w *lndPackageWallet) FinalizePsbt(ctx context.Context,
	packetBytes []byte) (*wire.MsgTx, error) {

	packet, err := psbt.NewFromRawBytes(bytes.NewReader(packetBytes), false)
	if err != nil {
		return nil, err
	}

	_, finalTx, err := w.WalletKit().FinalizePsbt(ctx, packet, "")

	return finalTx, err
}

// TestEsploraPackageRelayUnrollBroadcast proves that the Esplora-backed
// package submitter can broadcast the kind of transaction a unilateral exit
// produces — a zero-fee v3 (TRUC) parent carrying an ephemeral anchor — by
// driving the real txconfirm.CPFPBroadcaster against the harness's electrs
// `/txs/package` endpoint. This is the end-to-end confidence check for using
// the Esplora relay path for unroll on the lnd / neutrino backends, which
// cannot relay such packages on their own (darepo-client#590).
//
// The parent pays zero fee (its outputs sum to its single input), so it can
// only confirm via a CPFP child carried in a package. If the package confirms,
// Esplora package relay worked.
func TestEsploraPackageRelayUnrollBroadcast(t *testing.T) {
	h := NewSysTestHarness(t)
	ctx := h.Context()

	// Fund two confirmed LND UTXOs: one to build the zero-fee anchor
	// parent, one for the broadcaster to select as the CPFP child fee
	// input.
	h.Harness.FundOperatorLND(fundedUTXOAmount)
	h.Harness.FundOperatorLND(fundedUTXOAmount)
	h.WaitForLNDSync()

	lnd := h.Harness.LND
	wlt := &lndPackageWallet{
		BoardingBackend: lndbackend.NewBoardingBackend(
			lnd.WalletKit, lnd.ChainKit,
		),
	}

	// Build the LND chain backend with the Esplora package submitter wired
	// in, exactly as cmd/darepod does via package.esploraurl.
	backendCfg := chainbackends.LNDBackendFromLndClientConfig{LND: lnd}
	backend := chainbackends.NewLNDBackendFromLndClient(
		backendCfg.WithLogger(
			h.SubLogger(chainbackends.LndClientSubsystem),
		),
	)
	submitter, err := esplorarpc.New(
		h.Harness.EsploraURL,
		esplorarpc.WithLog(
			h.SubLogger("ESPL"),
		),
	)
	require.NoError(t, err)
	backend.SetPackageSubmitter(submitter)

	require.NoError(t, backend.Start())
	t.Cleanup(func() { _ = backend.Stop() })

	chainActor := chainsource.NewChainSourceActor(
		chainsource.ChainSourceConfig{
			Backend: backend,
			System:  h.ActorSystem(),
		}.WithLogger(h.SubLogger(chainsource.Subsystem)),
	)
	chainRef := actor.RegisterWithSystem(
		h.ActorSystem(),
		"chain-source", actor.NewServiceKey[
			chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
		](
			"chain-source",
		),
		chainActor,
	)

	// Pick the parent-input UTXO and lease it so the broadcaster's
	// fee-input selection picks the other UTXO rather than double-spending
	// the parent's input.
	utxos, err := wlt.ListUnspent(ctx, 1, math.MaxInt32)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(utxos), 2, "need >=2 confirmed UTXOs")

	parentInput := utxos[0]

	var lockID walletcore.LockID
	copy(lockID[:], "systest-esplora-package-relay")
	_, err = wlt.LeaseOutput(ctx, lockID, parentInput.Outpoint, time.Hour)
	require.NoError(t, err)

	// Build a zero-fee v3 parent with an ephemeral anchor: the exact shape
	// of an unroll proof/sweep parent. The single non-anchor output equals
	// the input value, so the parent pays zero fee and cannot relay alone.
	changeScript, err := wlt.NewWalletPkScript(ctx)
	require.NoError(t, err)

	parent := wire.NewMsgTx(arktx.TxVersion)
	parent.AddTxIn(&wire.TxIn{
		PreviousOutPoint: parentInput.Outpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})
	parent.AddTxOut(&wire.TxOut{
		Value:    int64(parentInput.Amount),
		PkScript: changeScript,
	})
	parent.AddTxOut(arkscript.AnchorOutput())

	signedParent := signWalletInput(ctx, t, wlt, parent, parentInput)
	parentTxid := signedParent.TxHash()

	// Submit via the real CPFP broadcaster, which builds and signs the
	// fee-paying child and submits the parent+child package through the
	// chain source → LNDBackend.SubmitPackage → Esplora /txs/package.
	broadcaster := txconfirm.NewCPFPBroadcaster(txconfirm.BroadcasterConfig{
		ChainSource: chainRef,
		Wallet:      wlt,
		Log:         fn.Some(h.SubLogger("TXCF")),
	})

	result, err := broadcaster.Submit(ctx, 0, &txconfirm.BroadcastRequest{
		Tx:    signedParent,
		Label: "systest-esplora-unroll",
	})
	require.NoError(t, err, "Esplora package submission failed")
	require.NotNil(t, result.ChildTxid, "expected a CPFP child")

	// Both parent and child must reach bitcoind's mempool, proving electrs
	// accepted the package and relayed it, then confirm in the next block.
	h.Harness.WaitMempoolTx(parentTxid.String())
	h.Harness.WaitMempoolTx(result.ChildTxid.String())
	h.Harness.Generate(1)

	require.Empty(
		t, h.Harness.MempoolTxIDs(),
		"package should have confirmed and drained the mempool",
	)
}

// signWalletInput finalizes the single wallet input of tx via the wallet's
// PSBT signer, returning the fully signed transaction.
func signWalletInput(ctx context.Context, t *testing.T, w *lndPackageWallet,
	tx *wire.MsgTx, in *walletcore.Utxo) *wire.MsgTx {

	t.Helper()

	op := tx.TxIn[0].PreviousOutPoint
	packet, err := psbt.New(
		[]*wire.OutPoint{&op}, tx.TxOut, tx.Version, tx.LockTime,
		[]uint32{tx.TxIn[0].Sequence},
	)
	require.NoError(t, err)

	packet.Inputs[0].WitnessUtxo = &wire.TxOut{
		Value:    int64(in.Amount),
		PkScript: in.PkScript,
	}

	// FundOperatorLND hands out P2WPKH (SegWit v0) outputs, so the witness
	// signature must carry an explicit SIGHASH_ALL byte. Without setting
	// the PSBT sighash type, LND finalizes a witness bitcoind rejects with
	// "Signature hash type missing or not understood". The production CPFP
	// path sets it the same way via cpfpFeeInputSighash.
	packet.Inputs[0].SighashType = txscript.SigHashAll

	var buf bytes.Buffer
	require.NoError(t, packet.Serialize(&buf))

	signed, err := w.FinalizePsbt(ctx, buf.Bytes())
	require.NoError(t, err)

	return signed
}
