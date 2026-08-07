package tapassets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	tapgrpc "github.com/lightninglabs/tap-sdk/grpc"
	"github.com/lightninglabs/wavelength/harness"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

const (
	treeE2ETimeout   = 60 * time.Second
	treeE2ELeafSats  = btcutil.Amount(10_000)
	treeE2EExitDelay = 144
	treeE2ESweepCSV  = 1008
	treeE2EChildFee  = int64(5_000)
)

// localTreeSigner is an in-process input.MuSig2Signer over one private
// key, mirroring lib/tree's test signer so the e2e can run the real
// multi-party signing ceremony without lnd signers.
type localTreeSigner struct {
	privKey       *btcec.PrivateKey
	sessions      map[input.MuSig2SessionID]*localTreeSession
	nextSessionID int
}

type localTreeSession struct {
	info         *input.MuSig2SessionInfo
	musigSession *musig2.Session
	allNonces    bool
}

func newLocalTreeSigner(privKey *btcec.PrivateKey) *localTreeSigner {
	return &localTreeSigner{
		privKey:  privKey,
		sessions: make(map[input.MuSig2SessionID]*localTreeSession),
	}
}

// MuSig2CreateSession opens a real MuSig2 session over the signer's key.
func (l *localTreeSigner) MuSig2CreateSession(_ input.MuSig2Version,
	_ keychain.KeyLocator, signers []*btcec.PublicKey,
	tweaks *input.MuSig2Tweaks, _ [][musig2.PubNonceSize]byte,
	localNonces *musig2.Nonces) (*input.MuSig2SessionInfo, error) {

	nonces := localNonces
	if nonces == nil {
		fresh, err := musig2.GenNonces(
			musig2.WithPublicKey(
				l.privKey.PubKey(),
			),
			musig2.WithNonceSecretKeyAux(l.privKey),
		)
		if err != nil {
			return nil, err
		}
		nonces = fresh
	}

	ctxOpts := []musig2.ContextOption{
		musig2.WithKnownSigners(signers),
	}
	if tweaks != nil && len(tweaks.TaprootTweak) > 0 {
		ctxOpts = append(
			ctxOpts,
			musig2.WithTaprootTweakCtx(tweaks.TaprootTweak),
		)
	}

	musigCtx, err := musig2.NewContext(l.privKey, true, ctxOpts...)
	if err != nil {
		return nil, err
	}
	session, err := musigCtx.NewSession(
		musig2.WithPreGeneratedNonce(nonces),
	)
	if err != nil {
		return nil, err
	}

	sessionID := input.MuSig2SessionID{byte(l.nextSessionID)}
	l.nextSessionID++

	l.sessions[sessionID] = &localTreeSession{
		info: &input.MuSig2SessionInfo{
			SessionID:   sessionID,
			PublicNonce: nonces.PubNonce,
		},
		musigSession: session,
	}

	return l.sessions[sessionID].info, nil
}

// MuSig2RegisterNonces registers other signers' individual nonces.
func (l *localTreeSigner) MuSig2RegisterNonces(id input.MuSig2SessionID,
	nonces [][musig2.PubNonceSize]byte) (bool, error) {

	session, ok := l.sessions[id]
	if !ok {
		return false, fmt.Errorf("session not found")
	}

	var (
		haveAll bool
		err     error
	)
	for _, nonce := range nonces {
		haveAll, err = session.musigSession.RegisterPubNonce(nonce)
		if err != nil {
			return false, err
		}
	}
	session.allNonces = haveAll

	return haveAll, nil
}

// MuSig2RegisterCombinedNonce registers the aggregated nonce directly:
// tree signing shares only the per-transaction aggregate, never the
// individual nonces.
func (l *localTreeSigner) MuSig2RegisterCombinedNonce(id input.MuSig2SessionID,
	agg [musig2.PubNonceSize]byte) error {

	session, ok := l.sessions[id]
	if !ok {
		return fmt.Errorf("session not found")
	}

	err := session.musigSession.RegisterCombinedNonce(agg)
	if err != nil {
		return err
	}
	session.allNonces = true

	return nil
}

// MuSig2Sign produces the local partial signature.
func (l *localTreeSigner) MuSig2Sign(id input.MuSig2SessionID, msg [32]byte,
	_ bool) (*musig2.PartialSignature, error) {

	session, ok := l.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session not found")
	}
	if !session.allNonces {
		return nil, fmt.Errorf("not all nonces registered")
	}

	return session.musigSession.Sign(msg)
}

// MuSig2CombineSig folds other signers' partial signatures into the final
// schnorr signature.
func (l *localTreeSigner) MuSig2CombineSig(id input.MuSig2SessionID,
	partials []*musig2.PartialSignature) (*schnorr.Signature, bool, error) {

	session, ok := l.sessions[id]
	if !ok {
		return nil, false, fmt.Errorf("session not found")
	}

	var (
		haveAll bool
		err     error
	)
	for _, partial := range partials {
		haveAll, err = session.musigSession.CombineSig(partial)
		if err != nil {
			return nil, false, err
		}
	}
	if !haveAll {
		return nil, false, fmt.Errorf("missing partial signatures")
	}

	finalSig := session.musigSession.FinalSig()
	if finalSig == nil {
		return nil, false, fmt.Errorf("final signature is invalid")
	}

	return finalSig, true, nil
}

// MuSig2GetCombinedNonce is unused by the tree signing ceremony.
func (l *localTreeSigner) MuSig2GetCombinedNonce(id input.MuSig2SessionID) (
	[musig2.PubNonceSize]byte, error) {

	return [musig2.PubNonceSize]byte{}, fmt.Errorf("not used")
}

// MuSig2Cleanup drops a session.
func (l *localTreeSigner) MuSig2Cleanup(id input.MuSig2SessionID) error {
	delete(l.sessions, id)

	return nil
}

// treeE2EParticipant bundles one signer's key material.
type treeE2EParticipant struct {
	priv   *btcec.PrivateKey
	pub    *btcec.PublicKey
	signer *localTreeSigner
}

// newTreeE2EParticipant creates a participant with a fresh key.
func newTreeE2EParticipant(t *testing.T) *treeE2EParticipant {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return &treeE2EParticipant{
		priv:   priv,
		pub:    priv.PubKey(),
		signer: newLocalTreeSigner(priv),
	}
}

// TestAssetTreeE2E builds, signs, and unrolls a complete asset-aware VTXO
// tree against a real tapd: mint a grouped asset, anchor it beneath a
// MuSig2 batch output, materialize a three-leaf tree of tapd-committed
// transitions, run the multi-party signing ceremony, verify every
// signature, and force the whole tree on-chain through v3 package relay.
func TestAssetTreeE2E(t *testing.T) {
	image := os.Getenv("ARK_ITEST_TAPD_IMAGE")
	if image == "" {
		t.Skip("set ARK_ITEST_TAPD_IMAGE to run the asset tree e2e")
	}

	opts := harness.DefaultOptions()
	opts.GroupName = t.Name()
	opts.StartTapd = true
	opts.TapdImage = image

	h := harness.NewHarness(t, &opts)
	t.Cleanup(h.Stop)
	h.Start()
	h.FundOperatorLND(2 * btcutil.SatoshiPerBitcoin)

	client, err := tapgrpc.NewClient(&tapgrpc.Config{
		Host:     net.JoinHostPort("127.0.0.1", h.TapdGRPCPort),
		Network:  tapsdk.NetworkRegtest,
		TLS:      tapgrpc.TLSFromPath(h.TapdTLSCertPath()),
		Macaroon: tapsdk.MacaroonFromPath(h.TapdMacaroonPath()),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	wallet := tapsdk.NewWallet(client, tapsdk.NetworkRegtest)

	operator := newTreeE2EParticipant(t)
	users := []*treeE2EParticipant{
		newTreeE2EParticipant(t),
		newTreeE2EParticipant(t),
		newTreeE2EParticipant(t),
	}
	amounts := []uint64{800, 200, 500}

	// Mint 1,500 grouped units and export + verify the issuance proof.
	record := mintTreeE2EAsset(t, h, client, 1_500)
	require.True(t, record.AssetRef.IsGroupRef())

	// Commits and verifiers identify the asset by its group-aware ref;
	// proof exports address the concrete issuance by its id.
	assetRef := record.AssetRef
	exportRef := tapsdk.AssetRefFromAssetID(record.Genesis.IssuanceID)
	mintProof := exportTreeE2EProof(
		t, client, exportRef, record.ScriptKey.PubKey, nil,
	)

	// The batch output aggregates operator and every leaf owner, with
	// the operator sweep leaf as the asset commitment's sibling.
	rootCosigners := tree.UniqueCosigners([]*btcec.PublicKey{
		operator.pub, users[0].pub, users[1].pub, users[2].pub,
	})
	sweepLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		operator.pub, treeE2ESweepCSV,
	)
	require.NoError(t, err)

	batch := commitTreeBatchAnchor(
		t, h, wallet, client, assetRef, mintProof, 1_500, rootCosigners,
		sweepLeaf,
	)

	// Build leaf policies and the anchor lookup keyed by owner.
	leaves := make([]tree.LeafDescriptor, len(users))
	anchorsByOwner := make(map[string]TreeLeafAnchor, len(users))
	for i, user := range users {
		leafAnchor, pkScript := treeE2ELeafAnchor(
			t, user.pub, operator.pub,
		)
		anchorsByOwner[keyHex(user.pub)] = leafAnchor
		leaves[i] = tree.LeafDescriptor{
			PkScript:    pkScript,
			Amount:      treeE2ELeafSats,
			CoSignerKey: user.pub,
			AssetAmount: amounts[i],
		}
	}

	digest := sha256.Sum256([]byte(batch.outpoint.String()))
	cfg := TreeMaterializerConfig{
		Wallet:    wallet,
		AssetRef:  assetRef,
		SweepLeaf: sweepLeaf,
		LeafAnchor: func(node *tree.Node) (TreeLeafAnchor, error) {
			for _, cosigner := range node.CoSigners {
				anchor, ok := anchorsByOwner[keyHex(cosigner)]
				if ok {
					return anchor, nil
				}
			}

			return TreeLeafAnchor{}, fmt.Errorf("no leaf anchor " +
				"for node")
		},
		Root:   batch.commit.RootSource,
		Digest: tapsdk.Hash(digest),
	}

	// The context is created inside BuildAssetTree; the config field is
	// rebound there.
	cfg.AssetContext = tree.NewAssetTreeContext()

	built, assetCtx, err := BuildAssetTree(
		t.Context(), cfg, AssetTreeRequest{
			Leaves:        leaves,
			OperatorKey:   operator.pub,
			Radix:         2,
			BatchOutpoint: batch.outpoint,
			BatchOutput:   batch.output,
			AssetAmount:   1_500,
		},
	)
	require.NoError(t, err)
	require.Equal(t, 3, built.NumLeaves())
	require.Equal(t, 5, built.NumTx())

	// Multi-party signing ceremony, then verify every signature.
	signAssetTree(t, built, assetCtx, operator, users)
	require.NoError(t, built.VerifySigned())

	// The tree was built and signed against the unconfirmed commitment
	// transition; only now does the batch anchor hit the chain.
	publishBatchAnchor(t, h, batch.commit)
	h.GenerateAndWait(1)

	// Force the entire tree on-chain, parent before child, through v3
	// package relay with a chained fee input.
	publishTreePackages(t, h, built)
}

// treeE2EBatch bundles the caller-funded batch anchor: the sealed commit
// (with the tree root source), the batch outpoint, and the final funded
// transaction awaiting broadcast.
type treeE2EBatch struct {
	commit   *BatchAnchorCommit
	outpoint wire.OutPoint
	output   *wire.TxOut
}

// commitTreeBatchAnchor commits the minted asset beneath the tree's batch
// output on a caller-funded anchor transaction, mirroring the operator's
// commitment-transaction flow: derive the composed script before funding,
// fund with an LND wallet UTXO, then seal against the final transaction.
// The transaction is not broadcast; the tree builds and signs first.
func commitTreeBatchAnchor(t *testing.T, h *harness.Harness,
	wallet *tapsdk.Wallet, client tapsdk.Client, assetRef tapsdk.AssetRef,
	mintProof []byte, amount uint64, rootCosigners []*btcec.PublicKey,
	sweepLeaf txscript.TapLeaf) *treeE2EBatch {

	t.Helper()
	ctx := t.Context()

	// Locate the mint anchor in tapd's inventory: the funding UTXO the
	// commitment transition spends.
	verified, err := client.VerifyProof(ctx, mintProof)
	require.NoError(t, err)
	require.True(t, verified.Valid)
	tip := verified.DecodedProof

	utxos, err := client.ListUtxos(ctx, &tapsdk.ListUtxosRequest{
		IncludeLeased: true,
	})
	require.NoError(t, err)
	var anchor *tapsdk.ManagedUtxo
	for _, candidate := range utxos {
		if candidate != nil && candidate.OutPoint == tip.Outpoint {
			anchor = candidate
			break
		}
	}
	require.NotNil(t, anchor, "mint anchor not in tapd inventory")

	anchorInternalKey, err := btcec.ParsePubKey(anchor.InternalKey[:])
	require.NoError(t, err)
	fundingOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash(tip.Outpoint.Txid),
		Index: tip.Outpoint.Index,
	}

	batchValue := int64(treeE2ELeafSats) * 3
	req := &BatchAnchorRequest{
		AssetRef: assetRef,
		Amount:   amount,
		Sources: []BatchAnchorSource{{
			ProofFile: append([]byte(nil), mintProof...),
			Amount:    amount,
			Verifier: &proofInventoryVerifier{
				client:    client,
				assetRef:  assetRef,
				amount:    amount,
				anchor:    tip.Outpoint,
				assetRoot: anchor.TaprootAssetRoot,
			},
			AnchorOutpoint:    fundingOutpoint,
			AnchorInternalKey: anchorInternalKey,
		}},
		Cosigners:      rootCosigners,
		SweepLeaf:      sweepLeaf,
		Digest:         tapsdk.Hash(sha256.Sum256([]byte(t.Name()))),
		OutputIndex:    0,
		OutputValueSat: batchValue,
	}

	committer, err := NewBatchAnchorCommitter(wallet)
	require.NoError(t, err)

	// Derive the composed batch script against the pre-funding template.
	templateTx := wire.NewMsgTx(2)
	templateTx.AddTxIn(wire.NewTxIn(&fundingOutpoint, nil, nil))
	placeholder, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootKeyNoScript(&arkscript.ARKNUMSKey),
	)
	require.NoError(t, err)
	templateTx.AddTxOut(&wire.TxOut{
		Value:    batchValue,
		PkScript: placeholder,
	})
	template, err := psbt.NewFromUnsignedTx(templateTx)
	require.NoError(t, err)

	derived, err := committer.DeriveScript(ctx, req, template)
	require.NoError(t, err)

	// Fund deterministically with an LND wallet UTXO: the derived
	// script lands on the batch output, change returns to the wallet.
	lndUtxos, err := h.LND.WalletKit.ListUnspent(ctx, 1, 0x7fffffff)
	require.NoError(t, err)
	var feeUtxo *lnwallet.Utxo
	for _, candidate := range lndUtxos {
		if candidate.Value >= 1_000_000 {
			feeUtxo = candidate
			break
		}
	}
	require.NotNil(t, feeUtxo, "no LND utxo large enough to fund")

	// Change goes to a fresh P2WPKH address: tapd requires the taproot
	// internal key of every non-asset P2TR output to build exclusion
	// proofs, and segwit v0 outputs sidestep that requirement.
	changeAddr, err := h.LND.WalletKit.NextAddr(
		ctx, "", walletrpc.AddressType_WITNESS_PUBKEY_HASH, true,
	)
	require.NoError(t, err)
	changeScript, err := txscript.PayToAddrScript(changeAddr)
	require.NoError(t, err)

	fundedTx := templateTx.Copy()
	fundedTx.TxOut[0].PkScript = append([]byte(nil), derived.PkScript...)
	fundedTx.AddTxIn(wire.NewTxIn(&feeUtxo.OutPoint, nil, nil))
	change := int64(feeUtxo.Value) + anchor.AmtSat - batchValue -
		treeE2EChildFee
	require.Positive(t, change)
	fundedTx.AddTxOut(&wire.TxOut{
		Value:    change,
		PkScript: changeScript,
	})

	funded, err := psbt.NewFromUnsignedTx(fundedTx)
	require.NoError(t, err)
	funded.Inputs[1].WitnessUtxo = &wire.TxOut{
		Value:    int64(feeUtxo.Value),
		PkScript: append([]byte(nil), feeUtxo.PkScript...),
	}

	commit, err := committer.Commit(ctx, req, funded, derived)
	require.NoError(t, err)

	return &treeE2EBatch{
		commit: commit,
		outpoint: wire.OutPoint{
			Hash:  fundedTx.TxHash(),
			Index: req.OutputIndex,
		},
		output: wire.NewTxOut(batchValue, derived.PkScript),
	}
}

// publishBatchAnchor signs the funding input, finalizes, and broadcasts
// the committed batch anchor transaction.
func publishBatchAnchor(t *testing.T, h *harness.Harness,
	commit *BatchAnchorCommit) {

	t.Helper()
	ctx := t.Context()

	packet, err := psbtutil.Parse(commit.AnchorPSBT)
	require.NoError(t, err)
	signed, err := h.LND.WalletKit.SignPsbt(ctx, packet)
	require.NoError(t, err)
	finalized, _, err := h.LND.WalletKit.FinalizePsbt(ctx, signed, "")
	require.NoError(t, err)

	finalTx, err := psbt.Extract(finalized)
	require.NoError(t, err)
	require.NoError(
		t, h.LND.WalletKit.PublishTransaction(
			ctx, finalTx, "tree-e2e-batch-anchor",
		),
	)
}

// treeE2ELeafAnchor compiles a standard VTXO policy for one leaf owner and
// projects it into the materializer's leaf anchor shape.
func treeE2ELeafAnchor(t *testing.T, owner, operator *btcec.PublicKey) (
	TreeLeafAnchor, []byte) {

	t.Helper()

	encoded, err := arkscript.EncodeStandardVTXOTemplate(
		owner, operator, treeE2EExitDelay,
	)
	require.NoError(t, err)
	template, err := arkscript.DecodePolicyTemplate(encoded)
	require.NoError(t, err)
	policy, err := template.Compile()
	require.NoError(t, err)
	pkScript, err := template.PkScript()
	require.NoError(t, err)

	tapLeaves := make([]txscript.TapLeaf, len(policy.Leaves))
	for idx := range policy.Leaves {
		tapLeaves[idx] = policy.Leaves[idx].Leaf
	}

	return TreeLeafAnchor{
		UncomposedPkScript: pkScript,
		InternalKey:        policy.InternalKey,
		TapLeaves:          tapLeaves,
	}, pkScript
}

// signAssetTree runs the full multi-party MuSig2 ceremony: one signer
// session per participant, nonce aggregation per transaction, partial
// signatures, and final combination submitted back onto the tree.
func signAssetTree(t *testing.T, built *tree.Tree,
	assetCtx *tree.AssetTreeContext, operator *treeE2EParticipant,
	users []*treeE2EParticipant) {

	t.Helper()

	lookup := assetCtx.TweakLookup()
	participants := append([]*treeE2EParticipant{operator}, users...)

	sessions := make([]*tree.SignerSession, len(participants))
	for i, participant := range participants {
		session, err := built.NewTreeSignerSession(
			participant.signer, &keychain.KeyDescriptor{
				PubKey: participant.pub,
			},
			lookup,
		)
		require.NoError(t, err)
		sessions[i] = session
	}

	// Phase 1: aggregate nonces per transaction across exactly the
	// signers whose paths include it.
	noncesByTx := make(map[tree.TxID][][musig2.PubNonceSize]byte)
	for _, session := range sessions {
		for txid, nonce := range session.GetNonces() {
			noncesByTx[txid] = append(noncesByTx[txid], nonce)
		}
	}
	aggByTx := make(map[tree.TxID]tree.Musig2PubNonce, len(noncesByTx))
	for txid, nonces := range noncesByTx {
		agg, err := musig2.AggregateNonces(nonces)
		require.NoError(t, err)
		aggByTx[txid] = agg
	}
	for _, session := range sessions {
		scoped := make(map[tree.TxID]tree.Musig2PubNonce)
		for txid := range session.SessionIDs() {
			scoped[txid] = aggByTx[txid]
		}
		require.NoError(t, session.RegisterAggNonces(scoped))
	}

	// Phase 2: partial signatures from everyone; the operator's session
	// spans every node and combines the others' partials.
	partialsByTx := make(map[tree.TxID][]*musig2.PartialSignature)
	for i, session := range sessions {
		partials, err := session.Signatures(false)
		require.NoError(t, err)

		// The operator's own partial is already inside its session.
		if i == 0 {
			continue
		}
		for txid, partial := range partials {
			partialsByTx[txid] = append(
				partialsByTx[txid], partial,
			)
		}
	}

	operatorIDs := sessions[0].SessionIDs()
	finalSigs := make(map[tree.TxID]*schnorr.Signature, len(operatorIDs))
	for txid, sessionID := range operatorIDs {
		sig, haveAll, err := operator.signer.MuSig2CombineSig(
			sessionID, partialsByTx[txid],
		)
		require.NoError(t, err)
		require.True(t, haveAll)
		finalSigs[txid] = sig
	}

	require.NoError(t, built.SubmitTreeSigs(finalSigs))
}

// esploraTxOut looks up one output of a transaction through the harness's
// electrs HTTP endpoint.
func esploraTxOut(t *testing.T, h *harness.Harness, txid string,
	pkScript []byte) (uint32, int64) {

	t.Helper()

	var (
		index uint32
		value int64
	)
	require.Eventually(t, func() bool {
		resp, err := http.Get(h.EsploraURL + "/tx/" + txid)
		if err != nil || resp.StatusCode != http.StatusOK {
			return false
		}
		defer func() {
			_ = resp.Body.Close()
		}()

		var decoded struct {
			Vout []struct {
				ScriptPubKey string `json:"scriptpubkey"`
				Value        int64  `json:"value"`
			} `json:"vout"`
		}
		if json.NewDecoder(resp.Body).Decode(&decoded) != nil {
			return false
		}
		for i, out := range decoded.Vout {
			script, err := hex.DecodeString(out.ScriptPubKey)
			if err != nil {
				continue
			}
			if bytes.Equal(script, pkScript) {
				index = uint32(i)
				value = out.Value

				return true
			}
		}

		return false
	}, treeE2ETimeout, time.Second, "esplora never indexed %s", txid)

	return index, value
}

// publishTreePackages broadcasts every node transaction parent-first via
// v3 package relay, funding each package's fees through a chained P2WPKH
// input.
func publishTreePackages(t *testing.T, h *harness.Harness, built *tree.Tree) {
	t.Helper()

	// Fund a throwaway fee key whose change chains across packages.
	feeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	feeHash := btcaddr.Hash160(feeKey.PubKey().SerializeCompressed())
	feeAddr, err := btcaddr.NewAddressWitnessPubKeyHash(
		feeHash, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	feeScript, err := txscript.PayToAddrScript(feeAddr)
	require.NoError(t, err)

	fundingTxid := h.Faucet(feeAddr.String(), 1_000_000)
	h.GenerateAndWait(1)
	feeIndex, feeValue := esploraTxOut(t, h, fundingTxid, feeScript)
	fundingHash, err := chainhash.NewHashFromStr(fundingTxid)
	require.NoError(t, err)
	feeOutpoint := wire.OutPoint{Hash: *fundingHash, Index: feeIndex}

	bitcoind, err := h.BitcoindClient()
	require.NoError(t, err)

	var publish func(node *tree.Node)
	publish = func(node *tree.Node) {
		parentTx, err := node.ToSignedTx()
		require.NoError(t, err)

		child, changeValue := buildTreeFeeChild(
			t, parentTx, feeOutpoint, feeValue, feeKey, feeScript,
		)

		result, err := bitcoind.SubmitPackage(
			[]*wire.MsgTx{parentTx}, child, nil,
		)
		require.NoError(t, err)
		require.NotNil(t, result)
		for wtxid, txResult := range result.TxResults {
			require.Empty(
				t, txResult.Error, "package tx %s rejected",
				wtxid,
			)
		}

		h.GenerateAndWait(1)

		feeOutpoint = wire.OutPoint{
			Hash:  child.TxHash(),
			Index: 0,
		}
		feeValue = changeValue

		for _, idx := range sortedChildIndices(node.Children) {
			publish(node.Children[idx])
		}
	}
	publish(built.Root)
}

// buildTreeFeeChild spends the parent's P2A anchor plus the chained fee
// input into a single change output, signing the fee input.
func buildTreeFeeChild(t *testing.T, parent *wire.MsgTx,
	feeOutpoint wire.OutPoint, feeValue int64, feeKey *btcec.PrivateKey,
	feeScript []byte) (*wire.MsgTx, int64) {

	t.Helper()

	anchorIndex := len(parent.TxOut) - 1
	anchorOutpoint := wire.OutPoint{
		Hash:  parent.TxHash(),
		Index: uint32(anchorIndex),
	}

	child := wire.NewMsgTx(3)
	child.AddTxIn(&wire.TxIn{
		PreviousOutPoint: anchorOutpoint,
		Sequence:         wire.MaxTxInSequenceNum - 2,
	})
	child.AddTxIn(&wire.TxIn{
		PreviousOutPoint: feeOutpoint,
		Sequence:         wire.MaxTxInSequenceNum - 2,
	})
	changeValue := feeValue - treeE2EChildFee
	require.Greater(t, changeValue, int64(1_000))
	child.AddTxOut(wire.NewTxOut(changeValue, feeScript))

	prevOuts := txscript.NewMultiPrevOutFetcher(
		map[wire.OutPoint]*wire.TxOut{
			anchorOutpoint: parent.TxOut[anchorIndex],
			feeOutpoint:    wire.NewTxOut(feeValue, feeScript),
		},
	)
	hashes := txscript.NewTxSigHashes(child, prevOuts)
	witness, err := txscript.WitnessSignature(
		child, hashes, 1, feeValue, feeScript, txscript.SigHashAll,
		feeKey, true,
	)
	require.NoError(t, err)
	child.TxIn[1].Witness = witness

	// The P2A anchor input needs no witness: it is anyone-can-spend.
	return child, changeValue
}

// mintTreeE2EAsset mints and finalizes a grouped fungible asset.
func mintTreeE2EAsset(t *testing.T, h *harness.Harness, client tapsdk.Client,
	amount uint64) *tapsdk.AssetRecord {

	t.Helper()
	ctx := t.Context()

	batch, err := client.MintAsset(ctx, &tapsdk.MintAssetRequest{
		Asset: &tapsdk.MintAsset{
			AssetType:     tapsdk.AssetTypeFungible,
			AssetVersion:  tapsdk.AssetVersionV1,
			Name:          "TREE",
			InitialSupply: amount,
			AllowIssuance: true,
		},
		ShortResponse: true,
	})
	require.NoError(t, err)

	_, err = client.FinalizeBatch(ctx, &tapsdk.FinalizeBatchRequest{
		ShortResponse: true,
	})
	require.NoError(t, err)

	waitTreeE2EBatchState(t, client, batch.BatchKey,
		func(state tapsdk.BatchState) bool {
			return state == tapsdk.BatchStateBroadcast ||
				state == tapsdk.BatchStateConfirmed ||
				state == tapsdk.BatchStateFinalized
		},
	)
	h.GenerateAndWait(6)
	waitTreeE2EBatchState(t, client, batch.BatchKey,
		func(state tapsdk.BatchState) bool {
			return state == tapsdk.BatchStateFinalized
		},
	)

	var record *tapsdk.AssetRecord
	require.Eventually(t, func() bool {
		records, listErr := client.ListAssetRecords(
			ctx, &tapsdk.ListAssetsRequest{},
		)
		if listErr != nil {
			return false
		}
		for _, candidate := range records {
			if candidate != nil &&
				candidate.Genesis.Tag == "TREE" &&
				candidate.Amount == amount {

				record = candidate

				return true
			}
		}

		return false
	}, treeE2ETimeout, time.Second, "minted asset never visible")

	return record
}

// waitTreeE2EBatchState polls the mint batch until it reaches the wanted
// state.
func waitTreeE2EBatchState(t *testing.T, client tapsdk.Client,
	batchKey tapsdk.PubKey, ready func(tapsdk.BatchState) bool) {

	t.Helper()

	require.Eventually(t, func() bool {
		batches, err := client.ListBatches(
			t.Context(), &tapsdk.ListBatchesRequest{
				BatchKey: &batchKey,
			},
		)
		if err != nil || len(batches) != 1 || batches[0] == nil {
			return false
		}

		return ready(batches[0].Batch.State)
	}, treeE2ETimeout, time.Second, "mint batch state never reached")
}

// exportTreeE2EProof exports and sanity-verifies a proof file.
func exportTreeE2EProof(t *testing.T, client tapsdk.Client,
	assetRef tapsdk.AssetRef, scriptKey tapsdk.PubKey,
	outpoint *tapsdk.Outpoint) []byte {

	t.Helper()

	exported, err := client.ExportProof(
		t.Context(), assetRef, scriptKey, outpoint,
	)
	require.NoError(t, err)
	require.NotEmpty(t, exported.RawProofFile)

	verified, err := client.VerifyProof(
		t.Context(), exported.RawProofFile,
	)
	require.NoError(t, err)
	require.True(t, verified.Valid)

	return exported.RawProofFile
}

// keyHex returns the compressed hex encoding of a public key.
func keyHex(key *btcec.PublicKey) string {
	return hex.EncodeToString(key.SerializeCompressed())
}
