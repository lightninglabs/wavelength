package round

// End-to-end coverage for the interior VTXO-tree output binding that round
// admission already enforces.
//
// The binding is covered at the admission level in vtxo_tree_binding_test.go
// (TestValidateVTXOTreeBindingRejectsInteriorScriptSubstitution) and at the
// unit level in lib/tree/scripts_test.go. What these tests add is end-to-end
// depth over a real MuSig2 ceremony on a two-leaf tree: every node carries a
// signature that verifies for its declared cosigner set, including on the
// tampered tree, and the FSM is driven to the exact failure state with no
// nonce generated. The honest control is additionally spent by the consensus
// engine, so the tree is known to be spendable and not merely admitted.
//
// A tree can keep a valid outpoint topology, conserve value, and pay the
// client's leaf correctly while the interior output that funds that leaf pays
// a script that is not the leaf's final key. Admission must reject such a tree
// before the client contributes nonces or signatures.

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/require"
)

// interiorBindingSweepDelay is the sweep delay the fixture tree commits to.
// validateRoundSweepPolicy reconstructs the sweep leaf from the round's
// advertised key and delay and requires its tap hash to match every tree's
// SweepTapscriptRoot.
const interiorBindingSweepDelay = 1008

// interiorBindingTwoLeafTree builds an honest two-party tree with one leaf
// for the client under test and one for a second participant. The root is then
// a real branch whose outputs fund the leaves, which a single-leaf tree cannot
// express.
func interiorBindingTwoLeafTree(t *testing.T, h *realSigningTestHarness,
	vtxoReq types.VTXORequest, otherPubKey *btcec.PublicKey) *tree.Tree {

	t.Helper()

	clientPkScript, err := vtxoReq.EffectivePkScript()
	require.NoError(t, err)

	otherPkScript, err := txscript.PayToTaprootScript(otherPubKey)
	require.NoError(t, err)

	leaves := []tree.LeafDescriptor{{
		PkScript:    clientPkScript,
		Amount:      vtxoReq.Amount,
		CoSignerKey: h.clientPubKey,
	}, {
		PkScript:    otherPkScript,
		Amount:      vtxoReq.Amount,
		CoSignerKey: otherPubKey,
	}}

	batchPkScript, err := txscript.PayToTaprootScript(h.operatorPubKey)
	require.NoError(t, err)

	batchOutput := &wire.TxOut{
		Value:    int64(vtxoReq.Amount) * 2,
		PkScript: batchPkScript,
	}

	sweepLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		h.operatorPubKey, interiorBindingSweepDelay,
	)
	require.NoError(t, err)
	sweepRoot := sweepLeaf.TapHash()

	vtxtTree, err := tree.NewTree(
		h.newTestOutpoint(), batchOutput, leaves, h.operatorPubKey,
		sweepRoot[:], 2,
	)
	require.NoError(t, err)

	return vtxtTree
}

// interiorBindingSignNode runs a full MuSig2 ceremony over sigHash for the
// given signer set and returns the final aggregate signature. Tree cosigner
// sets are not uniform (the root commits to every leaf owner), so each node is
// signed by exactly its declared cosigners.
func interiorBindingSignNode(t *testing.T, sigHash [32]byte, sweepRoot []byte,
	privKeys ...*btcec.PrivateKey) *schnorr.Signature {

	t.Helper()

	pubKeys := make([]*btcec.PublicKey, len(privKeys))
	for i, priv := range privKeys {
		pubKeys[i] = priv.PubKey()
	}

	ctxOpts := []musig2.ContextOption{
		musig2.WithKnownSigners(pubKeys),
	}
	if len(sweepRoot) > 0 {
		ctxOpts = append(
			ctxOpts, musig2.WithTaprootTweakCtx(sweepRoot),
		)
	}

	nonces := make([]*musig2.Nonces, len(privKeys))
	sessions := make([]*musig2.Session, len(privKeys))
	for i, priv := range privKeys {
		n, err := musig2.GenNonces(
			musig2.WithPublicKey(
				priv.PubKey(),
			),
			musig2.WithNonceSecretKeyAux(priv),
		)
		require.NoError(t, err)
		nonces[i] = n

		ctx, err := musig2.NewContext(priv, true, ctxOpts...)
		require.NoError(t, err)

		sess, err := ctx.NewSession(musig2.WithPreGeneratedNonce(n))
		require.NoError(t, err)
		sessions[i] = sess
	}

	for i, sess := range sessions {
		for j, n := range nonces {
			if i == j {
				continue
			}

			_, err := sess.RegisterPubNonce(n.PubNonce)
			require.NoError(t, err)
		}
	}

	partials := make([]*musig2.PartialSignature, len(sessions))
	for i, sess := range sessions {
		partial, err := sess.Sign(sigHash)
		require.NoError(t, err)
		partials[i] = partial
	}

	var haveAll bool
	for i := 1; i < len(partials); i++ {
		var err error
		haveAll, err = sessions[0].CombineSig(partials[i])
		require.NoError(t, err)
	}
	require.True(t, haveAll)

	finalSig := sessions[0].FinalSig()
	require.NotNil(t, finalSig)

	return finalSig
}

// interiorBindingSignTree produces a valid aggregate signature for every node
// of the tree, signing each node's sighash with that node's declared cosigner
// set. The sighashes commit to whatever prevouts the tree declares, so the
// signatures are cryptographically valid even for a tree whose interior output
// scripts were replaced as well.
func interiorBindingSignTree(t *testing.T, h *realSigningTestHarness,
	otherPrivKey *btcec.PrivateKey, otherPubKey *btcec.PublicKey,
	vtxtTree *tree.Tree) map[tree.TxID]*schnorr.Signature {

	t.Helper()

	keyring := map[string]*btcec.PrivateKey{
		string(h.clientPubKey.SerializeCompressed()):   h.clientPrivKey,
		string(h.operatorPubKey.SerializeCompressed()): h.operatorPrivKey,
		string(otherPubKey.SerializeCompressed()):      otherPrivKey,
	}

	fetcher, err := vtxtTree.Root.PrevOutputFetcher(vtxtTree.BatchOutput)
	require.NoError(t, err)

	sigs := make(map[tree.TxID]*schnorr.Signature)
	err = vtxtTree.Root.ForEach(func(node *tree.Node) error {
		tx, txErr := node.ToTx()
		if txErr != nil {
			return txErr
		}

		sigHash, sigErr := computeTreeNodeSigHash(node, tx, fetcher)
		if sigErr != nil {
			return sigErr
		}

		privKeys := make([]*btcec.PrivateKey, 0, len(node.CoSigners))
		for _, cosigner := range node.CoSigners {
			priv, ok := keyring[string(
				cosigner.SerializeCompressed(),
			)]
			if !ok {
				return fmt.Errorf("no private key for "+
					"cosigner %x",
					cosigner.SerializeCompressed())
			}

			privKeys = append(privKeys, priv)
		}

		sigs[tree.TxID(tx.TxHash())] = interiorBindingSignNode(
			t, sigHash, vtxtTree.SweepTapscriptRoot, privKeys...,
		)

		return nil
	})
	require.NoError(t, err)

	return sigs
}

// interiorBindingClientLeaf locates the client's leaf among the root's
// children and returns its child index, which is also the parent output index
// that funds it.
func interiorBindingClientLeaf(t *testing.T, vtxtTree *tree.Tree,
	clientPubKey *btcec.PublicKey) (uint32, *tree.Node) {

	t.Helper()

	root := vtxtTree.Root
	require.Len(t, root.Children, 2)
	require.Len(t, root.Outputs, 3)

	var childIdx uint32
	var clientLeaf *tree.Node
	for idx, child := range root.Children {
		if child.IsLeaf() && tree.ContainsCosigner(
			child.CoSigners, clientPubKey,
		) {

			childIdx, clientLeaf = idx, child
		}
	}
	require.NotNil(t, clientLeaf)
	require.Equal(t, childIdx, clientLeaf.Input.Index)

	return childIdx, clientLeaf
}

// interiorBindingEngineSpend runs the consensus script engine over the
// client's leaf transaction spending the given parent output. A nil error
// means the leaf can really be broadcast against that output.
func interiorBindingEngineSpend(t *testing.T, clientLeaf *tree.Node,
	parentOutput *wire.TxOut) error {

	t.Helper()

	leafTx, err := clientLeaf.ToSignedTx()
	require.NoError(t, err)

	fetcher := txscript.NewCannedPrevOutputFetcher(
		parentOutput.PkScript, parentOutput.Value,
	)
	sigHashes := txscript.NewTxSigHashes(leafTx, fetcher)

	engine, err := txscript.NewEngine(
		parentOutput.PkScript, leafTx, 0, txscript.StandardVerifyFlags,
		nil, sigHashes, parentOutput.Value, fetcher,
	)
	require.NoError(t, err)

	return engine.Execute()
}

// interiorBindingSigBytes converts parsed signatures into the wire form
// accepted by ValidateAndSubmitSignatures.
func interiorBindingSigBytes(
	sigs map[tree.TxID]*schnorr.Signature) map[chainhash.Hash][]byte {

	sigBytes := make(map[chainhash.Hash][]byte, len(sigs))
	for txid, sig := range sigs {
		sigBytes[chainhash.Hash(txid)] = sig.Serialize()
	}

	return sigBytes
}

// interiorBindingAdmission drives CommitmentTxReceivedState through the
// CommitmentTxBuilt event, the state that runs the tree binding and per-VTXO
// path validation before any nonce starts the MuSig2 ceremony.
func interiorBindingAdmission(t *testing.T, h *realSigningTestHarness,
	roundID RoundID, packet *psbt.Packet, vtxtTree *tree.Tree,
	intent BoardingIntent,
	vtxoReq types.VTXORequest) *ClientStateTransition {

	t.Helper()

	state := &CommitmentTxReceivedState{
		RoundID:      roundID,
		CommitmentTx: packet,
		TxID:         packet.UnsignedTx.TxHash(),
		SweepKey:     h.operatorPubKey,
		SweepDelay:   interiorBindingSweepDelay,
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
		Intents: Intents{
			Boarding: []BoardingIntent{
				intent,
			},
			VTXOs: []types.VTXORequest{
				vtxoReq,
			},
		},
		ClientTrees: make(map[SignerKey]*tree.Tree),
	}
	h.withState(state)

	transition, err := h.sendEvent(&CommitmentTxBuilt{
		RoundID: roundID,
		Tx:      packet,
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, transition)

	return transition
}

// interiorBindingSettleState constructs the post-admission state directly from
// the admitted tree, skipping the nonce and partial-signature rounds that the
// real flow runs first, so the test can deliver the operator's aggregated tree
// signatures to the release path.
func interiorBindingSettleState(t *testing.T, h *realSigningTestHarness,
	roundID RoundID, packet *psbt.Packet, vtxtTree *tree.Tree,
	intent BoardingIntent, vtxoReq types.VTXORequest) {

	t.Helper()

	signerKey := NewSignerKey(vtxoReq.SigningKey.PubKey)
	clientTree, err := vtxtTree.ExtractPathForCoSigners(
		vtxoReq.SigningKey.PubKey,
	)
	require.NoError(t, err)
	require.NotNil(t, clientTree)

	state := &PartialSigsSentState{
		RoundID:      roundID,
		CommitmentTx: packet,
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
		Intents: Intents{
			Boarding: []BoardingIntent{
				intent,
			},
			VTXOs: []types.VTXORequest{
				vtxoReq,
			},
		},
		ClientTrees: map[SignerKey]*tree.Tree{
			signerKey: clientTree,
		},
		BoardingInputIndices: map[wire.OutPoint]int{
			intent.Outpoint: 0,
		},
		Musig2Sessions: make(map[SignerKey]*tree.SignerSession),
	}

	h.setupMockWalletForBoardingSigning()
	h.setupMockRoundStoreForCommit()
	h.withState(state)
}

// TestVTXOTreeInteriorOutputBinding exercises the admission-time binding of
// every interior tree edge to its child's declared cosigners, end to end over
// a real MuSig2 ceremony. The honest control passes every gate and its
// presigned leaf really spends the branch output under consensus validation.
// For the tampered tree, topology, values, and aggregate signatures all remain
// valid, yet admission rejects it with the child-script error before any nonce
// is requested, and the presigned leaf cannot spend the substituted output.
func TestVTXOTreeInteriorOutputBinding(t *testing.T) {
	t.Parallel()

	t.Run("honest tree admitted, release from constructed "+
		"post-admission state", func(t *testing.T) {
		t.Parallel()

		h := newRealSigningTestHarness(t)
		intent := h.newTestBoardingIntentWithTapscript()
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		otherPrivKey, otherPubKey := generateTestKeyPair(t)
		vtxtTree := interiorBindingTwoLeafTree(
			t, h, vtxoReq, otherPubKey,
		)
		packet := h.bindTreeToCommitment(
			[]BoardingIntent{intent}, vtxtTree,
		)

		childIdx, clientLeaf := interiorBindingClientLeaf(
			t, vtxtTree, h.clientPubKey,
		)

		// The output funding the client's leaf is the taproot script
		// of that leaf's declared cosigner set, tweaked by the tree's
		// sweep root.
		honestKey, err := tree.ComputeFinalKey(
			clientLeaf.CoSigners, vtxtTree.SweepTapscriptRoot,
		)
		require.NoError(t, err)
		honestScript, err := txscript.PayToTaprootScript(honestKey)
		require.NoError(t, err)
		require.Equal(
			t, honestScript,
			vtxtTree.Root.Outputs[childIdx].PkScript,
		)

		sigs := interiorBindingSignTree(
			t, h, otherPrivKey, otherPubKey, vtxtTree,
		)

		// Every admission gate accepts the honest tree.
		require.NoError(
			t,
			validateVTXOTreeBinding(
				packet.UnsignedTx, map[int]*tree.Tree{
					0: vtxtTree,
				},
			),
		)
		require.NoError(t, vtxtTree.ValidateAnchors())

		// Note the ordering: ValidateAndSubmitSignatures stores the
		// aggregate signatures on the nodes before admission runs. The
		// real flow only sees signatures after the nonce and partial
		// signature rounds, so this is a test convenience; the release
		// test below starts from a state built with the same
		// assumption.
		require.NoError(
			t,
			vtxtTree.ValidateAndSubmitSignatures(
				interiorBindingSigBytes(sigs),
			),
		)

		// The presigned leaf really spends the honest branch output
		// under consensus script validation.
		require.NoError(
			t, interiorBindingEngineSpend(
				t, clientLeaf, vtxtTree.Root.Outputs[childIdx],
			),
		)

		roundID := testRoundIDTr("interior-binding-honest")
		transition := interiorBindingAdmission(
			t, h, roundID, packet, vtxtTree, intent, vtxoReq,
		)
		assertStateType[*CommitmentTxValidatedState](
			h.boardingTestHarness,
		)
		assertTransitionEmitsInternalEvent[*GenerateNonces](
			h.boardingTestHarness, transition,
		)

		// Release is driven from this constructed post-admission
		// state rather than by replaying the nonce and partial
		// signature rounds.
		h.clearOutbox()
		interiorBindingSettleState(
			t, h, roundID, packet, vtxtTree, intent, vtxoReq,
		)

		transition, err = h.sendEvent(&OperatorSigned{
			RoundID: roundID,
			AggSigs: sigs,
		})
		require.NoError(t, err)
		require.NotNil(t, transition)

		assertStateType[*InputSigSentState](h.boardingTestHarness)
		h.assertOutboxContainsType("*round.SubmitForfeitSigRequest")
		h.assertOutboxContainsType("*round.RegisterConfirmationRequest")
	})

	t.Run("interior output substitution rejected at admission",
		func(t *testing.T) {
			t.Parallel()

			h := newRealSigningTestHarness(t)
			intent := h.newTestBoardingIntentWithTapscript()
			vtxoReq := h.newTestVTXORequestForIntent(intent)
			otherPrivKey, otherPubKey := generateTestKeyPair(t)
			vtxtTree := interiorBindingTwoLeafTree(
				t, h, vtxoReq, otherPubKey,
			)
			packet := h.bindTreeToCommitment(
				[]BoardingIntent{intent}, vtxtTree,
			)

			childIdx, clientLeaf := interiorBindingClientLeaf(
				t, vtxtTree, h.clientPubKey,
			)

			// Replace the single root output funding the client's
			// leaf with a script for the operator key, which is a
			// cosigner of every node in this tree. The substitution
			// is therefore invisible to cosigner-set membership and
			// is only caught by recomputing the child's final key.
			// The value is unchanged, so conservation still holds.
			substitutedScript, err := txscript.PayToTaprootScript(
				h.operatorPubKey,
			)
			require.NoError(t, err)
			require.NotEqual(
				t, substitutedScript,
				vtxtTree.Root.Outputs[childIdx].PkScript,
			)
			vtxtTree.Root.Outputs[childIdx].PkScript = substitutedScript

			// Repair the child input links so the tree remains
			// structurally consistent, exactly as an operator built
			// it.
			relinkVTXOTree(t, vtxtTree.Root)

			// Re-sign every node over the relinked prevouts, so the
			// aggregate signatures are valid for this tree and only
			// the missing script binding can reject it.
			sigs := interiorBindingSignTree(
				t, h, otherPrivKey, otherPubKey, vtxtTree,
			)

			// The leaf itself still pays the client's accepted
			// script, so the leaf-level check has nothing to catch.
			leafPkScript, err := vtxoReq.EffectivePkScript()
			require.NoError(t, err)
			require.Equal(
				t, leafPkScript, clientLeaf.Outputs[0].PkScript,
			)

			// Structural topology and value flow still hold.
			require.NoError(t, vtxtTree.ValidateValueConservation())
			require.NoError(t, vtxtTree.ValidateAnchors())

			// The aggregate signatures also verify against this
			// tree, so a rejection below is attributable to the
			// script binding alone rather than to a bad signature.
			require.NoError(
				t,
				vtxtTree.ValidateAndSubmitSignatures(
					interiorBindingSigBytes(sigs),
				),
			)

			// Pairing with the honest control's consensus-engine
			// spend: the presigned leaf cannot spend the
			// substituted output, even though its signature is
			// valid for this tree's sighash.
			spendErr := interiorBindingEngineSpend(
				t, clientLeaf, vtxtTree.Root.Outputs[childIdx],
			)
			require.Error(t, spendErr)
			require.True(
				t, txscript.IsErrorCode(
					spendErr, txscript.ErrTaprootSigInvalid,
				),
				"expected a taproot signature failure, got %v",
				spendErr,
			)

			// The binding rejects the replaced interior edge with
			// the child-script error, naming the child output.
			err = validateVTXOTreeBinding(
				packet.UnsignedTx, map[int]*tree.Tree{
					0: vtxtTree,
				},
			)
			require.ErrorContains(t, err, "child output scripts")
			require.ErrorContains(
				t, err,
				fmt.Sprintf("output %d script does not match "+
					"child cosigners", childIdx),
			)

			// The same tree is rejected by the FSM at admission,
			// before it emits a nonce request.
			roundID := testRoundIDTr("interior-binding-replaced")
			transition := interiorBindingAdmission(
				t, h, roundID, packet, vtxtTree, intent,
				vtxoReq,
			)

			failedState := assertStateType[*ClientFailedState](
				h.boardingTestHarness,
			)
			require.Equal(
				t, "VTXO tree commitment binding failed",
				failedState.Reason,
			)
			require.ErrorContains(
				t, failedState.Error, "child output scripts",
			)
			require.ErrorContains(
				t, failedState.Error,
				"does not match child cosigners",
			)
			require.False(t, failedState.Recoverable)

			transition.NewEvents.WhenSome(
				func(emitted ClientEmittedEvent) {
					for _, evt := range emitted.InternalEvent {
						_, isNonce := evt.(*GenerateNonces)
						require.False(t, isNonce)
					}
				},
			)
		})
}
