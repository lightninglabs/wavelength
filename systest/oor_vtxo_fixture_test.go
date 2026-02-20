//go:build systest

package systest

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	clientoor "github.com/lightninglabs/darepo-client/oor"
	clientvtxo "github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// oorRealChainVTXO binds a v0 VTXO descriptor to a real-chain UTXO that was
// minted by a parent transaction.
//
// Real VTXOs are "virtual" and don't exist directly on-chain. The OOR system
// tests approximate the virtual-tx semantics by first creating a parent
// transaction that produces a standard v0 VTXO output, then spending that
// output in the checkpoint transaction.
type oorRealChainVTXO struct {
	// ParentTx is the tx that creates the VTXO output.
	ParentTx *wire.MsgTx

	// Outpoint is the real-chain UTXO outpoint that the test
	// treats as the VTXO being spent by the checkpoint
	// transaction.
	Outpoint wire.OutPoint

	// PrevOut is the previous output data for Outpoint.
	PrevOut *wire.TxOut

	// FundingTxid is the txid of the faucet transaction that
	// created the input outpoint spent by ParentTx.
	FundingTxid chainhash.Hash

	// VTXO is the client-side VTXO descriptor used for local persistence in
	// systests.
	VTXO *clientvtxo.Descriptor

	// OwnerLeafScript is the checkpoint owner leaf used for this VTXO.
	OwnerLeafScript []byte
}

// TransferInput returns the client OOR TransferInput for this minted VTXO.
func (v *oorRealChainVTXO) TransferInput() clientoor.TransferInput {
	return clientoor.TransferInput{
		VTXO:            v.VTXO,
		OwnerLeafScript: v.OwnerLeafScript,
	}
}

// oorMintRealVTXO creates a real regtest UTXO that is a valid v0 VTXO output,
// but is produced by a parent transaction instead of being funded directly.
func oorMintRealVTXO(t *testing.T, h *E2EHarness, operatorSigner input.Signer,
	operatorKey keychain.KeyDescriptor, ownerKey keychain.KeyDescriptor,
	exitDelay uint32, amount btcutil.Amount) *oorRealChainVTXO {

	t.Helper()

	require.NotNil(t, h)
	require.NotNil(t, operatorSigner)
	require.NotNil(t, operatorKey.PubKey)
	require.NotNil(t, ownerKey.PubKey)

	tapscript, err := scripts.VTXOTapScript(
		ownerKey.PubKey, operatorKey.PubKey, exitDelay,
	)
	require.NoError(t, err)

	tapKey, err := scripts.VTXOTapKey(
		ownerKey.PubKey, operatorKey.PubKey, exitDelay,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(tapKey)
	require.NoError(t, err)

	// Fund a BIP-0086 P2TR output controlled by the operator.
	// We'll spend it to create the VTXO output, ensuring the
	// VTXO outpoint has a real parent tx.
	//
	// The operator signer needs to be told this is a BIP-0086 key-spend by
	// providing an empty TapTweak (non-nil, zero-length slice) in the sign
	// descriptor.
	fundingTapKey := txscript.ComputeTaprootKeyNoScript(operatorKey.PubKey)
	fundingAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(fundingTapKey), oorChainParams,
	)
	require.NoError(t, err)

	fundingValue := amount + btcutil.Amount(oorVTXOParentFeeSat)
	fundingTxidStr := h.Harness.Faucet(fundingAddr.String(), fundingValue)
	h.Harness.Generate(oorConfirmDepth)

	fundingTxid, err := chainhash.NewHashFromStr(fundingTxidStr)
	require.NoError(t, err)

	rpc, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	defer rpc.Shutdown()

	fundingTx, err := rpc.GetRawTransaction(fundingTxid)
	require.NoError(t, err)

	fundingPkScript, err := txscript.PayToAddrScript(fundingAddr)
	require.NoError(t, err)

	outpoint, prevOut, err := oorFindOutpoint(
		fundingTx.MsgTx(), *fundingTxid, fundingPkScript,
	)
	require.NoError(t, err)

	// Create and sign a parent tx that mints a VTXO output.
	// This is a minimal "realized VTXO" fixture: the parent tx
	// is a normal on-chain tx, and the VTXO output script
	// matches the standard v0 VTXO pkScript.
	parent := wire.NewMsgTx(3)
	parent.AddTxIn(&wire.TxIn{
		PreviousOutPoint: outpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})
	parent.AddTxOut(&wire.TxOut{
		Value:    int64(amount),
		PkScript: pkScript,
	})

	require.Greater(t, prevOut.Value-int64(amount), int64(0),
		"funding output too small for VTXO parent fee",
	)

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)
	sigHashes := txscript.NewTxSigHashes(parent, prevFetcher)
	sig, err := operatorSigner.SignOutputRaw(parent, &input.SignDescriptor{
		KeyDesc:           operatorKey,
		Output:            prevOut,
		HashType:          txscript.SigHashDefault,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevFetcher,
		SignMethod:        input.TaprootKeySpendBIP0086SignMethod,
		TapTweak:          []byte{},
		InputIndex:        0,
	})
	require.NoError(t, err)

	parent.TxIn[0].Witness = wire.TxWitness{
		sig.Serialize(),
	}

	parentTxidPtr, err := rpc.SendRawTransaction(parent, false)
	require.NoError(t, err, "broadcast VTXO parent tx")
	require.Equal(t, parent.TxHash(), *parentTxidPtr)

	blocks := h.Harness.GenerateAndWait(1)
	require.NotEmpty(t, blocks)

	found := false
	for _, txid := range blocks[len(blocks)-1].TxIDs {
		if txid == parent.TxHash().String() {
			found = true
			break
		}
	}
	require.True(t, found, "VTXO parent tx not mined")

	vtxoOutpoint := wire.OutPoint{
		Hash:  parent.TxHash(),
		Index: 0,
	}
	vtxoPrevOut := &wire.TxOut{
		Value:    int64(amount),
		PkScript: pkScript,
	}

	desc := &clientvtxo.Descriptor{
		Outpoint: vtxoOutpoint,
		Amount:   amount,
		PkScript: pkScript,
		ClientKey: keychain.KeyDescriptor{
			KeyLocator: ownerKey.KeyLocator,
			PubKey:     ownerKey.PubKey,
		},
		OperatorKey:    operatorKey.PubKey,
		TapScript:      tapscript,
		RelativeExpiry: exitDelay,
		Status:         clientvtxo.VTXOStatusLive,
	}

	// For system tests we use OP_TRUE as the checkpoint owner leaf so
	// later CPFP spends can be constructed without signatures.
	ownerLeaf := []byte{txscript.OP_1}

	return &oorRealChainVTXO{
		ParentTx:        parent,
		Outpoint:        vtxoOutpoint,
		PrevOut:         vtxoPrevOut,
		FundingTxid:     *fundingTxid,
		VTXO:            desc,
		OwnerLeafScript: ownerLeaf,
	}
}
