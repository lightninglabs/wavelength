//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"bytes"
	"encoding/hex"
	"fmt"
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
	"google.golang.org/protobuf/proto"
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
		BatchExpiryHeight: 900,
		CommitmentSweeps: map[wire.OutPoint]*waved.ExitCommitmentSweep{
			external: {DelayBlocks: 500, LeafScript: []byte{
				1,
			},
				ControlBlock: []byte{
					2,
				}},
		},
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
	require.Equal(t, int32(900), response.BatchExpiryHeight)
	require.Equal(t, &wavewalletrpc.ExitCommitmentSweep{
		DelayBlocks: 500, LeafScriptHex: "01", ControlBlockHex: "02",
	}, response.ExternalInputs[0].CommitmentSweep)
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

// TestExportExitsSharesAncestry preserves distinct destinations and deadlines
// while returning common ancestry once, in deterministic dependency order.
func TestExportExitsSharesAncestry(t *testing.T) {
	t.Parallel()
	service, rpc := newAdminFixture(t)
	external := wire.OutPoint{Hash: chainhash.Hash{1}}
	root := wire.NewMsgTx(3)
	root.AddTxIn(wire.NewTxIn(&external, nil, wire.TxWitness{{1}}))
	root.AddTxOut(&wire.TxOut{Value: 25_000, PkScript: []byte{0x51}})
	root.AddTxOut(&wire.TxOut{Value: 25_000, PkScript: []byte{0x51}})
	for i := uint32(0); i < 2; i++ {
		target := wire.OutPoint{Hash: root.TxHash(), Index: i}
		proof, err := recovery.NewProof(
			target, 144+i, &recovery.Node{
				Kind: recovery.NodeKindTree,
				Tx:   root,
			},
		)
		require.NoError(t, err)
		sweep := wire.NewMsgTx(3)
		sweep.AddTxIn(
			wire.NewTxIn(
				&target, nil, wire.TxWitness{{byte(i)}},
			),
		)
		sweep.AddTxOut(
			&wire.TxOut{
				Value:    25_000,
				PkScript: []byte{0x51},
			},
		)
		sweep.AddTxOut(arkscript.AnchorOutput())
		previousOutput := &wire.TxOut{
			Value: 50_000,
			PkScript: []byte{
				0x51,
			},
		}
		destination := fmt.Sprintf("destination-%d", i)
		rpc.exitExportsResp = append(
			rpc.exitExportsResp, &waved.ExitExport{
				Network:           "regtest",
				GenesisHash:       "genesis",
				BatchExpiryHeight: 900 + int32(i),
				SweepAddress:      destination,
				FinalSweep:        sweep,
				Ancestry: &unroll.SignedAncestry{
					Proof: proof,
					ExternalInputs: []unroll.AncestryInput{
						{Outpoint: external,
							Output: previousOutput,
						},
					},
				},
			},
		)
	}
	req := &wavewalletrpc.ExportExitsRequest{}
	for _, exported := range rpc.exitExportsResp {
		req.Exits = append(req.Exits, &wavewalletrpc.ExportExitRequest{
			Outpoint: exported.Ancestry.Proof.
				TargetOutpoint().
				String(),
			SweepAddress: exported.SweepAddress,
		})
	}
	response, err := service.ExportExits(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, uint32(2), response.FormatVersion)
	require.Len(t, response.Transactions, 1)
	require.Len(t, response.ExternalInputs, 1)
	require.Len(t, response.Targets, 2)
	for i, target := range response.Targets {
		require.Equal(t, req.Exits[i].Outpoint, target.TargetOutpoint)
		require.Equal(t, uint32(144+i), target.CsvDelay)
		require.Equal(t, int32(900+i), target.BatchExpiryHeight)
		require.Equal(
			t, req.Exits[i].SweepAddress,
			target.FinalSweep.DestinationAddress,
		)
		require.Equal(
			t, req.Exits[i].SweepAddress,
			rpc.exitExportsLast[i].SweepAddress,
		)
	}
	rpc.exitExportErr = status.Error(codes.NotFound, "missing target")
	response, err = service.ExportExits(t.Context(), req)
	require.Nil(t, response)
	require.Equal(t, codes.NotFound, status.Code(err))
}

// TestMergeExitExportsRefusesConflicts rejects ambiguity in shared transaction
// bytes, chain identity, and externally supplied previous outputs.
func TestMergeExitExportsRefusesConflicts(t *testing.T) {
	t.Parallel()
	rootID := chainhash.Hash{1}.String()
	childID := chainhash.Hash{2}.String()
	bundle := &wavewalletrpc.ExportExitResponse{
		Network: "regtest", GenesisHash: "genesis",
		Transactions: []*wavewalletrpc.ExitAncestryTransaction{
			{
				Txid:     rootID,
				RawTxHex: "0102",
			},
			{
				Txid:     childID,
				RawTxHex: "0304",
				ParentTxids: []string{
					rootID,
				},
			},
		},
		ExternalInputs: []*wavewalletrpc.ExitAncestryInput{{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{
					9,
				},
			}.String(),
			AmountSat: 50_000, PkScriptHex: "51",
		}},
	}
	merged, err := mergeExitExports(
		[]*wavewalletrpc.ExportExitResponse{bundle, bundle},
	)
	require.NoError(t, err)
	require.Len(t, merged.Transactions, 2)
	require.Equal(t, rootID, merged.Transactions[0].Txid)
	require.Len(t, merged.ExternalInputs, 1)
	for _, mutate := range []func(*wavewalletrpc.ExportExitResponse){
		func(b *wavewalletrpc.ExportExitResponse) {
			b.GenesisHash = "wrong"
		},
		func(b *wavewalletrpc.ExportExitResponse) {
			b.Transactions[0].RawTxHex = "wrong"
		},
		func(b *wavewalletrpc.ExportExitResponse) {
			b.ExternalInputs[0].AmountSat++
		},
	} {
		other, ok :=
			proto.Clone(bundle).(*wavewalletrpc.ExportExitResponse)
		require.True(t, ok)
		mutate(other)
		_, err := mergeExitExports(
			[]*wavewalletrpc.ExportExitResponse{bundle, other},
		)
		require.Error(t, err)
	}
}
