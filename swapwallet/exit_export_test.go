//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestExportExitProjectsPortableBundle verifies dependency order, exact witness
// serialization, anchor discovery, and external prevout projection before the
// swap runtime has started and without asking for backing-wallet inputs.
func TestExportExitProjectsPortableBundle(t *testing.T) {
	t.Parallel()
	service, rpc := newAdminFixture(t)
	external := wire.OutPoint{Hash: chainhash.Hash{1}}
	root := wire.NewMsgTx(3)
	root.AddTxIn(wire.NewTxIn(&external, nil, wire.TxWitness{{1, 2}}))
	root.AddTxOut(&wire.TxOut{Value: 50_000, PkScript: []byte{0x51}})
	root.AddTxOut(arkscript.AnchorOutput())
	parent := wire.OutPoint{Hash: root.TxHash()}
	target := wire.NewMsgTx(3)
	target.AddTxIn(wire.NewTxIn(&parent, nil, wire.TxWitness{{3, 4}}))
	target.AddTxOut(&wire.TxOut{Value: 49_000, PkScript: []byte{0x51}})
	outpoint := wire.OutPoint{Hash: target.TxHash()}
	proof, err := recovery.NewProof(
		outpoint, 144, &recovery.Node{
			Kind: recovery.NodeKindArk,
			Tx:   target,
		}, &recovery.Node{
			Kind: recovery.NodeKindTree,
			Tx:   root,
		},
	)
	require.NoError(t, err)
	rpc.exitExportResp = &waved.ExitExport{
		Network: "regtest", GenesisHash: "genesis",
		Ancestry: &unroll.SignedAncestry{
			Proof: proof,
			ExternalInputs: []unroll.AncestryInput{{
				Outpoint: external,
				Output: &wire.TxOut{
					Value: 50_000, PkScript: []byte{
						0x51,
					},
				},
			}},
		},
	}
	// The final sweep is supplied by the daemon; the wallet layer preserves
	// its exact witness bytes and separates it from immediate ancestry
	// work.
	sweep := wire.NewMsgTx(3)
	sweep.AddTxIn(wire.NewTxIn(&outpoint, nil, wire.TxWitness{{5, 6}}))
	sweep.AddTxOut(&wire.TxOut{Value: 49_000, PkScript: []byte{0x51}})
	sweep.AddTxOut(arkscript.AnchorOutput())
	rpc.exitExportResp.FinalSweep = sweep
	rpc.exitExportResp.SweepAddress = "destination"

	response, err := service.ExportExit(
		t.Context(), &wavewalletrpc.ExportExitRequest{
			Outpoint:     outpoint.String(),
			SweepAddress: "destination",
		},
	)
	require.NoError(t, err)
	require.Equal(t, outpoint.String(), rpc.exitExportLast.Outpoint)
	require.Equal(t, uint32(2), response.FormatVersion)
	require.Equal(t, "regtest", response.Network)
	require.Equal(t, "genesis", response.GenesisHash)
	require.Equal(t, outpoint.String(), response.TargetOutpoint)
	require.Equal(t, uint32(144), response.CsvDelay)
	require.Len(t, response.Transactions, 2)
	for i, tx := range []*wire.MsgTx{root, target} {
		raw, err := hex.DecodeString(response.Transactions[i].RawTxHex)
		require.NoError(t, err)
		var decoded wire.MsgTx
		require.NoError(t, decoded.Deserialize(bytes.NewReader(raw)))
		require.Equal(t, tx.WitnessHash(), decoded.WitnessHash())
		require.Equal(
			t, tx.TxHash().String(), response.Transactions[i].Txid,
		)
	}
	require.Equal(
		t, []string{parent.Hash.String()},
		response.Transactions[1].ParentTxids,
	)
	require.Equal(
		t, wire.OutPoint{Hash: root.TxHash(), Index: 1}.String(),
		response.Transactions[0].AnchorOutpoint,
	)
	require.Empty(t, response.Transactions[1].AnchorOutpoint)
	require.Equal(t, "tree", response.Transactions[0].Kind)
	require.Equal(t, "ark", response.Transactions[1].Kind)
	require.Len(t, response.ExternalInputs, 1)
	require.Equal(t, external.String(), response.ExternalInputs[0].Outpoint)
	require.Equal(t, int64(50_000), response.ExternalInputs[0].AmountSat)
	require.Equal(t, "51", response.ExternalInputs[0].PkScriptHex)
	require.Equal(t, "destination", rpc.exitExportLast.SweepAddress)
	require.NotNil(t, response.FinalSweep)
	require.Equal(t, "destination", response.FinalSweep.DestinationAddress)
	require.Equal(t, int64(49_000), response.FinalSweep.AmountSat)
	require.Equal(t, sweep.TxHash().String(), response.FinalSweep.Txid)
	require.Equal(
		t, wire.OutPoint{Hash: sweep.TxHash(), Index: 1}.String(),
		response.FinalSweep.AnchorOutpoint,
	)
	rawSweep, err := hex.DecodeString(response.FinalSweep.RawTxHex)
	require.NoError(t, err)
	var parsedSweep wire.MsgTx
	require.NoError(t, parsedSweep.Deserialize(bytes.NewReader(rawSweep)))
	require.Equal(t, sweep.WitnessHash(), parsedSweep.WitnessHash())

	require.Zero(t, rpc.unrollCalls)
	rpc.exitExportResp.FinalSweep = nil
	rpc.exitExportResp.SweepAddress = ""
	ancestryOnly, err := service.ExportExit(
		t.Context(), &wavewalletrpc.ExportExitRequest{
			Outpoint: outpoint.String(),
		},
	)
	require.NoError(t, err)
	require.Nil(t, ancestryOnly.FinalSweep)
	require.Empty(t, rpc.exitExportLast.SweepAddress)

}

// TestExportExitPropagatesFailure preserves the daemon's actionable status.
func TestExportExitPropagatesFailure(t *testing.T) {
	t.Parallel()
	service, rpc := newAdminFixture(t)
	rpc.exitExportErr = status.Error(
		codes.FailedPrecondition, "unsigned ancestry",
	)
	_, err := service.ExportExit(
		t.Context(), &wavewalletrpc.ExportExitRequest{
			Outpoint: "target:0",
		},
	)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}
