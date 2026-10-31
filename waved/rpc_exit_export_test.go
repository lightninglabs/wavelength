package waved

import (
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newExitExportFixture persists a signed tree with a real signature so export
// tests exercise database serialization as well as witness verification.
func newExitExportFixture(t *testing.T) (*RPCServer, *vtxo.Descriptor) {
	t.Helper()
	rpc, store := newLeaveAdmissionServer(t)
	rpc.server.walletReady = make(chan struct{})
	close(rpc.server.walletReady)
	rpc.server.chainParams = &chaincfg.RegressionNetParams
	desc := newRefreshEstimateVTXO(t, 1, 50_000, 900)
	key, _ := btcec.PrivKeyFromBytes([]byte{1})
	script, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootKeyNoScript(
			key.PubKey(),
		),
	)
	require.NoError(t, err)
	batch := wire.OutPoint{Hash: chainhash.Hash{1}}
	batchOutput := &wire.TxOut{Value: 50_000, PkScript: script}
	root := &tree.Node{
		Input: batch,
		Outputs: []*wire.TxOut{
			{
				Value:    50_000,
				PkScript: desc.PkScript,
			},
			arkscript.AnchorOutput(),
		},
	}
	tx, err := root.ToTx()
	require.NoError(t, err)
	fetcher := txscript.NewCannedPrevOutputFetcher(script, 50_000)
	sig, err := txscript.RawTxInTaprootSignature(
		tx, txscript.NewTxSigHashes(tx, fetcher), 0, 50_000, script,
		nil, txscript.SigHashDefault, key,
	)
	require.NoError(t, err)
	root.Signature, err = schnorr.ParseSignature(sig)
	require.NoError(t, err)
	desc.Outpoint = wire.OutPoint{Hash: tx.TxHash()}
	desc.CommitmentTxID = batch.Hash
	desc.Ancestry = []vtxo.Ancestry{{
		CommitmentTxID: batch.Hash,
		TreeDepth:      1,
		TreePath: &tree.Tree{
			BatchOutpoint: batch,
			BatchOutput:   batchOutput,
			Root:          root,
		},
	}}
	require.NoError(t, store.SaveVTXO(t.Context(), desc))

	return rpc, desc
}

// TestExportExitWithoutFeeWallet proves external export does not depend on a
// wallet backend, chain backend, operator connection, or unroll registry, and
// leaves the VTXO available for its owner to decide when to exit.
func TestExportExitWithoutFeeWallet(t *testing.T) {
	t.Parallel()
	rpc, desc := newExitExportFixture(t)
	exported, err := rpc.ExportExit(
		t.Context(), &ExitExportRequest{
			Outpoint: desc.Outpoint.String(),
		},
	)
	require.NoError(t, err)
	require.Equal(t, "regtest", exported.Network)
	require.Equal(
		t, chaincfg.RegressionNetParams.GenesisHash.String(),
		exported.GenesisHash,
	)
	require.Equal(
		t, desc.Outpoint, exported.Ancestry.Proof.TargetOutpoint(),
	)
	require.Len(t, exported.Ancestry.ExternalInputs, 1)
	require.Nil(t, exported.FinalSweep)
	stored, err := rpc.server.vtxoStore.GetVTXO(t.Context(), desc.Outpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusLive, stored.Status)

	// Repeating an export is deterministic and has no admission effect.
	repeated, err := rpc.ExportExit(
		t.Context(), &ExitExportRequest{
			Outpoint: desc.Outpoint.String(),
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, exported.Ancestry.Proof.Layers(),
		repeated.Ancestry.Proof.Layers(),
	)
}

// TestExportExitValidatesSweepAddress rejects cross-network payouts and fails
// explicitly when a requested sweep cannot be signed, rather than silently
// exporting an ancestry-only bundle.
func TestExportExitValidatesSweepAddress(t *testing.T) {
	t.Parallel()
	rpc, desc := newExitExportFixture(t)
	for _, network := range []*chaincfg.Params{
		&chaincfg.MainNetParams, &chaincfg.RegressionNetParams,
	} {
		address, err := btcaddr.NewAddressTaproot(
			schnorr.SerializePubKey(desc.ClientKey.PubKey), network,
		)
		require.NoError(t, err)
		exported, err := rpc.ExportExit(t.Context(), &ExitExportRequest{
			Outpoint:     desc.Outpoint.String(),
			SweepAddress: address.EncodeAddress(),
		})
		require.Nil(t, exported)
		wantCode := codes.Unavailable
		if network == &chaincfg.MainNetParams {
			wantCode = codes.InvalidArgument
		}
		require.Equal(t, wantCode, status.Code(err))
		if wantCode == codes.Unavailable {
			require.ErrorContains(t, err, "signer")
		}
	}
}

// TestExportExitRejectsUnavailable covers invalid requests and refuses a
// locally spent VTXO even though its signed ancestry is still in storage.
func TestExportExitRejectsUnavailable(t *testing.T) {
	t.Parallel()
	rpc, desc := newExitExportFixture(t)
	_, err := rpc.ExportExit(t.Context(), &ExitExportRequest{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = rpc.ExportExit(
		t.Context(), &ExitExportRequest{Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{2},
		}.String()},
	)
	require.Equal(t, codes.NotFound, status.Code(err))
	require.NoError(
		t,
		rpc.server.vtxoStore.UpdateVTXOStatus(
			t.Context(), desc.Outpoint, vtxo.VTXOStatusSpent,
		),
	)
	_, err = rpc.ExportExit(
		t.Context(), &ExitExportRequest{
			Outpoint: desc.Outpoint.String(),
		},
	)
	require.Equal(t, codes.NotFound, status.Code(err))
}
