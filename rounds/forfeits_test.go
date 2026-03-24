package rounds

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/tx"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestCompleteForfeitTxs verifies that completeForfeitTxs correctly adds the
// server's signatures to forfeit transactions. A forfeit tx has two inputs:
//  1. VTXO input (index 0): spent via collaborative tapscript path requiring
//     both client and server signatures
//  2. Connector input (index 1): spent via operator-only keyspend path
//
// The client provides their VTXO signature upfront; completeForfeitTxs adds
// the server's signatures to both inputs to produce a fully-signed tx.
func TestCompleteForfeitTxs(t *testing.T) {
	t.Parallel()

	t.Run("completes forfeit tx", func(t *testing.T) {
		t.Parallel()

		const (
			vtxoAmount = btcutil.Amount(50000)
			exitDelay  = 144
		)
		h := newTestHarness(t)
		h.env.Terms.VTXOExitDelay = exitDelay

		// Create a client keypair. The client's public key is the
		// cosigner for the VTXO's collaborative spending path.
		clientPriv := testForfeitPrivKey(1)
		clientPub := clientPriv.PubKey()

		// Create the VTXO descriptor which defines the taproot output
		// structure: a collaborative path (client + operator) and an
		// exit path (client after timelock).
		vtxoDesc, err := tree.NewVTXODescriptor(
			vtxoAmount, clientPub, h.operatorPub, nil, exitDelay,
		)
		require.NoError(t, err)

		vtxo := &VTXO{
			Descriptor: vtxoDesc,
			OperatorKeyDesc: &keychain.KeyDescriptor{
				PubKey: h.operatorPub,
			},
			Status: VTXOStatusLive,
		}

		// Define the outpoint for the VTXO being forfeited.
		vtxoOutpoint := wire.OutPoint{
			Hash:  testForfeitHash(t, "vtxo"),
			Index: 0,
		}

		// Build a commitment transaction with a connector output.
		commitmentTx := wire.NewMsgTx(2)
		connectorScript, err := txscript.PayToTaprootScript(
			txscript.ComputeTaprootOutputKey(h.operatorPub, nil),
		)
		require.NoError(t, err)

		connectorOutput := &wire.TxOut{
			Value:    int64(h.env.Terms.ConnectorDustAmount),
			PkScript: connectorScript,
		}
		commitmentTx.AddTxOut(connectorOutput)

		connectorRootOutpoint := wire.OutPoint{
			Hash:  commitmentTx.TxHash(),
			Index: 0,
		}

		connectorTree, err := tree.BuildConnectorTree(
			connectorRootOutpoint,
			connectorOutput,
			tree.ConnectorDescriptor{
				PkScript:  connectorOutput.PkScript,
				NumLeaves: 1,
				Amount:    h.env.Terms.ConnectorDustAmount,
			},
			h.operatorPub,
			int(h.env.Terms.TreeRadix),
		)
		require.NoError(t, err)

		leafPath, err := connectorTree.ExtractPathForIndices(0)
		require.NoError(t, err)

		leaves := leafPath.Root.GetLeafNodes()
		require.Len(t, leaves, 1)

		leaf := leaves[0]
		connectorLeafOutpoint, err := leaf.GetNonAnchorOutpoint()
		require.NoError(t, err)

		var connectorLeafOutput *wire.TxOut
		anchorScript := scripts.AnchorOutput().PkScript
		for _, out := range leaf.Outputs {
			if !bytes.Equal(out.PkScript, anchorScript) {
				connectorLeafOutput = out
				break
			}
		}
		require.NotNil(t, connectorLeafOutput)

		// Set up the client registration with the forfeit input.
		forfeitInput := &ForfeitInput{
			Outpoint: &vtxoOutpoint,
			VTXO:     vtxo,
		}
		reg := &ClientRegistration{
			ForfeitInputs: []*ForfeitInput{forfeitInput},
		}

		// Build the unsigned forfeit transaction structure. This has
		// the VTXO as input 0, connector as input 1, and outputs for
		// the penalty (to server) and anchor.
		forfeitTx := buildForfeitTx(
			t, vtxoOutpoint, vtxoAmount, *connectorLeafOutpoint,
			h.env.ForfeitScript,
		)

		// Client signs the VTXO input using the collaborative tapscript
		// path. This is the signature the client submits to the server.
		clientSig := forfeitTxSig(
			t, forfeitTx, clientPriv, vtxoOutpoint,
			connectorLeafOutput, h.operatorPub, exitDelay,
			vtxoDesc,
		)

		// Set up connector assignments mapping forfeited VTXOs to their
		// assigned connector leaves.
		leafOutpoint := *connectorLeafOutpoint
		leafOutput := connectorLeafOutput
		connectorAssignments :=
			map[wire.OutPoint]*ConnectorLeafAssignment{
				vtxoOutpoint: {
					ConnectorOutputIndex: 0,
					LeafIndex:            0,
					LeafOutpoint:         leafOutpoint,
					LeafOutput:           leafOutput,
				},
			}

		// Call completeForfeitTxs to add the server's signatures.
		// This should sign both the VTXO input (collaborative path)
		// and the connector input (operator keyspend).
		spent, err := completeForfeitTxs(
			[]*types.ForfeitTxSig{{
				UnsignedTx:    forfeitTx,
				ClientVTXOSig: clientSig,
			}},
			reg, connectorAssignments,
			h.env.WalletController,
			h.env.Terms.OperatorKey,
			h.env.RoundID,
		)
		require.NoError(t, err)
		require.Len(t, spent, 1)

		descriptor := &ConnectorTreeDescriptor{
			OutputIndex:   0,
			NumLeaves:     1,
			ForfeitScript: h.env.ForfeitScript,
		}

		rehydratedTree, err := buildConnectorTreeFromDescriptor(
			commitmentTx, descriptor, h.operatorPub,
			int(h.env.Terms.TreeRadix),
		)
		require.NoError(t, err)

		rehydratedPath, err := rehydratedTree.ExtractPathForIndices(
			spent[0].ForfeitInfo.LeafIndex,
		)
		require.NoError(t, err)

		rehydratedLeaves := rehydratedPath.Root.GetLeafNodes()
		require.Len(t, rehydratedLeaves, 1)

		rehydratedLeaf := rehydratedLeaves[0]
		rehydratedOutpoint, err := rehydratedLeaf.GetNonAnchorOutpoint()
		require.NoError(t, err)

		require.Equal(t, *rehydratedOutpoint,
			forfeitTx.TxIn[tx.ForfeitConnectorInputIndex].
				PreviousOutPoint,
		)

		// Verify both inputs now have witness data.
		vtxoWitness :=
			forfeitTx.TxIn[tx.ForfeitVTXOInputIndex].Witness
		require.NotEmpty(t, vtxoWitness)

		connectorWitness :=
			forfeitTx.TxIn[tx.ForfeitConnectorInputIndex].Witness
		require.NotEmpty(t, connectorWitness)

		// Build a prevout fetcher for script verification. This
		// provides the previous outputs being spent so the script
		// engine can verify signatures.
		prevFetcher, err := tx.NewForfeitPrevOutFetcher(
			&tx.VTXOSpendContext{
				Outpoint: vtxoOutpoint,
				Output: &wire.TxOut{
					Value:    int64(vtxoDesc.Amount),
					PkScript: vtxoDesc.PkScript,
				},
				TapScript: mustVTXOTapScript(
					t, clientPub, h.operatorPub,
					exitDelay,
				),
			},
			&tx.ConnectorSpendContext{
				Outpoint: *connectorLeafOutpoint,
				Output:   connectorLeafOutput,
			},
		)
		require.NoError(t, err)

		sigHashes := txscript.NewTxSigHashes(forfeitTx, prevFetcher)

		// Verify the VTXO input (index 0) executes successfully. This
		// validates the collaborative tapscript path with both the
		// client and server signatures.
		testutils.AssertEngineExecution(t, 0, true, func() (
			*txscript.Engine, error) {

			return txscript.NewEngine(
				vtxoDesc.PkScript, forfeitTx,
				tx.ForfeitVTXOInputIndex,
				txscript.StandardVerifyFlags, nil,
				sigHashes, int64(vtxoDesc.Amount), prevFetcher,
			)
		})

		// Verify the connector input (index 1) executes successfully.
		// This validates the operator-only keyspend signature.
		testutils.AssertEngineExecution(t, 1, true, func() (
			*txscript.Engine, error) {

			return txscript.NewEngine(
				connectorLeafOutput.PkScript, forfeitTx,
				tx.ForfeitConnectorInputIndex,
				txscript.StandardVerifyFlags, nil,
				sigHashes, connectorLeafOutput.Value,
				prevFetcher,
			)
		})
	})

	// This test verifies that completeForfeitTxs rejects forfeit
	// transactions that reference a different connector leaf than what
	// was assigned. This prevents clients from using arbitrary connectors.
	t.Run("wrong connector leaf rejected", func(t *testing.T) {
		t.Parallel()

		const (
			vtxoAmount = btcutil.Amount(50000)
			exitDelay  = 144
		)
		h := newTestHarness(t)
		h.env.Terms.VTXOExitDelay = exitDelay

		clientPriv := testForfeitPrivKey(2)
		clientPub := clientPriv.PubKey()

		vtxoDesc, err := tree.NewVTXODescriptor(
			vtxoAmount, clientPub, h.operatorPub, nil, exitDelay,
		)
		require.NoError(t, err)

		vtxo := &VTXO{
			Descriptor: vtxoDesc,
			OperatorKeyDesc: &keychain.KeyDescriptor{
				PubKey: h.operatorPub,
			},
			Status: VTXOStatusLive,
		}

		vtxoOutpoint := wire.OutPoint{
			Hash:  testForfeitHash(t, "vtxo-wrong"),
			Index: 0,
		}
		connectorOutpoint := wire.OutPoint{
			Hash:  testForfeitHash(t, "connector-wrong"),
			Index: 0,
		}

		connectorScript, err := txscript.PayToTaprootScript(
			txscript.ComputeTaprootOutputKey(h.operatorPub, nil),
		)
		require.NoError(t, err)

		connectorLeafOutput := &wire.TxOut{
			Value:    int64(330),
			PkScript: connectorScript,
		}

		forfeitInput := &ForfeitInput{
			Outpoint: &vtxoOutpoint,
			VTXO:     vtxo,
		}
		reg := &ClientRegistration{
			ForfeitInputs: []*ForfeitInput{forfeitInput},
		}

		// Build forfeit tx that spends connectorOutpoint.
		forfeitTx := buildForfeitTx(
			t, vtxoOutpoint, vtxoAmount, connectorOutpoint,
			h.env.ForfeitScript,
		)

		clientSig := forfeitTxSig(
			t, forfeitTx, clientPriv, vtxoOutpoint,
			connectorLeafOutput, h.operatorPub, exitDelay,
			vtxoDesc,
		)

		// Create connector assignments with a DIFFERENT leaf outpoint
		// than what the forfeit tx actually references. This simulates
		// a client trying to use an unauthorized connector.
		wrongLeafHash := testForfeitHash(t, "wrong-leaf")
		connectorAssignments :=
			map[wire.OutPoint]*ConnectorLeafAssignment{
				vtxoOutpoint: {
					LeafOutpoint: wire.OutPoint{
						Hash:  wrongLeafHash,
						Index: 1,
					},
					LeafOutput: connectorLeafOutput,
				},
			}

		// Should fail because the forfeit tx references
		// connectorOutpoint but the assignment says it should
		// reference wrongLeafHash.
		_, err = completeForfeitTxs(
			[]*types.ForfeitTxSig{{
				UnsignedTx:    forfeitTx,
				ClientVTXOSig: clientSig,
			}},
			reg, connectorAssignments,
			h.env.WalletController,
			h.env.Terms.OperatorKey,
			h.env.RoundID,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(),
			"references wrong connector leaf")
	})
}

// testForfeitPrivKey returns a deterministic private key for tests.
func testForfeitPrivKey(index byte) *btcec.PrivateKey {
	keyBytes := make([]byte, 32)
	keyBytes[31] = index

	privKey, _ := btcec.PrivKeyFromBytes(keyBytes)

	return privKey
}

// testForfeitHash returns a deterministic hash for tests.
func testForfeitHash(t *testing.T, tag string) chainhash.Hash {
	t.Helper()

	return chainhash.HashH([]byte(tag))
}

// mustVTXOTapScript returns a VTXO tapscript or fails the test.
func mustVTXOTapScript(t *testing.T, clientPub *btcec.PublicKey,
	operatorPub *btcec.PublicKey, exitDelay uint32) *waddrmgr.Tapscript {

	t.Helper()

	tapScript, err := scripts.VTXOTapScript(
		clientPub, operatorPub, exitDelay,
	)
	require.NoError(t, err)

	return tapScript
}
