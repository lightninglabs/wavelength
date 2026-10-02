package db

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/internal/expiryfixture"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// TestBackfillCommitmentHeightsAuthenticatedPath exercises signed target
// validation, normal round decoding, database reload, and height-only repair.
// Distinct paths within one commitment remain distinct; a missing or heightless
// final fragment rolls back the entire repair. Shared cached trees stay intact.
func TestBackfillCommitmentHeightsAuthenticatedPath(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"round direct",
		"same commitment leaves",
		"multiple commitments",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var candidate *arkrpc.VTXO
			if name == "round direct" {
				candidate, _ = expiryfixture.Round(
					t, 10000, []byte{0x51}, 50, 321, 20,
				)
			} else {
				candidate, _ = expiryfixture.Merge(
					t, name == "same commitment leaves",
				)
			}
			store, _, baseDB := newVTXOStoreForTest(t)
			backfill := store.BackfillVTXOCommitmentHeights
			rows := baseDB.ListVTXOAncestryPaths
			desc := createTestVTXODescriptor(
				t, testRoundIDDB(name), 90,
			)
			copy(desc.Outpoint.Hash[:], candidate.Outpoint.Txid)
			desc.Outpoint.Index = candidate.Outpoint.Vout
			desc.PkScript = candidate.PkScript
			desc.Amount = btcutil.Amount(candidate.ValueSat)
			desc.ChainDepth = int(candidate.ChainDepth)
			var err error
			desc.Ancestry, err = vtxo.AncestryFromRPC(
				candidate.AncestryPaths,
			)
			require.NoError(t, err)
			for i := range desc.Ancestry {
				fragment := &desc.Ancestry[i]
				pb, err := roundpb.TreeToProto(
					fragment.TreePath,
				)
				require.NoError(t, err)
				fragment.TreePath, err = roundpb.TreeFromProto(
					pb,
				)
				require.NoError(t, err)
				require.NotNil(
					t, fragment.TreePath.Root.FinalKey,
				)
				fragment.CommitmentHeight = 0
			}
			desc.CommitmentTxID = desc.Ancestry[0].CommitmentTxID
			require.NoError(t, store.SaveVTXO(t.Context(), desc))

			// Hold the shared decode-cache view across the repair.
			cached, err := store.ListLiveVTXOs(t.Context())
			require.NoError(t, err)
			require.Len(t, cached, 1)
			rowKey := sqlc.ListVTXOAncestryPathsParams{
				VtxoOutpointHash:  desc.Outpoint.Hash[:],
				VtxoOutpointIndex: int32(desc.Outpoint.Index),
			}
			before, err := rows(
				t.Context(), rowKey,
			)
			require.NoError(t, err)

			// The resolver validates every signed spend against its
			// actual parent, and binds the target outpoint, value,
			// and script.
			response := &arkrpc.ListVTXOsByScriptsResponse{
				Vtxos: []*arkrpc.VTXO{
					candidate,
				},
			}
			extras, err := vtxo.ResolveIncomingAncestry(
				t.Context(), func(_ context.Context, script,
					_ []byte, _ uint32) (
					*arkrpc.ListVTXOsByScriptsResponse,
					error) {

					require.Equal(t, desc.PkScript, script)

					return response, nil
				}, desc.Outpoint, desc.PkScript, 128, 128,
			)
			require.NoError(t, err)
			if len(extras.Ancestry) > 1 {
				for _, invalid := range []string{
					"missing",
					"heightless",
					"duplicate",
				} {
					bad := append(
						[]vtxo.Ancestry(nil),
						extras.Ancestry...,
					)
					switch invalid {
					case "missing":
						bad = bad[:len(bad)-1]

					case "duplicate":
						dup := desc.Ancestry[0]
						h := bad[0].CommitmentHeight
						dup.CommitmentHeight = h
						bad = append(bad, dup)

					case "heightless":
						last := &bad[len(bad)-1]
						last.CommitmentHeight = 0
					}
					count, err := backfill(
						t.Context(), desc.Outpoint, bad,
						1000,
					)
					require.Error(t, err)
					require.Zero(t, count)
					after, err := rows(
						t.Context(), rowKey,
					)
					require.NoError(t, err)
					require.Equal(t, before, after)
				}
			}
			count, err := backfill(
				t.Context(), desc.Outpoint, extras.Ancestry,
				1000,
			)
			require.NoError(t, err)
			require.Equal(t, len(desc.Ancestry), count)
			after, err := rows(
				t.Context(), rowKey,
			)
			require.NoError(t, err)
			stored, err := store.GetVTXO(t.Context(), desc.Outpoint)
			require.NoError(t, err)
			for i := range after {
				require.Equal(
					t, extras.Ancestry[i].CommitmentHeight,
					stored.Ancestry[i].CommitmentHeight,
				)
				after[i].CommitmentHeight = 0
				cachedBytes, err := SerializeTree(
					cached[0].Ancestry[i].TreePath,
				)
				require.NoError(t, err)
				require.Equal(
					t, before[i].TreePath, cachedBytes,
				)
				indexedRoot := extras.Ancestry[i].TreePath.Root
				require.Nil(t, indexedRoot.FinalKey)
			}
			require.Equal(
				t, before, after, "only heights may change",
			)
			count, err = backfill(
				t.Context(), desc.Outpoint, extras.Ancestry,
				1000,
			)
			require.NoError(t, err)
			require.Zero(t, count)
		})
	}
}

// TestBackfillFragmentKeyBindsProof checks that cache omission cannot relax
// commitment, path, signed transaction, or asset metadata identity. Durable
// keys still distinguish cache representations, and neither input is mutated.
func TestBackfillFragmentKeyBindsProof(t *testing.T) {
	t.Parallel()
	candidate, _ := expiryfixture.Round(t, 10000, []byte{0x51}, 50, 321, 30)
	ancestry, err := vtxo.AncestryFromRPC(candidate.AncestryPaths)
	require.NoError(t, err)
	original := ancestry[0]
	pb, err := roundpb.TreeToProto(original.TreePath)
	require.NoError(t, err)
	original.TreePath, err = roundpb.TreeFromProto(pb)
	require.NoError(t, err)

	// Prove input binding independently of asset-context validation.
	bitcoinKey, err := backfillAncestryFragmentKey(original)
	require.NoError(t, err)
	bitcoinBytes, err := SerializeTree(original.TreePath)
	require.NoError(t, err)
	changedInput := original
	changedInput.TreePath, err = DeserializeTree(bitcoinBytes)
	require.NoError(t, err)
	changedInput.TreePath.Root.Input.Hash[0]++
	changedInputKey, err := backfillAncestryFragmentKey(changedInput)
	require.NoError(t, err)
	require.NotEqual(t, bitcoinKey, changedInputKey)

	// Asset data uses the same serializer and must survive cache omission.
	root := original.TreePath.Root
	ctx := tree.NewAssetTreeContext()
	ctx.SetAssetRef("synthetic-asset")
	ctx.SetNodeAssetAmount(root, 7)
	ctx.SetSigningTweak(root.Input, bytes.Repeat([]byte{1}, 32))
	ctx.SetLeafAssetRoot(root.Input, bytes.Repeat([]byte{2}, 32))
	ctx.SetSealedPackage(root.Input, []byte{3})
	original.TreePath.AssetContext = ctx
	originalBytes, err := SerializeTree(original.TreePath)
	require.NoError(t, err)
	want, err := backfillAncestryFragmentKey(original)
	require.NoError(t, err)

	tests := []struct {
		name   string
		change func(*vtxo.Ancestry)
		match  bool
	}{
		{
			"cache absent",
			func(a *vtxo.Ancestry) {
				a.TreePath.Root.FinalKey = nil
			},
			true,
		},
		{
			"commitment",
			func(a *vtxo.Ancestry) {
				a.CommitmentTxID[0]++
			},
			false,
		},
		{
			"batch outpoint",
			func(a *vtxo.Ancestry) {
				a.TreePath.BatchOutpoint.Index++
			},
			false,
		},
		{
			"output value",
			func(a *vtxo.Ancestry) {
				a.TreePath.Root.Outputs[0].Value++
			},
			false,
		},
		{
			"output script",
			func(a *vtxo.Ancestry) {
				a.TreePath.Root.Outputs[0].PkScript = []byte{
					0x52,
				}
			},
			false,
		},
		{"signature", func(a *vtxo.Ancestry) {
			key, _ := btcec.PrivKeyFromBytes([]byte{42})
			a.TreePath.Root.Signature, err = schnorr.Sign(
				key,
				bytes.Repeat(
					[]byte{4}, 32,
				),
			)
			require.NoError(t, err)
		}, false},
		{"cosigner", func(a *vtxo.Ancestry) {
			_, key := btcec.PrivKeyFromBytes([]byte{43})
			a.TreePath.Root.CoSigners[0] = key
		}, false},
		{
			"sweep root",
			func(a *vtxo.Ancestry) {
				a.TreePath.SweepTapscriptRoot[0]++
			},
			false,
		},
		{
			"batch output",
			func(a *vtxo.Ancestry) {
				a.TreePath.BatchOutput.Value++
			},
			false,
		},
		{
			"asset metadata absent",
			func(a *vtxo.Ancestry) {
				a.TreePath.AssetContext = nil
			},
			false,
		},
		{
			"asset reference",
			func(a *vtxo.Ancestry) {
				a.TreePath.AssetContext.SetAssetRef("other")
			},
			false,
		},
		{
			"asset amount",
			func(a *vtxo.Ancestry) {
				a.TreePath.AssetContext.SetNodeAssetAmount(
					a.TreePath.Root, 8,
				)
			},
			false,
		},
		{"asset tweak", func(a *vtxo.Ancestry) {
			a.TreePath.AssetContext.SetSigningTweak(
				root.Input,
				bytes.Repeat(
					[]byte{5}, 32,
				),
			)
		}, false},
		{"asset root", func(a *vtxo.Ancestry) {
			a.TreePath.AssetContext.SetLeafAssetRoot(
				root.Input,
				bytes.Repeat(
					[]byte{6}, 32,
				),
			)
		}, false},
		{
			"sealed package",
			func(a *vtxo.Ancestry) {
				a.TreePath.AssetContext.SetSealedPackage(
					root.Input, []byte{7},
				)
			},
			false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := original
			changed.TreePath, err = DeserializeTree(originalBytes)
			require.NoError(t, err)
			test.change(&changed)
			got, err := backfillAncestryFragmentKey(changed)
			require.NoError(t, err)
			if test.match {
				require.Equal(t, want, got)
				local, err := AncestryFragmentKey(original)
				require.NoError(t, err)
				remote, err := AncestryFragmentKey(changed)
				require.NoError(t, err)
				require.NotEqual(t, local, remote)
			} else {
				require.NotEqual(t, want, got)
			}
		})
	}
	after, err := SerializeTree(original.TreePath)
	require.NoError(t, err)
	require.Equal(t, originalBytes, after)
	// The commitment remains part of the key even without a tree.
	a, err := backfillAncestryFragmentKey(vtxo.Ancestry{})
	require.NoError(t, err)
	b, err := backfillAncestryFragmentKey(vtxo.Ancestry{
		CommitmentTxID: chainhash.Hash{1},
	})
	require.NoError(t, err)
	require.NotEqual(t, a, b)
}
