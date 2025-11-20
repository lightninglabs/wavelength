package assets_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/darepo-client/assets"
	"github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/taproot-assets/address"
	tapasset "github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/rpcutils"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/assetwalletrpc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

const (
	assetTreeTimeout        = 60 * time.Second
	csvDelayBlocks          = 10
	clientShare      uint64 = 30_000
)

type clientTreeContext struct {
	alias       string
	tapClient   *harness.TapClientHarness
	tapdHarness *harness.TapdHarness
	userKey     *btcec.PrivateKey
	operatorKey *btcec.PrivateKey
	leaf        tree.LeafDescriptor
	signer      *assets.LocalMuSig2Signer
	treeSigner  *tree.SignerSession
}

func TestArkAssetTree(t *testing.T) {
	opts := harness.DefaultOptions()
	opts.StartTapd = true
	opts.GroupName = "asset-tree-e2e"

	h := harness.NewHarness(t, &opts)
	h.Start()
	t.Cleanup(h.Stop)

	harness.FundNode(h, h.LND)

	operatorClient := h.NewTapClientHarness("operator")
	t.Cleanup(operatorClient.Close)

	var assetID tapasset.ID
	clientAliases := []string{"alice", "bob", "carol"}
	totalAmount := clientShare * uint64(len(clientAliases))
	minted := operatorClient.MintAsset(
		"ark-tree-asset", totalAmount, taprpc.AssetType_NORMAL,
	)
	copy(assetID[:], minted.AssetGenesis.AssetId)

	clients := make([]*harness.TapClientHarness, 0, len(clientAliases))
	clientHarnesses := make([]*harness.TapdHarness, 0, len(clientAliases))

	for _, alias := range clientAliases {
		tapdHarness := h.NewTapdHarness(alias)
		clientHarnesses = append(clientHarnesses, tapdHarness)
		client := tapdHarness.NewTapClientHarness()
		clients = append(clients, client)
		t.Cleanup(client.Close)
		t.Cleanup(tapdHarness.Stop)

		harness.FundNode(h, tapdHarness.LND)
		client.SyncUniverse()

		harness.SendAssetHelper(
			t, h, operatorClient, client, assetID, clientShare,
		)
	}
	logClientAssetBalances(
		t, "initial client balances", assetID, clientAliases, clients,
	)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	chainParams := address.ParamsForChain(
		chaincfg.RegressionNetParams.Name,
	)

	leafSpecs := make([]tree.LeafDescriptor, 0, len(clientAliases))
	clientContexts := make([]*clientTreeContext, 0, len(clientAliases))
	cosignerNames := make(map[string]string)

	for i, alias := range clientAliases {
		leaf, userKey := buildClientLeafDescriptor(
			t, h, operatorClient, clientHarnesses[i], clients[i],
			alias, assetID, clientShare, operatorKey, &chainParams,
		)

		clientCtx := &clientTreeContext{
			alias:       alias,
			tapClient:   clients[i],
			tapdHarness: clientHarnesses[i],
			userKey:     userKey,
			operatorKey: operatorKey,
			leaf:        leaf,
		}

		leafSpecs = append(leafSpecs, leaf)
		clientContexts = append(clientContexts, clientCtx)
		cosignerNames[hexKey(userKey.PubKey())] = alias
	}

	vtxos := make([]tree.VTXODescriptor, 0, len(leafSpecs))
	for _, leaf := range leafSpecs {
		vtxos = append(vtxos, tree.VTXODescriptor{
			PkScript:    leaf.PkScript,
			Amount:      leaf.Amount,
			CoSignerKey: leaf.CoSignerKey,
		})
	}

	// Ensure the operator wallet has spendable BTC for CPFP children.
	harness.FundNode(h, h.LND)

	t.Log("Constructing the asset tree")
	// Build a single anchor from onboarding proofs, then assemble the full
	// asset-aware tree using the AssetTreeAssembler so each node carries
	// the proper anchor plan/proof metadata. Tree nodes are always zero-fee
	// since they use ephemeral anchors (fees paid via CPFP child txs).
	assemblerCfg := tree.AssetTreeConfig{
		AssetID:     assetID,
		CSVDelay:    csvDelayBlocks,
		OperatorKey: operatorKey.PubKey(),
		ChainParams: &chainParams,
	}
	assembler := tree.NewAssetTreeAssembler(
		assemblerCfg, operatorClient.AssetWalletClient,
	)

	userPrivs := make([]*btcec.PrivateKey, len(clientContexts))
	for i, ctx := range clientContexts {
		userPrivs[i] = ctx.userKey
	}

	// Import onboarding proofs into the operator's tapd universe BEFORE
	// building the batch anchor. This is required so that tapd can validate
	// the input proofs during CommitVirtualPsbts and generate correct
	// output proofs via PublishAndLogTransfer.
	//
	// The onboarding addresses use a script key with a non-default tweak
	// (script path), which tapd won't recognize automatically in a separate
	// node. We must explicitly declare this script key in the operator's
	// wallet so proof import/validation can succeed.
	t.Log("Declaring onboarding script key in operator's tapd wallet")
	onboardingArtifacts, err := assets.BuildOpTrueArtifacts(
		tapasset.NUMSPubKey,
	)
	require.NoError(t, err)

	onboardingScriptKey := onboardingArtifacts.ScriptKey
	onboardingScriptKey.Type = tapasset.ScriptKeyScriptPathExternal
	onboardingScriptKey.TweakedScriptKey.Type =
		tapasset.ScriptKeyScriptPathExternal
	onboardingScriptKey.TweakedScriptKey.Tweak =
		tapasset.NUMSPubKey.SerializeCompressed()

	scriptKey := rpcutils.MarshalScriptKey(onboardingScriptKey)
	_, err = operatorClient.TapdClient.DeclareScriptKey(
		t.Context(), &assetwalletrpc.DeclareScriptKeyRequest{
			ScriptKey: scriptKey,
		},
	)
	require.NoError(t, err)

	t.Log("Importing onboarding proofs into operator's tapd")
	for i, leaf := range leafSpecs {
		_, importErr := operatorClient.TapdClient.ImportProofFile(
			t.Context(), leaf.Asset.InputProof,
		)
		require.NoError(t, importErr)
		t.Logf("Imported onboarding proof %d to operator's tapd", i)
	}

	rootPlan, rootOutpoint, rootOutput, anchorTx,
		builder, _, err := buildBoardingBatch(
		t, h, leafSpecs, assetID, operatorKey, &chainParams,
		operatorClient, userPrivs,
	)
	require.NoError(t, err)

	t.Logf("Batch tx has %d inputs, %d outputs", len(anchorTx.TxIn),
		len(anchorTx.TxOut))
	for i, out := range anchorTx.TxOut {
		t.Logf("Batch output %d: value=%d sats", i, out.Value)
	}
	t.Logf("Tree root outpoint: %s, root output value: %d", rootOutpoint,
		rootOutput.Value)

	// Broadcast via package relay (anchor + CPFP).
	t.Logf("Broadcasting anchor tx (%s) via package relay",
		anchorTx.TxHash())
	block, blockHeight, batchTxIndex := publishPackage(t, h, anchorTx)

	// Use the builder's Proof() method to get a complete proof file with
	// confirmation data. This handles PrevWitness updates and V1 witness
	// population automatically.
	batchOutIndex := uint32(0)
	rootProof, err := builder.Proof(
		int(batchOutIndex), &assets.ProofParams{
			Block:       block,
			BlockHeight: blockHeight,
			TxIndex:     int(batchTxIndex),
		},
	)
	require.NoError(t, err)
	t.Logf("Got root proof from builder.Proof() (%d bytes)", len(rootProof))

	rootProofFile, err := proof.DecodeFile(rootProof)
	require.NoError(t, err)
	rootProofEntry, err := rootProofFile.LastProof()
	require.NoError(t, err)

	// Compute the internal key for the tree root. This is the MuSig2
	// aggregate of operator + all client cosigners.
	rootCosigners := make([]*btcec.PublicKey, 0, len(leafSpecs)+1)
	rootCosigners = append(rootCosigners, operatorKey.PubKey())
	for _, leaf := range leafSpecs {
		rootCosigners = append(rootCosigners, leaf.CoSignerKey)
	}
	batchInternalKey, err := tree.ComputeInternalKey(rootCosigners)
	require.NoError(t, err)

	// Sanity check: verify that the proof's internal key matches the
	// computed internal key.
	proofInternalKey := rootProofEntry.InclusionProof.InternalKey
	require.Equal(
		t, schnorr.SerializePubKey(proofInternalKey),
		schnorr.SerializePubKey(batchInternalKey),
	)

	radix := 2
	assetTree, assetCtx, err := assembler.BuildTree(
		t.Context(), rootOutpoint, rootPlan, rootProof, rootOutput,
		leafSpecs, radix,
	)
	require.NoError(t, err)

	// Create tweak lookup from the asset context for signing sessions.
	tweakLookup := tree.TweakLookupFromAssetContext(assetCtx)

	t.Log("creating muSig signing sessions for operator and clients")
	opSigner := assets.NewLocalMuSig2Signer(operatorKey)
	signerSessions := make([]*tree.SignerSession, 0, len(clientContexts)+1)
	operatorSession, err := assetTree.NewTreeSignerSession(
		opSigner, &keychain.KeyDescriptor{
			PubKey: operatorKey.PubKey(),
		}, tweakLookup,
	)
	require.NoError(t, err)
	signerSessions = append(signerSessions, operatorSession)

	for _, ctx := range clientContexts {
		ctx.signer = assets.NewLocalMuSig2Signer(ctx.userKey)
		sess, err := assetTree.NewTreeSignerSession(
			ctx.signer, &keychain.KeyDescriptor{
				PubKey: ctx.userKey.PubKey(),
			}, tweakLookup,
		)
		require.NoError(t, err)
		ctx.treeSigner = sess
		signerSessions = append(signerSessions, sess)
	}

	t.Log("Running musig2 tree signing round")
	finalSigs := runTreeSigning(
		t, assetTree, opSigner, operatorSession, signerSessions[1:],
	)
	require.NoError(t, assetTree.SubmitTreeSigs(finalSigs))
	require.NoError(t, assetTree.VerifySigned())

	cosignerNames[hexKey(operatorKey.PubKey())] = "operator"
	t.Logf("\n%s", renderTree(assetTree.Root, assetCtx, cosignerNames))

	// Import the root proof to the universe BEFORE publishing tree nodes.
	// This is critical: child nodes need to look up the parent proof to
	// validate their own proofs. Without this import, they will fail with
	// "no universe proof found".
	//
	// The batch root uses an OpTrueUniqueScript script key derived from the
	// MuSig2 internal key. Since this is a non-wallet-derived key with a
	// script path tweak, we must declare it to the operator's wallet before
	// importing the proof.
	t.Log("Declaring root script key in operator's tapd wallet")
	rootScriptArtifacts, err := assets.BuildOpTrueArtifacts(
		batchInternalKey,
	)
	require.NoError(t, err)

	rootScriptKey := rootScriptArtifacts.ScriptKey
	rootScriptKey.Type = tapasset.ScriptKeyScriptPathExternal
	rootScriptKey.TweakedScriptKey.Type =
		tapasset.ScriptKeyScriptPathExternal

	_, err = operatorClient.TapdClient.DeclareScriptKey(
		t.Context(), &assetwalletrpc.DeclareScriptKeyRequest{
			ScriptKey: rpcutils.MarshalScriptKey(rootScriptKey),
		},
	)
	require.NoError(t, err)

	t.Log("Importing root proof to universe for proof chain continuity")
	_, rootImportErr := operatorClient.TapdClient.ImportProofFile(
		t.Context(), rootProof,
	)
	require.NoError(t, rootImportErr, "failed to import root proof")

	// Publish tree nodes using the Finalize workflow. This rebuilds the
	// builder, applies signatures, broadcasts via package relay, then uses
	// builder.Proof() after confirmation to get full proofs.
	publishTreeWithPackages(
		t, h, assetTree, assetCtx, operatorClient.TapdClient, rootProof,
		operatorKey.PubKey(), assetID, &chainParams,
	)

	t.Log("Tree publishing complete - all transactions accepted!")

	// Log balances before sweep.
	logClientAssetBalances(
		t, "before sweep", assetID, clientAliases, clients,
	)

	// Perform cooperative sweep for each leaf using script path spend.
	// Both client and operator sign, allowing immediate spending without
	// CSV delay. The VTXOs use ARKNUMSKey as internal key, so script path
	// spending is required (keyspend not possible with NUMS internal key).
	//
	// Note: We match clients to leaves by checking if the client's userKey
	// is in the leaf's CoSigners list, since GetLeafNodes() may return
	// leaves in a different order than clientContexts was built.
	leafNodes := assetTree.Root.GetLeafNodes()
	for _, leaf := range leafNodes {
		// Sanity check leaf metadata via assetCtx.
		leafState := assetCtx.Get(leaf)
		require.NotNil(t, leafState)
		require.NotNil(t, leafState.Leaf)
		require.NotEmpty(t, leafState.AssetProof)

		// Find the client context that matches this leaf's cosigner.
		var ctx *clientTreeContext
		for _, c := range clientContexts {
			userPubKey := c.userKey.PubKey()
			if tree.ContainsCosigner(leaf.CoSigners, userPubKey) {
				ctx = c
				break
			}
		}
		require.NotNilf(t, ctx, "no client context for leaf cosigners")

		t.Logf("Sweeping assets for client %s", ctx.alias)
		sweepAssetWithBuilder(
			t, h, ctx, leaf, assetCtx, assetID, &chainParams,
		)
	}

	// Mine a block for any sweep transactions.
	h.GenerateAndWait(1)

	// Log balances after sweep.
	logClientAssetBalances(
		t, "after sweep", assetID, clientAliases, clients,
	)

	// Verify final balances are back to starting values.
	for i, client := range clients {
		ctxBalance, cancel := context.WithTimeout(
			t.Context(), assetTreeTimeout,
		)
		defer cancel()

		balance, err := client.TapdClient.GetAssetBalance(
			ctxBalance, assetID[:],
		)

		require.NoError(t, err)
		require.Equal(
			t, clientShare, balance,
			"client %s should have balance restored to %d",
			clientAliases[i], clientShare,
		)
	}

	t.Log("All client balances restored to starting values!")
}

func buildClientLeafDescriptor(t *testing.T, h *harness.Harness,
	operatorClient *harness.TapClientHarness,
	clientHarness *harness.TapdHarness,
	client *harness.TapClientHarness, alias string,
	assetID tapasset.ID, amount uint64,
	operatorKey *btcec.PrivateKey,
	chainParams *address.ChainParams) (tree.LeafDescriptor,
	*btcec.PrivateKey) {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), assetTreeTimeout)
	defer cancel()

	userKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	kit, err := assets.NewScriptOnlyOnboardingKit(
		userKey.PubKey(), operatorKey.PubKey(), keychain.KeyLocator{},
		assetID, amount, csvDelayBlocks, chainParams,
	)
	require.NoError(t, err)

	onboardingAddr, err := kit.NewOnboardingAddr(ctx, client.TapdClient)
	require.NoError(t, err)

	client.SendAsset(onboardingAddr.Encoded)
	h.GenerateAndWait(2)

	matcher := func(t *testing.T, transfers []*taprpc.AssetTransfer) (
		*taprpc.AssetTransfer, int) {

		transfer, idx, err := kit.FindMatchingTransfer(transfers)
		if err != nil {
			return nil, -1
		}

		return transfer, idx
	}

	transfer, outIdx := client.WaitForTransfer(matcher)
	output := transfer.Outputs[outIdx]

	outpointParts := strings.Split(output.Anchor.Outpoint, ":")
	require.Len(t, outpointParts, 2)

	var anchorOutputIndex uint32
	_, err = fmt.Sscanf(outpointParts[1], "%d", &anchorOutputIndex)
	require.NoError(t, err)

	var proofFile *taprpc.ProofFile
	require.Eventually(t, func() bool {
		resp, err := client.ExportProof(ctx, &taprpc.ExportProofRequest{
			AssetId:   output.AssetId,
			ScriptKey: output.ScriptKey,
			Outpoint: &taprpc.OutPoint{
				Txid:        transfer.AnchorTxHash,
				OutputIndex: anchorOutputIndex,
			},
		})
		if err != nil {
			return false
		}

		proofFile = resp

		return true
	}, assetTreeTimeout, time.Second)

	builder := assets.NewAssetTxBuilder(assetID, chainParams)

	// Script-only onboarding uses NUMS internal key with two tapscript
	// closures: cooperative (CHECKSIGADD) and CSV timeout.
	coopClosure := (&assets.CheckSigAddClosure{
		Key1: userKey.PubKey(),
		Key2: operatorKey.PubKey(),
	}).ScriptClosure()
	coopClosure.ID = "coop_multisig"

	csvClosure := (&assets.CSVClosure{
		Key:   userKey.PubKey(),
		Delay: csvDelayBlocks,
	}).ScriptClosure()
	csvClosure.ID = "csv"

	require.NoError(t, builder.AddAssetInput(assets.InputConfig{
		ProofFile: proofFile.RawProofFile,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(&scripts.ARKNUMSKey),
		},
		Closures: []assets.ScriptClosure{coopClosure, csvClosure},
	}))

	require.NoError(t, builder.AddAssetOutput(assets.OutputConfig{
		Amount: amount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(operatorKey.PubKey()),
		},
		Script: assets.OpTrueUniqueScript(operatorKey.PubKey()),
	}))

	_, err = builder.Compile(ctx)
	require.NoError(t, err)

	// Produce the VTXO script for the leaf.
	vtxoKey, err := scripts.VTXOTapKey(
		userKey.PubKey(), operatorKey.PubKey(), csvDelayBlocks,
	)
	require.NoError(t, err)

	vtxoPkScript, err := txscript.PayToTaprootScript(vtxoKey)
	require.NoError(t, err)

	leafProof := proofFile.RawProofFile

	// Use proper BTC amounts for tree leaves. We inject BTC via a manual
	// input in buildSingleAnchorFromOnboarding, so we have sufficient
	// funds.
	const leafBtcSats = 10000

	leaf := tree.NewAssetLeafDescriptor(
		vtxoPkScript, btcutil.Amount(leafBtcSats), userKey.PubKey(),
		leafProof, amount, btcutil.Amount(leafBtcSats),
		[]byte(alias+"-change"),
	)

	return leaf, userKey
}

func runTreeSigning(t *testing.T, assetTree *tree.Tree,
	opSigner *assets.LocalMuSig2Signer, operatorSession *tree.SignerSession,
	clientSessions []*tree.SignerSession) map[tree.TxID]*schnorr.Signature {

	t.Helper()

	allSessions := append(
		[]*tree.SignerSession{operatorSession}, clientSessions...,
	)

	// Build a lookup of required cosigner order per transaction ID to avoid
	// any map iteration nondeterminism.
	cosignerOrder := make(map[tree.TxID][]string)
	nodesByTx := make(map[tree.TxID]*tree.Node)
	err := assetTree.Root.ForEach(func(n *tree.Node) error {
		tx, err := n.ToTx()
		if err != nil {
			return err
		}

		txid := tx.TxHash()
		nodesByTx[txid] = n
		for _, cosigner := range n.CoSigners {
			cosignerOrder[txid] = append(
				cosignerOrder[txid], hexKey(cosigner),
			)
		}

		// Keys are aggregated with WithSortedKeys, so ensure we follow
		// the same deterministic ordering when registering nonces/sigs.
		sort.Strings(cosignerOrder[txid])

		return nil
	})
	require.NoError(t, err)

	sessionByKey := make(map[string]*tree.SignerSession)
	noncesBySession := make(
		map[string]map[tree.TxID]tree.Musig2PubNonce,
	)
	for _, sess := range allSessions {
		key := hexKey(sess.PubKey())
		sessionByKey[key] = sess

		nonces := sess.GetNonces()
		noncesBySession[key] = nonces
	}

	// Collect nonces per transaction and aggregate them.
	aggNonceSet := make(map[tree.TxID]tree.Musig2PubNonce)
	for txid, order := range cosignerOrder {
		nonces := make([][66]byte, 0, len(order))
		for _, key := range order {
			nonce, ok := noncesBySession[key][txid]
			require.Truef(t, ok, "missing nonce for %s", key)
			nonces = append(nonces, nonce)
		}

		aggNonce, err := musig2.AggregateNonces(nonces)
		require.NoError(t, err)
		aggNonceSet[txid] = aggNonce
	}

	for _, sess := range allSessions {
		require.NoError(t, sess.RegisterAggNonces(aggNonceSet))
	}

	sigsBySession := make(
		map[string]map[tree.TxID]*musig2.PartialSignature,
	)
	for _, sess := range allSessions {
		partialSigs, sigErr := sess.Signatures(false)
		require.NoError(t, sigErr)
		sigsBySession[hexKey(sess.PubKey())] = partialSigs
	}

	operatorKeyHex := hexKey(operatorSession.PubKey())
	partials := make(map[tree.TxID][]*musig2.PartialSignature)
	for txid, order := range cosignerOrder {
		for _, key := range order {
			if key == operatorKeyHex {
				continue
			}

			if sig, ok := sigsBySession[key][txid]; ok {
				partials[txid] = append(partials[txid], sig)
			}
		}
	}

	finalSigs := make(map[tree.TxID]*schnorr.Signature)
	opIDs := operatorSession.SessionIDs()
	require.NotNil(t, opSigner)

	for txid, sigs := range partials {
		finalSig, haveAll, err := opSigner.MuSig2CombineSig(
			opIDs[txid], sigs,
		)
		require.NoError(t, err)
		require.True(t, haveAll)
		finalSigs[txid] = finalSig
	}

	return finalSigs
}

func renderTree(node *tree.Node, assetCtx *tree.AssetContext,
	names map[string]string) string {

	if node == nil {
		return ""
	}

	var buf strings.Builder
	renderTreeNode(&buf, node, assetCtx, "", true, true, names)

	return buf.String()
}

func renderTreeNode(buf *strings.Builder, node *tree.Node,
	assetCtx *tree.AssetContext, prefix string, isLast bool, isRoot bool,
	labelMap map[string]string) {

	if node == nil {
		return
	}

	txid, _ := node.TXID()

	// Determine node type for the label.
	nodeType := "branch"
	if node.IsLeaf() {
		nodeType = "leaf"
	} else if isRoot {
		nodeType = "root"
	}

	// Print connector and node header.
	if isRoot {
		fmt.Fprintf(buf, "[%s] tx: %s\n", nodeType, txid)
	} else {
		connector := "├── "
		if isLast {
			connector = "└── "
		}
		fmt.Fprintf(buf, "%s%s[%s] tx: %s\n",
			prefix, connector, nodeType, txid)
	}

	// Build prefix for content lines.
	var (
		contentPrefix string
		childPrefix   string
	)

	switch {
	case isRoot:
		contentPrefix = "    "
		childPrefix = contentPrefix
	case isLast:
		contentPrefix = prefix + "    "
		childPrefix = prefix + "    "
	default:
		contentPrefix = prefix + "│   "
		childPrefix = prefix + "│   "
	}

	// Print cosigners.
	if len(node.CoSigners) > 0 {
		names := make([]string, 0, len(node.CoSigners))
		for _, pk := range node.CoSigners {
			names = append(names, identifyCosigner(pk, labelMap))
		}
		namesStr := strings.Join(names, ", ")
		fmt.Fprintf(buf, "%scosigners: %d (%s)\n",
			contentPrefix, len(node.CoSigners), namesStr)
	}

	// Print BTC funding for all nodes.
	leafMeta := assetCtx.GetLeaf(node)
	if leafMeta != nil {
		// Leaf: show individual funding.
		fmt.Fprintf(buf, "%sbtc: %v\n",
			contentPrefix, leafMeta.Funding)
	} else {
		subtreeFunding := tree.TotalBTCFunding(node, assetCtx)
		if subtreeFunding > 0 {
			// Branch/root: show aggregated funding.
			fmt.Fprintf(buf, "%sbtc (subtree): %v\n",
				contentPrefix, subtreeFunding)
		}
	}

	// Print leaf-specific info.
	if leafMeta != nil {
		fmt.Fprintf(buf, "%sasset amount: %d units\n",
			contentPrefix, leafMeta.AssetAmount)
	}

	// Calculate child prefix for recursion.
	if len(node.Children) == 0 {
		return
	}

	childKeys := make([]uint32, 0, len(node.Children))
	for outputIdx := range node.Children {
		childKeys = append(childKeys, outputIdx)
	}
	sort.Slice(childKeys, func(i, j int) bool {
		return childKeys[i] < childKeys[j]
	})

	for i, outputIdx := range childKeys {
		child := node.Children[outputIdx]
		childIsLast := i == len(childKeys)-1
		renderTreeNode(buf, child, assetCtx, childPrefix, childIsLast,
			false, labelMap)
	}
}

func identifyCosigner(pk *btcec.PublicKey, names map[string]string) string {
	if pk == nil {
		return "unknown"
	}

	key := hexKey(pk)
	if label, ok := names[key]; ok {
		return label
	}

	return fmt.Sprintf("%x", pk.SerializeCompressed()[:4])
}

func hexKey(pk *btcec.PublicKey) string {
	if pk == nil {
		return ""
	}

	return hex.EncodeToString(pk.SerializeCompressed())
}

// declareScriptKeyFromProof ensures the importing tapd wallet knows how to
// interpret the script key referenced by a proof file.
//
// Why this is needed: tapd requires non-wallet-derived script keys (for example
// OP_TRUE script path keys) to be explicitly declared via DeclareScriptKey
// before proofs that reference them can be imported/validated.
func declareScriptKeyFromProof(t *testing.T, ctx context.Context,
	tapdClient *assets.TapdClient, proofBlob []byte) {

	t.Helper()

	require.NotNil(t, tapdClient)
	require.NotEmpty(t, proofBlob)

	proofFile, err := proof.DecodeFile(proofBlob)
	require.NoError(t, err)

	lastProof, err := proofFile.LastProof()
	require.NoError(t, err)

	var scriptKey tapasset.ScriptKey

	// The proofs we import in this test are for OP_TRUE-based anchors. For
	// those, we can deterministically reconstruct a fully-populated script
	// key (including tweak + raw key) from the taproot internal key found
	// in the inclusion proof. This avoids relying on optional/omitted
	// script key internals in the proof encoding.
	internalKey := lastProof.InclusionProof.InternalKey
	if internalKey != nil {
		opTrue, err := assets.BuildOpTrueArtifacts(internalKey)
		require.NoError(t, err)
		scriptKey = opTrue.ScriptKey
	} else {
		scriptKey = lastProof.Asset.ScriptKey
	}

	require.NotNil(t, scriptKey.PubKey)
	require.NotNil(t, scriptKey.TweakedScriptKey)
	require.NotEmpty(t, scriptKey.TweakedScriptKey.Tweak)

	scriptKey.Type = tapasset.ScriptKeyScriptPathExternal
	scriptKey.TweakedScriptKey.Type = tapasset.ScriptKeyScriptPathExternal

	_, err = tapdClient.DeclareScriptKey(
		ctx, &assetwalletrpc.DeclareScriptKeyRequest{
			ScriptKey: rpcutils.MarshalScriptKey(scriptKey),
		},
	)
	if err != nil && !strings.Contains(err.Error(), "already") {
		require.NoError(t, err)
	}
}

// sweepAssetWithBuilder performs a cooperative 2-of-2 script path spend to
// sweep the leaf VTXO assets to the client's wallet. Both client (owner) and
// operator (cosigner) sign the transaction cooperatively using the
// CHECKSIGVERIFY + CHECKSIG script path.
//
// The VTXO leaf structure matches the pure BTC design:
//   - Internal key: NUMS (no keyspend possible)
//   - Leaf 0 (Collaborative): <owner> CHECKSIGVERIFY <cosigner> CHECKSIG
//   - Leaf 1 (Timeout): <owner> CHECKSIG <delay> CSV DROP
func sweepAssetWithBuilder(t *testing.T, h *harness.Harness,
	ctx *clientTreeContext, leaf *tree.Node, assetCtx *tree.AssetContext,
	assetID tapasset.ID, params *address.ChainParams) {

	t.Helper()

	leafState := assetCtx.Get(leaf)
	require.NotNil(t, leafState)
	require.NotNil(t, leafState.Leaf)
	require.NotEmpty(t, leafState.AssetProof)

	builder := assets.NewAssetTxBuilder(assetID, params)
	leafProof := leafState.AssetProof

	// Reconstruct anchor key and closures from known VTXO structure.
	// The anchor key is scripts.ARKNUMSKey (a NUMS point for script-only
	// spend). The tree leaf outputs use the VTXO script structure created
	// via scripts.VTXOTapKey which has CollabMultisig (CHECKSIGVERIFY +
	// CHECKSIG) and VTXOTimeout closures.
	anchorKey := assets.AnchorKeySpec{
		Mode: assets.AnchorKeyModeStatic,
		Key:  schnorr.SerializePubKey(&scripts.ARKNUMSKey),
	}

	// Reconstruct closures matching the VTXO script structure from
	// scripts.VTXOTapScript:
	// - CollabMultisig: owner + cosigner (CHECKSIGVERIFY + CHECKSIG)
	// - VTXOTimeout: owner + CSV delay (CHECKSIG + CSV + DROP)
	coopClosure := (&assets.CollabMultisigClosure{
		OwnerKey:    ctx.userKey.PubKey(),
		CosignerKey: ctx.operatorKey.PubKey(),
	}).ScriptClosure()

	csvClosure := (&assets.VTXOTimeoutClosure{
		Key:   ctx.userKey.PubKey(),
		Delay: csvDelayBlocks,
	}).ScriptClosure()

	closures := []assets.ScriptClosure{coopClosure, csvClosure}

	ctxSweep, cancel := context.WithTimeout(t.Context(), assetTreeTimeout)
	defer cancel()

	// Sync the client's universe with the operator before sweep so it has
	// the full proof chain including the issuance proof.
	ctx.tapClient.SyncUniverse()

	// Import the proof to the client's tapd so it knows about the input.
	// The proof was generated during tree publishing but wasn't imported
	// to the client's tapd.
	declareScriptKeyFromProof(
		t, ctxSweep, ctx.tapClient.TapdClient, leafProof,
	)
	_, importErr := ctx.tapClient.TapdClient.ImportProofFile(
		ctxSweep, leafProof)
	if importErr != nil {
		t.Logf("Failed to import input proof for sweep: %v", importErr)
	} else {
		t.Logf("Imported input proof for sweep")
	}

	// For MuSig2 keyspend, no sequence restriction is needed (unlike CSV).
	require.NoError(t, builder.AddAssetInput(assets.InputConfig{
		ProofFile: leafProof,
		AnchorKey: anchorKey,
		Closures:  closures,
	}))

	destScriptKey, destInternalKeyDesc, err := ctx.tapClient.
		DeriveNewKeys(ctxSweep)
	require.NoError(t, err)

	// Use DirectWalletScript so the wallet recognizes the output as its
	// own and tracks the balance.
	destPubKey := destInternalKeyDesc.PubKey
	destInternalKeyBytes := schnorr.SerializePubKey(destPubKey)
	require.NoError(t, builder.AddAssetOutput(assets.OutputConfig{
		Amount: clientShare,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  destInternalKeyBytes,
		},
		Script: assets.DirectWalletScript(&destScriptKey),
	}))

	_, err = builder.Compile(ctxSweep)
	require.NoError(t, err)

	require.NoError(t, builder.Commit(
		ctxSweep, ctx.tapClient.AssetWalletClient,
		assets.CommitOptions{FeeRate: chainfee.SatPerVByte(10)},
	))

	// Use the CollabMultisig script path spend. The VTXOs in this test use
	// ARKNUMSKey as the internal key (a NUMS point), so keyspend is not
	// possible. Both user (owner) and operator (cosigner) sign the script
	// path using CHECKSIGVERIFY + CHECKSIG.
	var collabClosureID string
	for _, c := range closures {
		if c.ID == "collab_multisig" {
			collabClosureID = c.ID
			break
		}
	}
	require.NotEmpty(t, collabClosureID,
		"collab_multisig closure not found")

	scriptSpend, err := builder.PrepareScriptSpend(0, collabClosureID)
	require.NoError(t, err)

	// Both owner (user) and cosigner (operator) sign for CHECKSIGVERIFY +
	// CHECKSIG. The owner's signature is verified first (CHECKSIGVERIFY),
	// then the cosigner's (CHECKSIG).
	ownerSig, err := schnorr.Sign(ctx.userKey, scriptSpend.SigHash[:])
	require.NoError(t, err)

	sigHash := scriptSpend.SigHash[:]
	cosignerSig, err := schnorr.Sign(ctx.operatorKey, sigHash)
	require.NoError(t, err)

	ownerKeyHex := hex.EncodeToString(
		schnorr.SerializePubKey(ctx.userKey.PubKey()),
	)
	cosignerKeyHex := hex.EncodeToString(
		schnorr.SerializePubKey(ctx.operatorKey.PubKey()),
	)

	require.NoError(t, builder.ApplyScriptSpend(
		scriptSpend, map[string][]byte{
			ownerKeyHex:    ownerSig.Serialize(),
			cosignerKeyHex: cosignerSig.Serialize(),
		},
	))

	_, err = builder.FinalizeAnchor(ctxSweep, ctx.tapdHarness.LND.WalletKit)
	require.NoError(t, err)

	resp, err := builder.Publish(
		ctxSweep, ctx.tapClient.TapdClient,
		fmt.Sprintf("ark-exit-%s", ctx.alias), assets.PublishOptions{},
	)
	require.NoError(t, err)
	t.Logf("Sweep tx published: %x", resp.Transfer.AnchorTxHash)

	// Mine the sweep transaction and get block info for proof construction.
	minedBlocks := h.GenerateAndWait(1)
	require.Len(t, minedBlocks, 1)
	minedBlock := minedBlocks[0]
	t.Logf("Mined block: %s (height=%d)",
		minedBlock.Header.Hash, minedBlock.Header.Height)

	// Get the block for proof construction.
	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	defer rpcClient.Shutdown()

	blockHash, err := chainhash.NewHashFromStr(minedBlock.Header.Hash)
	require.NoError(t, err)
	rawBlock, err := rpcClient.GetBlock(blockHash)
	require.NoError(t, err)

	// Find the sweep tx index in the block.
	anchorTxHash, err := chainhash.NewHash(resp.Transfer.AnchorTxHash)
	require.NoError(t, err)
	txIndex := -1
	for i, tx := range rawBlock.Transactions {
		if tx.TxHash() == *anchorTxHash {
			txIndex = i
			break
		}
	}
	require.GreaterOrEqual(t, txIndex, 0, "sweep tx not found in block")

	// Build the proof using the builder's Proof() method with block
	// confirmation data. This produces a correct proof file that can be
	// imported and verified.
	sweepProof, err := builder.Proof(0, &assets.ProofParams{
		Block:       rawBlock,
		BlockHeight: uint32(minedBlock.Header.Height),
		TxIndex:     txIndex,
	})
	require.NoError(t, err)
	t.Logf("Built sweep proof from Proof() (%d bytes)", len(sweepProof))

	_, err = ctx.tapClient.ImportProofFile(ctxSweep, sweepProof)
	require.NoError(t, err)
	t.Logf("Imported sweep proof to universe")

	// The wallet doesn't automatically recognize externally-built
	// transfers. We need to call RegisterTransfer to inform the daemon
	// about the incoming asset so it appears in the wallet balance.
	require.NotEmpty(t, resp.Transfer.Outputs)

	// Find the output that matches our destination script key.
	destScriptKeyBytes := destScriptKey.PubKey.SerializeCompressed()
	var sweepOutput *taprpc.TransferOutput
	for _, out := range resp.Transfer.Outputs {
		if bytes.Equal(out.ScriptKey, destScriptKeyBytes) {
			sweepOutput = out
			break
		}
	}
	require.NotNil(t, sweepOutput, "sweep output not found in transfer")
	t.Logf("Found sweep output at %s with script key %x",
		sweepOutput.Anchor.Outpoint, sweepOutput.ScriptKey)

	// Parse the outpoint string "txid:index" to extract components.
	outpointParts := strings.Split(sweepOutput.Anchor.Outpoint, ":")
	require.Len(t, outpointParts, 2, "invalid outpoint format")
	outputIndex, err := strconv.ParseUint(outpointParts[1], 10, 32)
	require.NoError(t, err)

	// Register the transfer so the wallet recognizes the incoming asset.
	registerResp, err := ctx.tapClient.RegisterTransfer(
		ctxSweep, &taprpc.RegisterTransferRequest{
			AssetId:   assetID[:],
			ScriptKey: destScriptKeyBytes,
			Outpoint: &taprpc.OutPoint{
				Txid:        resp.Transfer.AnchorTxHash,
				OutputIndex: uint32(outputIndex),
			},
		},
	)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		require.NoError(t, err)
	}
	if err == nil {
		t.Logf("Registered transfer, asset amount: %d",
			registerResp.RegisteredAsset.Amount)
	} else {
		t.Logf("RegisterTransfer returned: %v", err)
	}

	t.Logf("Waiting for balance update")
	expectedAssetAmt := clientShare
	if leafState.Leaf != nil && leafState.Leaf.AssetAmount > 0 {
		expectedAssetAmt = leafState.Leaf.AssetAmount
	}
	ctx.tapClient.WaitForAssetBalance(assetID[:], expectedAssetAmt)
}

// publishTreeWithPackages walks the signed tree top-down and uses the Finalize
// workflow to get proofs from tapd, then broadcasts each transaction via
// package relay (parent tx + CPFP child).
//
// The workflow:
// 1. For each node (parent before children):
//   - Call Finalize with SkipBroadcast to log the transfer and get proofs
//   - Broadcast the signed transaction via package relay
//
// The rootInputProof is the proof for the input being spent by the root node
// (typically the aggregated onboarding anchor proof).
func publishTreeWithPackages(t *testing.T, h *harness.Harness,
	assetTree *tree.Tree, assetCtx *tree.AssetContext,
	tapClient *assets.TapdClient, rootInputProof []byte,
	operatorKey *btcec.PublicKey, assetID tapasset.ID,
	chainParams *address.ChainParams) {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	// Track proofs by outpoint so children can look up their input proofs.
	// Key format: "txid:index"
	proofsByOutpoint := make(map[string][]byte)

	// Store the root input proof at the root's input outpoint.
	rootInputKey := fmt.Sprintf("%s:%d",
		assetTree.Root.Input.Hash, assetTree.Root.Input.Index)
	proofsByOutpoint[rootInputKey] = rootInputProof
	t.Logf("Stored root input proof at %s", rootInputKey)

	// Gather nodes in pre-order so parents are published before children.
	var nodes []*tree.Node
	err := assetTree.Root.ForEach(func(n *tree.Node) error {
		nodes = append(nodes, n)
		return nil
	})
	require.NoError(t, err)

	// Build tree config for finalization and create the materializer.
	treeCfg := tree.AssetTreeConfig{
		AssetID:     assetID,
		CSVDelay:    csvDelayBlocks,
		OperatorKey: operatorKey,
		ChainParams: chainParams,
	}
	materializer := tree.NewAssetMaterializer(
		treeCfg, tapClient.AssetWalletClient, assetCtx,
	)

	for _, n := range nodes {
		txid, err := n.TXID()
		require.NoError(t, err)

		t.Logf("Finalizing tree node tx=%s", txid)

		// Determine parent proof: root uses rootInputProof, children
		// use parent's output proof keyed by their input outpoint.
		inputKey := n.Input.String()
		parentProof := proofsByOutpoint[inputKey]
		require.NotEmptyf(t, parentProof,
			"no parent proof for input %s", inputKey)

		// Finalize the node to log the transfer and get proofs from
		// tapd. We use SkipBroadcast since we broadcast via package
		// relay.
		result, finalizeErr := materializer.FinalizeAssetNode(
			ctx, n, parentProof,
		)
		require.NoError(t, finalizeErr, "finalize node %s", txid)

		// Debug: log input/output sums for the finalized anchor tx.
		var outputSum int64
		for _, out := range result.AnchorTx.TxOut {
			outputSum += out.Value
		}
		t.Logf("Finalize tx=%s outputs=%d sum_out=%d",
			result.AnchorTx.TxHash(), len(result.AnchorTx.TxOut),
			outputSum)

		// Broadcast the anchor transaction via package relay FIRST to
		// get block info for updating proofs.
		t.Logf("Broadcasting tree node tx=%s via package relay",
			result.AnchorTx.TxHash())
		minedBlock, blockHeight, txIndex := publishPackage(t, h,
			result.AnchorTx)

		// Use builder.Proof() to generate proofs with correct OP_TRUE
		// witnesses. The builder's Proof() method handles the witness
		// derivation properly.
		anchorTxid := result.AnchorTx.TxHash().String()

		// Determine number of outputs from the builder's active
		// packets.
		activePackets := result.Builder.ActivePackets()
		numOutputs := len(activePackets[0].Outputs)
		anchorIndices := make([]uint32, numOutputs)
		for idx := 0; idx < numOutputs; idx++ {
			// Build proof for this output using the finalized
			// transfer data to avoid witness/state mismatches.
			proofParams := &assets.ProofParams{
				Block:       minedBlock,
				BlockHeight: blockHeight,
				TxIndex:     int(txIndex),
			}
			parentProofs := [][]byte{parentProof}
			transferData := result.TransferData
			proofBlob, proofErr := assets.BuildProofFromTransferData( //nolint:ll
				transferData, parentProofs, idx, proofParams,
			)
			require.NoError(t, proofErr, "build proof %d", idx)

			// Extract the anchor output index from the proof. The
			// virtual output index (idx) may differ from the anchor
			// output index in split transactions. We must use the
			// anchor index when storing so children can look up
			// proofs by their Input outpoint (txid:anchorIdx).
			proofFile, decodeErr := proof.DecodeFile(proofBlob)
			require.NoError(t, decodeErr, "decode proof %d", idx)
			lastProof, lastErr := proofFile.LastProof()
			require.NoError(t, lastErr, "get last proof %d", idx)
			anchorOutputIdx := lastProof.InclusionProof.OutputIndex

			// Store the proof keyed by anchor output index.
			key := fmt.Sprintf("%s:%d", anchorTxid, anchorOutputIdx)
			proofsByOutpoint[key] = proofBlob
			anchorIndices[idx] = anchorOutputIdx
			t.Logf("Stored output proof at %s (%d bytes)", key,
				len(proofBlob))

			// Also update asset context's proof if this is a leaf.
			nodeState := assetCtx.Get(n)
			if nodeState != nil && idx == 0 {
				nodeState.AssetProof = proofBlob
			}
		}

		// Import the proofs to the universe for proof chain continuity.
		for idx := 0; idx < numOutputs; idx++ {
			anchorIdx := anchorIndices[idx]
			key := fmt.Sprintf("%s:%d", anchorTxid, anchorIdx)
			updatedProof := proofsByOutpoint[key]
			if len(updatedProof) == 0 {
				continue
			}

			declareScriptKeyFromProof(
				t, ctx, tapClient, updatedProof,
			)

			_, importErr := tapClient.ImportProofFile(
				ctx, updatedProof,
			)
			if importErr != nil {
				if strings.Contains(
					importErr.Error(), "already exists",
				) {

					continue
				}

				t.Logf("Warning: failed to import "+
					"proof: %v", importErr)
			}
		}
	}
}

// publishPackage broadcasts the parent tx along with a CPFP child and mines
// them in a block. Returns the mined block the block height and the parent tx
// index in the block.
func publishPackage(t *testing.T, h *harness.Harness,
	parentTx *wire.MsgTx) (*wire.MsgBlock, uint32, uint32) {

	t.Helper()

	require.NotNil(t, parentTx)

	childTx := buildCPFPForAnchor(t, h, parentTx)

	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	defer rpcClient.Shutdown()

	btcClient, err := h.BitcoindClient()
	require.NoError(t, err)

	t.Logf("Submitting package: parent=%s, child=%s", parentTx.TxHash(),
		childTx.TxHash())

	res, err := btcClient.SubmitPackage(
		[]*wire.MsgTx{parentTx}, childTx, nil,
	)
	require.NoError(t, err)

	t.Logf("Package result: %d txs", len(res.TxResults))
	for txid, txRes := range res.TxResults {
		if txRes.Error != nil {
			t.Fatalf("Package tx %s rejected: %s (res=%v)", txid,
				*txRes.Error, res.TxResults)
		}

		t.Logf("Package tx %s accepted (resultTx=%v)", txid, txRes.TxID)
	}
	if res.PackageMsg != "" {
		t.Logf("Package msg: %s", res.PackageMsg)
	}

	// Wait for mempool acceptance.
	mempoolTxIDs := h.WaitMempoolTxCount(len(res.TxResults))
	require.Contains(
		t, mempoolTxIDs, parentTx.TxHash().String(),
		"parent tx not in mempool",
	)
	require.Contains(
		t, mempoolTxIDs, childTx.TxHash().String(),
		"child tx not in mempool",
	)

	blocks := h.GenerateAndWait(1)
	minedBlock := blocks[0]

	t.Logf("Package mined in block %s at height %d",
		minedBlock.Header.Hash, minedBlock.Header.Height)

	// Get the raw block for confirmation data.
	blockHash, err := chainhash.NewHashFromStr(
		minedBlock.Header.Hash,
	)
	require.NoError(t, err)

	rawBlock, err := rpcClient.GetBlock(blockHash)
	rpcClient.Shutdown()
	require.NoError(t, err)

	// Find the tx index in the block.
	txIndex := -1
	for i, blockTx := range rawBlock.Transactions {
		if blockTx.TxHash() == parentTx.TxHash() {
			txIndex = i
			break
		}
	}
	require.GreaterOrEqual(
		t, txIndex, 0, "parent tx not found in mined block",
	)

	t.Logf("Package tx=%s included in block %d (hash %s) at index %d",
		parentTx.TxHash(), minedBlock.Header.Height,
		minedBlock.Header.Hash, txIndex)

	return rawBlock, uint32(minedBlock.Header.Height), uint32(txIndex)
}

// buildCPFPForAnchor constructs a fee-paying child that spends the zero-value
// anchor output of the given parent transaction.

// refreshLeafProofs exports updated proofs for each leaf anchor output after
// the virtual tree has been published on-chain.
func buildCPFPForAnchor(t *testing.T, h *harness.Harness,
	parentTx *wire.MsgTx) *wire.MsgTx {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), assetTreeTimeout)
	defer cancel()

	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	defer rpcClient.Shutdown()

	changeAddr, err := rpcClient.GetNewAddress("")
	require.NoError(t, err)

	// Locate the anchor output (anchor script P2A).
	anchorIdx := -1
	for i := len(parentTx.TxOut) - 1; i >= 0; i-- {
		out := parentTx.TxOut[i]
		if bytes.Equal(out.PkScript, scripts.AnchorPkScript) {
			anchorIdx = i
			break
		}
	}
	require.GreaterOrEqual(t, anchorIdx, 0,
		"anchor output not found in parent tx")

	walletShim := newWalletKitFundingShimTree(h.LND.WalletKit)

	_, childTx, err := assets.BuildAnchorChildForTx(
		ctx, walletShim, parentTx, anchorIdx,
		assets.AnchorChildOptions{
			ChangeAddress: changeAddr,
			FeeRate:       chainfee.SatPerKWeight(300_000),
		},
	)
	require.NoError(t, err)

	return childTx
}

func logClientAssetBalances(t *testing.T, label string, assetID tapasset.ID,
	aliases []string, clients []*harness.TapClientHarness) {

	t.Helper()

	for i, client := range clients {
		ctx, cancel := context.WithTimeout(
			t.Context(), assetTreeTimeout,
		)
		defer cancel()

		balance, err := client.GetAssetBalance(ctx, assetID[:])
		require.NoErrorf(
			t, err, "balance fetch failed for %s", aliases[i],
		)

		t.Logf("%s: %s = %d", label, aliases[i], balance)
	}
}

// buildBoardingBatch aggregates all onboarding proofs into a single zero-fee
// anchor output, and adds extra btc funding from a wallet attached btc input.
// Returns the builder (for Publish), asset anchor plan, proof suffix, outpoint,
// and asset anchor internal key.
func buildBoardingBatch(t *testing.T, h *harness.Harness,
	leaves []tree.LeafDescriptor, assetID tapasset.ID,
	operatorKey *btcec.PrivateKey, params *address.ChainParams,
	operatorClient *harness.TapClientHarness,
	userKeys []*btcec.PrivateKey) (*assets.AnchorPlan,
	wire.OutPoint, *wire.TxOut, *wire.MsgTx, *assets.AssetTxBuilder,
	*btcec.PublicKey, error) {

	ctx, cancel := context.WithTimeout(t.Context(), assetTreeTimeout)
	defer cancel()

	builder := assets.NewAssetTxBuilder(assetID, params)

	var total uint64
	var totalBtcIn int64
	var totalLeafBtc int64
	for idx, leaf := range leaves {
		if leaf.Asset == nil || len(leaf.Asset.InputProof) == 0 {
			return nil, wire.OutPoint{}, nil, nil, nil, nil,
				fmt.Errorf("leaf %d missing proof", idx)
		}

		decoded, err := proof.DecodeFile(leaf.Asset.InputProof)
		require.NoError(t, err)
		lastProof, err := decoded.LastProof()
		require.NoError(t, err)

		t.Logf("Leaf %d asset anchor out=%s:%d", idx,
			lastProof.AnchorTx.TxHash(),
			lastProof.InclusionProof.OutputIndex)

		anchorOutIdx := lastProof.InclusionProof.OutputIndex
		anchorOut := lastProof.AnchorTx.TxOut[anchorOutIdx]
		totalBtcIn += anchorOut.Value
		totalLeafBtc += int64(leaf.Amount)

		total += leaf.Asset.AssetAmount

		// Reconstruct anchor key and closures for this leaf. The anchor
		// key is scripts.ARKNUMSKey (NUMS point for script-only spend),
		// matching what buildLeafDescriptor uses.
		anchorKey := assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(&scripts.ARKNUMSKey),
		}

		coopClosure := (&assets.CheckSigAddClosure{
			Key1: userKeys[idx].PubKey(),
			Key2: operatorKey.PubKey(),
		}).ScriptClosure()
		coopClosure.ID = "coop_multisig"

		csvClosure := (&assets.CSVClosure{
			Key:   userKeys[idx].PubKey(),
			Delay: csvDelayBlocks,
		}).ScriptClosure()
		csvClosure.ID = "csv"

		closures := []assets.ScriptClosure{coopClosure, csvClosure}

		if err := builder.AddAssetInput(assets.InputConfig{
			ProofFile: leaf.Asset.InputProof,
			AnchorKey: anchorKey,
			Closures:  closures,
		}); err != nil {
			return nil, wire.OutPoint{}, nil, nil, nil, nil,
				fmt.Errorf("add input %d: %w", idx, err)
		}
	}

	// Build MuSig2 participants: operator + all client cosigners.
	cosigners := make([]*btcec.PublicKey, 0, len(leaves)+1)
	cosigners = append(cosigners, operatorKey.PubKey())
	for _, leaf := range leaves {
		cosigners = append(cosigners, leaf.CoSignerKey)
	}

	// Build MuSig2 spec for keyspend (cooperative path).
	muSig2Spec, err := assetsMuSigAll(cosigners, nil)
	if err != nil {
		return nil, wire.OutPoint{}, nil, nil, nil, nil,
			fmt.Errorf("build musig2 spec: %w", err)
	}

	// Compute the internal key for the batch anchor output's OP_TRUE
	// script. By constructing the op_true tapscript with an actual internal
	// key instead of a NUMS key, we ensure that the asset script key is
	// unique.
	internalKey, err := tree.ComputeInternalKey(cosigners)
	if err != nil {
		return nil, wire.OutPoint{}, nil, nil, nil, nil,
			fmt.Errorf("compute internal key: %w", err)
	}

	// CSV closure for operator timeout spend (same structure as tree
	// branches).
	csvClosure := (&assets.CSVClosure{
		Key:   operatorKey.PubKey(),
		Delay: csvDelayBlocks,
	}).ScriptClosure()
	csvClosure.ID = "csv"

	// Batch anchor output structure:
	// - Keyspend: MuSig2(operator + all clients)
	// - Tapscript: CSV timeout for operator
	// - Asset script: OP_TRUE (any witness satisfies)
	// Use OpTrueUniqueScript so the script key is unique (based on internal
	// key) rather than the global NUMS-based OP_TRUE script key. This is
	// needed for correct proof verification - the sibling preimage in the
	// proof must match the one used to create the anchor output.
	if err := builder.AddAssetOutput(assets.OutputConfig{
		Amount: total,
		AnchorKey: assets.AnchorKeySpec{
			Mode:   assets.AnchorKeyModeMuSig2,
			MuSig2: muSig2Spec,
		},
		Closures: []assets.ScriptClosure{csvClosure},
		Script:   assets.OpTrueUniqueScript(internalKey),
	}); err != nil {
		return nil, wire.OutPoint{}, nil, nil, nil, nil,
			fmt.Errorf("add output: %w", err)
	}

	// Add a BTC input to fund the tree. The onboarding inputs only have
	// ~3000 sats total, but we add ~100k sats for proper tree outputs.
	utxos, err := h.LND.WalletKit.ListUnspent(ctx, 1, 9999)
	if err != nil {
		return nil, wire.OutPoint{}, nil, nil, nil, nil,
			fmt.Errorf("list unspent: %w", err)
	}
	if len(utxos) == 0 {
		return nil, wire.OutPoint{}, nil, nil, nil, nil,
			fmt.Errorf("no UTXOs available for funding")
	}

	// Pick the first UTXO with sufficient value.
	var fundingUtxo *lnwallet.Utxo
	for _, u := range utxos {
		if u.Value > 100000 {
			fundingUtxo = u
			break
		}
	}
	if fundingUtxo == nil {
		return nil, wire.OutPoint{}, nil, nil, nil, nil,
			fmt.Errorf("no UTXO with sufficient value")
	}

	t.Logf("Adding BTC input %s:%d value=%d for tree funding",
		fundingUtxo.OutPoint.Hash, fundingUtxo.OutPoint.Index,
		fundingUtxo.Value)

	// Lease the UTXO to prevent the wallet from using it for CPFP
	// funding. This avoids "conflict-in-package" errors when the CPFP
	// child and anchor both try to spend the same UTXO.
	lockID := wtxmgr.LockID{0x01} // Simple lock ID for testing.
	_, err = h.LND.WalletKit.LeaseOutput(
		ctx, lockID, fundingUtxo.OutPoint, 10*time.Minute,
	)
	if err != nil {
		return nil, wire.OutPoint{}, nil, nil, nil, nil,
			fmt.Errorf("lease utxo: %w", err)
	}

	if err := builder.AddBtcInput(assets.BtcInputSpec{
		Description: "tree-funding",
		Outpoint:    fundingUtxo.OutPoint,
		WitnessUtxo: &wire.TxOut{
			Value:    int64(fundingUtxo.Value),
			PkScript: fundingUtxo.PkScript,
		},
	}); err != nil {
		return nil, wire.OutPoint{}, nil, nil, nil, nil,
			fmt.Errorf("add btc input: %w", err)
	}

	// Calculate the total input value. We run the parent at zero fee and
	// rely on a CPFP child spending a dedicated anchor output. All sats go
	// to the asset output; the BTC anchor is zero-value.
	//
	// Inputs: ~3000 sats (3 onboarding UTXOs) + BTC input
	// Outputs: tree output (sum of leaf BTC), operator change, zero-value
	// BTC anchor
	totalInputs := int64(fundingUtxo.Value) + totalBtcIn
	treeValue := totalLeafBtc
	operatorChange := totalInputs - treeValue
	require.GreaterOrEqualf(t, operatorChange, int64(0),
		"operator change negative (inputs %d, tree %d)",
		totalInputs, treeValue)
	t.Logf("BTC balance: sum(inputs)=%d output=%d",
		totalInputs, treeValue+operatorChange)

	// Add operator change output so excess funding is explicitly returned.
	// Use P2WPKH for operator change to avoid tapd needing taproot internal
	// key metadata (which is required for P2TR exclusion proofs).
	changeHash := btcutil.Hash160(
		operatorKey.PubKey().SerializeCompressed(),
	)
	changeAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		changeHash, params.Params,
	)
	require.NoError(t, err)
	changePkScript, err := txscript.PayToAddrScript(changeAddr)
	require.NoError(t, err)
	require.NoError(t, builder.AddBtcOutput(assets.BtcOutputSpec{
		Description: "operator-change",
		ValueSat:    operatorChange,
		PkScript:    changePkScript,
	}))

	// Add an ephemeral BTC anchor for CPFP fee bumping BEFORE compile and
	// commit so it is part of the PSBT we sign and broadcast.
	builder.AddEphemeralAnchor()

	plan, err := builder.Compile(ctx)
	if err != nil {
		return nil, wire.OutPoint{}, nil, nil, nil, nil,
			fmt.Errorf("compile anchor: %w", err)
	}

	// Don't skip wallet funding so that FinalizeAnchor will sign our
	// manually added BTC input. We set FeeRate to 0 to avoid the wallet
	// adding more inputs, but we still need the wallet to sign our input.
	//
	// Use AssetOutputValues to specify the asset output value BEFORE commit
	// so that the virtual PSBTs and proofs reference the correct tx hash.
	// NoChangeOutput tells tapd not to add an automatic change output.
	// SkipZeroFeeBalance prevents the builder from auto-adding a balance
	// output based on proof anchor values (which doesn't account for our
	// additional BTC input).
	if err := builder.Commit(ctx, operatorClient.AssetWalletClient,
		assets.CommitOptions{
			FeeRate:            0, // Zero-fee parent.
			NoChangeOutput:     true,
			SkipZeroFeeBalance: true,
			SkipWalletFunding:  true, // We fund manually.
			AssetOutputValues: map[uint32]int64{
				0: treeValue, // Only the tree needs this BTC.
			},
		}); err != nil {
		return nil, wire.OutPoint{}, nil, nil, nil, nil,
			fmt.Errorf("commit anchor: %w", err)
	}

	anchorPsbt := builder.AnchorPsbt()
	if anchorPsbt == nil {
		return nil, wire.OutPoint{}, nil, nil, nil, nil,
			fmt.Errorf("anchor psbt missing")
	}

	// Sign the onboarding inputs via cooperative tapscript spend.
	for i := 0; i < len(leaves); i++ {
		scriptSpend, err := builder.PrepareScriptSpend(
			i, "coop_multisig",
		)
		if err != nil {
			return nil, wire.OutPoint{}, nil, nil, nil, nil,
				fmt.Errorf("prepare script spend %d: %w", i,
					err)
		}

		// Sign with user key (for this input) and operator key.
		userPriv := userKeys[i]
		userKeyHex := hex.EncodeToString(
			schnorr.SerializePubKey(userPriv.PubKey()),
		)
		opKeyHex := hex.EncodeToString(
			schnorr.SerializePubKey(operatorKey.PubKey()),
		)

		sigs := make(map[string][]byte)

		userSig, err := schnorr.Sign(userPriv, scriptSpend.SigHash[:])
		if err != nil {
			return nil, wire.OutPoint{}, nil, nil, nil, nil,
				fmt.Errorf("sign user %d: %w", i, err)
		}
		sigs[userKeyHex] = userSig.Serialize()

		opSig, err := schnorr.Sign(operatorKey, scriptSpend.SigHash[:])
		if err != nil {
			return nil, wire.OutPoint{}, nil, nil, nil, nil,
				fmt.Errorf("sign operator %d: %w", i, err)
		}
		sigs[opKeyHex] = opSig.Serialize()

		if err := builder.ApplyScriptSpend(
			scriptSpend, sigs,
		); err != nil {
			return nil, wire.OutPoint{}, nil, nil, nil, nil,
				fmt.Errorf("apply script spend %d: %w", i, err)
		}
	}

	// Sign and finalize the wallet-owned BTC input (the one added via
	// AddBtcInput for tree funding). FinalizeAnchor skips wallet finalize
	// when using an ephemeral P2A anchor (skipWalletFinalize=true), so we
	// need to handle wallet inputs explicitly.
	anchorPsbt = builder.AnchorPsbt()
	signedPsbt, err := h.LND.WalletKit.SignPsbt(ctx, anchorPsbt)
	if err != nil {
		return nil, wire.OutPoint{}, nil, nil, nil, nil,
			fmt.Errorf("sign btc input: %w", err)
	}

	// Finalize the signed PSBT to convert signatures to final witnesses.
	// This is needed because SignPsbt only puts signatures in PSBT fields,
	// not in FinalScriptWitness which is required for extraction.
	finalizedPsbt, _, err := h.LND.WalletKit.FinalizePsbt(
		ctx, signedPsbt, "",
	)
	if err != nil {
		return nil, wire.OutPoint{}, nil, nil, nil, nil,
			fmt.Errorf("finalize btc input: %w", err)
	}

	// Update the builder's anchor PSBT with the finalized version so that
	// FinalizeAnchor can apply our custom witnesses to it.
	builder.SetAnchorPsbt(finalizedPsbt)

	// Finalize the anchor PSBT. Since we already signed wallet inputs
	// above, this will only apply our custom witnesses (PrepareScriptSpend,
	// ApplyKeySpendSignature) to the onboarding inputs.
	_, err = builder.FinalizeAnchor(ctx, h.LND.WalletKit)
	if err != nil {
		return nil, wire.OutPoint{}, nil, nil, nil, nil,
			fmt.Errorf("finalize anchor: %w", err)
	}

	// Extract the finalized anchor tx.
	anchorPsbt = builder.AnchorPsbt()
	anchorTx, err := psbt.Extract(anchorPsbt)
	if err != nil {
		return nil, wire.OutPoint{}, nil, nil, nil, nil,
			fmt.Errorf("extract anchor: %w", err)
	}

	rootPlan := &plan.OutputPlans[0]
	rootOutpoint := wire.OutPoint{
		Hash:  anchorTx.TxHash(),
		Index: uint32(rootPlan.OutputIndex),
	}
	rootTxOut := anchorTx.TxOut[rootPlan.OutputIndex]

	return rootPlan, rootOutpoint, rootTxOut, anchorTx,
		builder, internalKey, nil
}

// assetsMuSigAll builds a MuSig2Spec over the provided cosigners (sorted)
// with the given taproot tweak.
func assetsMuSigAll(keys []*btcec.PublicKey,
	taprootTweak []byte) (*assets.MuSig2Spec, error) {

	if len(keys) == 0 {
		return nil, fmt.Errorf("no cosigners")
	}

	sort.Slice(keys, func(i, j int) bool {
		return bytes.Compare(
			keys[i].SerializeCompressed(),
			keys[j].SerializeCompressed(),
		) < 0
	})

	participants := make([]assets.MuSig2Participant, 0, len(keys))
	for _, k := range keys {
		participants = append(participants, assets.MuSig2Participant{
			Role:   assets.SignerRole("cosigner"),
			PubKey: k.SerializeCompressed(),
		})
	}

	return &assets.MuSig2Spec{
		Participants: participants,
		SortKeys:     true,
		Tweaks: assets.MuSig2Tweaks{
			TaprootTweak: taprootTweak,
		},
	}, nil
}

type walletKitFundingShimTree struct {
	kit lndclient.WalletKitClient
}

func newWalletKitFundingShimTree(
	kit lndclient.WalletKitClient) *walletKitFundingShimTree {

	return &walletKitFundingShimTree{kit: kit}
}

func (w *walletKitFundingShimTree) FundPsbt(ctx context.Context,
	packet *psbt.Packet, changeIndex int, feeRate chainfee.SatPerKWeight) (
	*psbt.Packet, error) {

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return nil, err
	}

	coinSelect := &walletrpc.PsbtCoinSelect{
		Psbt: buf.Bytes(),
		ChangeOutput: &walletrpc.PsbtCoinSelect_ExistingOutputIndex{
			ExistingOutputIndex: int32(changeIndex),
		},
	}

	req := &walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_CoinSelect{
			CoinSelect: coinSelect,
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerKw{
			SatPerKw: uint64(feeRate),
		},
		SpendUnconfirmed: false,
	}

	funded, _, _, err := w.kit.FundPsbt(ctx, req)
	if err != nil {
		return nil, err
	}

	return funded, nil
}

func (w *walletKitFundingShimTree) SignPsbt(ctx context.Context,
	packet *psbt.Packet) (*psbt.Packet, error) {

	return w.kit.SignPsbt(ctx, packet)
}
