package tapassets

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/stretchr/testify/require"
)

// assetVTXOFixture is a materialized asset tree beneath a commitment
// transaction, with the extracted path of its first leaf.
type assetVTXOFixture struct {
	tree         *treeMaterializerFixture
	commitmentTx *wire.MsgTx
	path         *tree.Tree
	leaf         *tree.Node
	leafOutpoint wire.OutPoint
	committed    *commitResult
}

func newAssetVTXOFixture(t *testing.T) *assetVTXOFixture {
	t.Helper()

	commitmentTx := wire.NewMsgTx(3)
	commitmentTx.AddTxIn(
		wire.NewTxIn(
			&wire.OutPoint{
				Hash: chainhash.Hash{0xC0},
			},
			nil,
			nil,
		),
	)
	commitmentTx.AddTxOut(wire.NewTxOut(1_000, []byte{0x51}))
	commitmentTx.AddTxOut(wire.NewTxOut(30_000, []byte{0x51}))
	batch := wire.OutPoint{Hash: commitmentTx.TxHash(), Index: 1}

	f := newTreeMaterializerFixtureAt(t, batch, 800, 200, 500)
	require.NoError(
		t,
		tree.Materialize(
			t.Context(), f.structure.Root, tree.MaterializeParams{
				Input: f.batch,
			},
			f.mat,
		),
	)
	built := &tree.Tree{
		Root:          f.structure.Root,
		BatchOutpoint: f.batch,
		BatchOutput:   f.batchOut,
		AssetContext:  f.structure.AssetContext,
	}
	path, err := built.ExtractPathForCoSigners(f.leaves[0].CoSignerKey)
	require.NoError(t, err)
	require.NotNil(t, path)

	leaves := path.Root.GetLeafNodes()
	require.Len(t, leaves, 1)
	leafOutpoint, err := leaves[0].GetNonAnchorOutpoint()
	require.NoError(t, err)
	committed := f.driver.packages[string(
		path.AssetContext.SealedPackage(leaves[0].Input),
	)]
	require.NotNil(t, committed)

	return &assetVTXOFixture{
		tree:         f,
		commitmentTx: commitmentTx,
		path:         path,
		leaf:         leaves[0],
		leafOutpoint: *leafOutpoint,
		committed:    committed,
	}
}

func (f *assetVTXOFixture) leafRoot() tapsdk.Hash {
	var root tapsdk.Hash
	copy(root[:], f.path.AssetContext.LeafAssetRoot(f.leaf.Input))

	return root
}

// cloneCommit copies the outputs that the leaf checks read.
func cloneCommit(committed *commitResult) *commitResult {
	clone := *committed
	clone.outputs = append([]commitOutput(nil), committed.outputs...)
	clone.inputs = append([]commitInput(nil), committed.inputs...)

	return &clone
}

// TestCheckLeafOutputs checks that a leaf package must move exactly the
// requested asset into the VTXO under an OP_TRUE key.
func TestCheckLeafOutputs(t *testing.T) {
	t.Parallel()

	f := newAssetVTXOFixture(t)
	ref := f.tree.mat.cfg.AssetRef
	leafOutpoint := sdkOutpoint(f.leafOutpoint)

	testCases := []struct {
		name   string
		mutate func(*commitResult)
		amount uint64
		err    string
	}{
		{
			name:   "requested asset",
			amount: 800,
		},
		{
			name:   "different amount",
			amount: 799,
			err:    "holds 800 units, want 799",
		},
		{
			name: "different asset",
			mutate: func(c *commitResult) {
				c.outputs[0].assetRef = tapsdk.
					AssetRefFromAssetID(tapsdk.AssetID{
						0xBB,
					})
			},
			amount: 800,
			err:    "holds",
		},
		{
			name: "script key not OP_TRUE",
			mutate: func(c *commitResult) {
				c.outputs[0].scriptMode = scriptExternal
			},
			amount: 800,
			err:    "not OP_TRUE locked",
		},
		{
			name: "missing OP_TRUE witness",
			mutate: func(c *commitResult) {
				c.outputs[0].opTrueWitness = nil
			},
			amount: 800,
			err:    "not OP_TRUE locked",
		},
		{
			name: "different asset root",
			mutate: func(c *commitResult) {
				c.outputs[0].taprootAssetRoot[0] ^= 1
			},
			amount: 800,
			err:    "leaf asset root",
		},
		{
			name: "asset outside the VTXO",
			mutate: func(c *commitResult) {
				c.outputs[0].anchorOutpoint.Index++
			},
			amount: 800,
			err:    "not in the VTXO",
		},
		{
			name: "no asset outputs",
			mutate: func(c *commitResult) {
				c.outputs = nil
			},
			amount: 800,
			err:    "no asset outputs",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			committed := cloneCommit(f.committed)
			if testCase.mutate != nil {
				testCase.mutate(committed)
			}
			err := checkLeafOutputs(
				committed, ref, testCase.amount, leafOutpoint,
				f.leafRoot(),
			)
			if testCase.err == "" {
				require.NoError(t, err)

				return
			}
			require.ErrorContains(t, err, testCase.err)
		})
	}
}

// TestOPTrueSpendBindsScriptKey checks the package validation that lets
// checkLeafOutputs trust an output's OP_TRUE script mode: the spend info must
// commit the OP_TRUE leaf to the output's script key.
func TestOPTrueSpendBindsScriptKey(t *testing.T) {
	t.Parallel()

	internal, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	opTrue := []byte{txscript.OP_TRUE}
	tapTree := txscript.AssembleTaprootScriptTree(
		txscript.NewBaseTapLeaf(opTrue),
	)
	root := tapTree.RootNode.TapHash()
	outputKey := txscript.ComputeTaprootOutputKey(
		internal.PubKey(), root[:],
	)
	proof := tapTree.LeafMerkleProofs[0].ToControlBlock(internal.PubKey())
	controlBlock, err := proof.ToBytes()
	require.NoError(t, err)

	var internalKey, scriptKey tapsdk.PubKey
	copy(internalKey[:], internal.PubKey().SerializeCompressed())
	copy(scriptKey[:], outputKey.SerializeCompressed())
	spend := &tapsdk.CustomAssetOPTrueSpendInfo{
		LeafScript:   opTrue,
		LeafVersion:  uint8(txscript.BaseLeafVersion),
		ControlBlock: controlBlock,
		InternalKey:  internalKey,
	}
	require.NoError(t, spend.Validate(scriptKey))

	other, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	copy(scriptKey[:], other.PubKey().SerializeCompressed())
	require.ErrorContains(
		t, spend.Validate(scriptKey),
		"verify OP_TRUE spend commitment",
	)
}

// TestCheckProvenOutput checks that a verified proof path must end in the
// exact package output.
func TestCheckProvenOutput(t *testing.T) {
	t.Parallel()

	f := newAssetVTXOFixture(t)
	ref := f.tree.mat.cfg.AssetRef
	output := f.committed.outputs[0]
	proven := tapsdk.AssetProofPathSummary{
		AssetRef:       output.assetRef,
		IssuanceID:     output.issuanceID,
		Amount:         output.amount,
		AnchorOutpoint: output.anchorOutpoint,
		ScriptKey:      output.scriptKey,
	}

	testCases := []struct {
		name   string
		mutate func(*tapsdk.AssetProofPathSummary)
		err    string
	}{
		{
			name: "package output",
		},
		{
			name: "different asset",
			mutate: func(s *tapsdk.AssetProofPathSummary) {
				s.AssetRef = tapsdk.AssetRefFromAssetID(
					tapsdk.AssetID{0xBB},
				)
			},
			err: "proven asset does not match",
		},
		{
			name: "different tranche",
			mutate: func(s *tapsdk.AssetProofPathSummary) {
				s.IssuanceID[0] ^= 1
			},
			err: "proven asset does not match",
		},
		{
			name: "different amount",
			mutate: func(s *tapsdk.AssetProofPathSummary) {
				s.Amount--
			},
			err: "proven amount",
		},
		{
			name: "different anchor",
			mutate: func(s *tapsdk.AssetProofPathSummary) {
				s.AnchorOutpoint.Index++
			},
			err: "not in the VTXO",
		},
		{
			name: "different script key",
			mutate: func(s *tapsdk.AssetProofPathSummary) {
				s.ScriptKey[1] ^= 1
			},
			err: "not the OP_TRUE key",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			summary := proven
			if testCase.mutate != nil {
				testCase.mutate(&summary)
			}
			err := checkProvenOutput(&summary, &output, ref)
			if testCase.err == "" {
				require.NoError(t, err)

				return
			}
			require.ErrorContains(t, err, testCase.err)
		})
	}

	require.ErrorContains(
		t, checkProvenOutput(nil, &output, ref),
		"no summary",
	)
}

// TestCheckPackageTx checks that a leaf accepts only the package that seals
// its own transaction.
func TestCheckPackageTx(t *testing.T) {
	t.Parallel()

	f := newAssetVTXOFixture(t)
	require.NoError(t, checkPackageTx(f.committed, f.leaf))

	other := f.tree.structure.Root.GetLeafNodes()
	var otherLeaf *tree.Node
	for _, leaf := range other {
		if leaf.Input != f.leaf.Input {
			otherLeaf = leaf
			break
		}
	}
	require.NotNil(t, otherLeaf)
	require.ErrorContains(
		t, checkPackageTx(f.committed, otherLeaf),
		"does not seal the asset VTXO leaf",
	)
}

// TestRoundProofVerifier checks that unconfirmed proof steps must anchor in
// round transactions on the VTXO's own path.
func TestRoundProofVerifier(t *testing.T) {
	t.Parallel()

	f := newAssetVTXOFixture(t)
	chain := &fakeProofChain{}
	verifier, err := newRoundProofVerifier(
		chain, f.commitmentTx, f.path, f.leafOutpoint,
	)
	require.NoError(t, err)

	rootTx, err := f.path.Root.ToTx()
	require.NoError(t, err)
	leafTx, err := f.leaf.ToTx()
	require.NoError(t, err)

	// The full tree's root has a child that is not on this path.
	require.Len(t, f.path.Root.Children, 1)
	var pathChild, offPath wire.OutPoint
	for _, child := range f.path.Root.Children {
		pathChild = child.Input
	}
	for idx := range f.tree.structure.Root.Children {
		child := wire.OutPoint{Hash: rootTx.TxHash(), Index: idx}
		if _, ok := f.path.Root.Children[idx]; !ok {
			offPath = child
		}
	}
	require.NotEqual(t, wire.OutPoint{}, offPath)

	var otherTx *wire.MsgTx
	for _, leaf := range f.tree.structure.Root.GetLeafNodes() {
		if leaf.Input == f.leaf.Input {
			continue
		}
		otherTx, err = leaf.ToTx()
		require.NoError(t, err)

		break
	}
	require.NotNil(t, otherTx)

	step := func(tx *wire.MsgTx,
		anchor wire.OutPoint) tapsdk.UnconfirmedAnchorVerification {

		return tapsdk.UnconfirmedAnchorVerification{
			AnchorOutpoint:    sdkOutpoint(anchor),
			AnchorTransaction: serializeTx(tx),
		}
	}
	testCases := []struct {
		name       string
		transition tapsdk.UnconfirmedAnchorVerification
		err        string
	}{
		{
			name:       "batch output",
			transition: step(f.commitmentTx, f.tree.batch),
		},
		{
			name:       "path child",
			transition: step(rootTx, pathChild),
		},
		{
			name:       "VTXO",
			transition: step(leafTx, f.leafOutpoint),
		},
		{
			name:       "off-path child",
			transition: step(rootTx, offPath),
			err:        "not on the VTXO path",
		},
		{
			name: "transaction outside the round",
			transition: step(otherTx, wire.OutPoint{
				Hash: otherTx.TxHash(),
			}),
			err: "is not in the round",
		},
		{
			name:       "anchor in another transaction",
			transition: step(f.commitmentTx, f.leafOutpoint),
			err:        "anchor is not in its transaction",
		},
		{
			name: "malformed transaction",
			transition: tapsdk.UnconfirmedAnchorVerification{
				AnchorTransaction: []byte{
					0x01,
				},
			},
			err: "decode step",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			err := verifier.VerifyUnconfirmedAnchor(
				t.Context(), testCase.transition,
			)
			if testCase.err == "" {
				require.NoError(t, err)

				return
			}
			require.ErrorContains(t, err, testCase.err)
		})
	}

	// A confirmed deposit is checked by the chain backend before its
	// inventory is derived from the proof.
	chain.err = errors.New("backend unavailable")
	_, err = verifier.VerifyConfirmedProof(t.Context(), []byte{0x01})
	require.ErrorIs(t, err, chain.err)

	chain.err = nil
	_, err = verifier.VerifyConfirmedProof(t.Context(), []byte{0x01})
	require.ErrorIs(t, err, tapsdk.ErrAssetProofPathInvalid)
	require.Equal(t, 2, chain.calls)
}

// TestVerifyLeafOutputProofInputs checks that the leaf transition must spend
// exactly one asset input for each of its outputs.
func TestVerifyLeafOutputProofInputs(t *testing.T) {
	t.Parallel()

	f := newAssetVTXOFixture(t)
	ref := f.tree.mat.cfg.AssetRef
	verifier, err := newRoundProofVerifier(
		&fakeProofChain{}, f.commitmentTx, f.path, f.leafOutpoint,
	)
	require.NoError(t, err)

	output := f.committed.outputs[0]
	input := commitInput{
		packetRole:  output.packetRole,
		packetIndex: output.packetIndex,
		proofSource: commitProofSource{
			kind: tapsdk.CustomAnchorProofSourceUnknown,
		},
	}

	testCases := []struct {
		name   string
		inputs []commitInput
		err    string
	}{
		{
			name: "no input",
			err:  "has no asset input",
		},
		{
			name: "merged inputs",
			inputs: []commitInput{
				input,
				input,
			},
			err: "merges asset inputs",
		},
		{
			name: "unknown proof source",
			inputs: []commitInput{
				input,
			},
			err: "unknown input proof source",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			committed := cloneCommit(f.committed)
			committed.inputs = testCase.inputs
			err := verifyLeafOutputProof(
				t.Context(), committed, &output, ref, verifier,
			)
			require.ErrorContains(t, err, testCase.err)
		})
	}
}

// TestVerifyAssetVTXORejectsMalformedInput checks the request checks that run
// before any package is decoded.
func TestVerifyAssetVTXORejectsMalformedInput(t *testing.T) {
	t.Parallel()

	_, err := NewVTXOVerifier(nil)
	require.ErrorContains(t, err, "proof chain verifier is required")

	f := newAssetVTXOFixture(t)
	verifier, err := NewVTXOVerifier(&fakeProofChain{})
	require.NoError(t, err)
	ref := f.tree.mat.cfg.AssetRef.String()

	noContext := *f.path
	noContext.AssetContext = nil

	testCases := []struct {
		name     string
		assetRef string
		tree     *tree.Tree
		pkg      []byte
		err      string
	}{
		{
			name:     "no asset context",
			assetRef: ref,
			tree:     &noContext,
			err:      "requires a commitment transaction",
		},
		{
			name:     "malformed asset reference",
			assetRef: "not-an-asset",
			tree:     f.path,
			err:      "asset VTXO reference",
		},
		{
			name:     "malformed package",
			assetRef: ref,
			tree:     f.path,
			pkg: []byte{
				0x01,
			},
			err: "decode tap-sdk transfer package",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			err := verifier.VerifyAssetVTXO(
				t.Context(), testCase.assetRef, 800,
				f.commitmentTx, testCase.tree, testCase.pkg,
			)
			require.ErrorContains(t, err, testCase.err)
		})
	}
}

// TestTapdProofChainVerifier checks that tapd verifies a proof file without
// writing to its universe when it knows the asset, and learns an unstored
// asset or an unknown group from the issuance proof or, for a reissuance,
// from its federation.
func TestTapdProofChainVerifier(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		client *fakeTapdProofClient
		calls  []string
		err    string
	}{
		{
			name: "known group",
			client: &fakeTapdProofClient{
				validFile: true, grouped: true,
				knownGroup: true,
			},
			calls: []string{
				"verify",
			},
		},
		{
			name: "ungrouped asset",
			client: &fakeTapdProofClient{
				validFile: true,
			},
			calls: []string{
				"verify",
			},
		},
		{
			name: "ungrouped asset on a fresh tapd",
			client: &fakeTapdProofClient{
				validFile: true, unstored: true,
			},
			calls: []string{
				"verify", "unpack", "decode", "insert",
				"verify",
			},
		},
		{
			name: "group anchor on a fresh tapd",
			client: &fakeTapdProofClient{
				validFile: true, grouped: true, anchor: true,
			},
			calls: []string{
				"verify", "unpack", "decode", "insert",
				"verify",
			},
		},
		{
			name: "reissuance on a fresh tapd",
			client: &fakeTapdProofClient{
				validFile: true, grouped: true,
				servers: []string{
					"uni-a",
					"uni-b",
				},
				groupServer: "uni-b",
			},
			calls: []string{
				"verify", "unpack", "decode", "insert", "list",
				"sync uni-a", "insert", "sync uni-b", "insert",
				"verify",
			},
		},
		{
			name: "reissuance without a federation",
			client: &fakeTapdProofClient{
				validFile: true, grouped: true,
			},
			err: "does not know asset group",
		},
		{
			name: "reissuance unknown to the federation",
			client: &fakeTapdProofClient{
				validFile: true, grouped: true,
				servers: []string{
					"uni-a",
				},
			},
			err: "uni-a: group key not known",
		},
		{
			name: "first proof is a transfer",
			client: &fakeTapdProofClient{
				transfer: true,
			},
			err: "first proof is not an issuance",
		},
		{
			name: "issuance rejected",
			client: &fakeTapdProofClient{
				insertErr: errors.New("bad issuance"),
			},
			err: "insert issuance proof: bad issuance",
		},
		{
			name:   "invalid proof file",
			client: &fakeTapdProofClient{},
			err:    "tapd rejected the proof file",
		},
		{
			name: "tapd unavailable",
			client: &fakeTapdProofClient{
				validFile: true,
				verifyErr: errors.New("tapd unavailable"),
				insertErr: errors.New("connection refused"),
			},
			err: "tapd unavailable",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			verifier := &TapdProofChainVerifier{
				client: testCase.client,
			}
			err := verifier.VerifyProofChain(
				t.Context(), []byte("proof-file"),
			)
			if testCase.err != "" {
				require.ErrorContains(t, err, testCase.err)

				return
			}
			require.NoError(t, err)
			require.Equal(t, testCase.calls, testCase.client.calls)
		})
	}

	_, err := NewTapdProofChainVerifier(nil)
	require.ErrorContains(t, err, "tapd client is required")
}

type fakeProofChain struct {
	err   error
	calls int
}

func (f *fakeProofChain) VerifyProofChain(context.Context, []byte) error {
	f.calls++

	return f.err
}

// fakeTapdProofClient models tapd's group rule: a grouped asset verifies only
// once tapd knows its group, which a group anchor issuance or a federation
// sync teaches it. An unstored asset models tapd failing to describe a valid
// proof of an asset it has not stored, until its issuance is inserted.
type fakeTapdProofClient struct {
	validFile   bool
	unstored    bool
	verifyErr   error
	grouped     bool
	anchor      bool
	transfer    bool
	knownGroup  bool
	servers     []string
	groupServer string
	insertErr   error
	calls       []string
}

func (f *fakeTapdProofClient) UnpackProofFile(_ context.Context,
	proofFile []byte) ([][]byte, error) {

	f.calls = append(f.calls, "unpack")

	return [][]byte{[]byte("issuance"), bytes.Clone(proofFile)}, nil
}

func (f *fakeTapdProofClient) DecodeProof(context.Context, []byte) (
	*tapsdk.DecodedProof, error) {

	f.calls = append(f.calls, "decode")

	ref := tapsdk.AssetRefFromAssetID(tapsdk.AssetID{0x01})
	if f.grouped {
		_, groupKey := btcec.PrivKeyFromBytes([]byte{0x01})
		var key tapsdk.PubKey
		copy(key[:], groupKey.SerializeCompressed())
		ref = tapsdk.AssetRefFromGroupKey(key)
	}

	return &tapsdk.DecodedProof{
		AssetRef:   ref,
		IsIssuance: !f.transfer,
	}, nil
}

func (f *fakeTapdProofClient) InsertProof(context.Context, []byte,
	*tapsdk.DecodedProof) error {

	f.calls = append(f.calls, "insert")

	switch {
	case f.insertErr != nil:
		return f.insertErr

	case f.grouped && !f.knownGroup && !f.anchor:
		return errors.New("group key not known")
	}
	f.knownGroup = true
	f.unstored = false

	return nil
}

func (f *fakeTapdProofClient) VerifyProof(context.Context, []byte) (
	*tapsdk.VerifyProofResponse, error) {

	f.calls = append(f.calls, "verify")

	switch {
	case f.verifyErr != nil:
		return nil, f.verifyErr

	case f.unstored:
		return nil, errors.New("unable to marshal proof: unable to " +
			"fetch asset meta")
	}

	return &tapsdk.VerifyProofResponse{
		Valid: f.validFile && (!f.grouped || f.knownGroup),
	}, nil
}

func (f *fakeTapdProofClient) ListFederationServers(context.Context) (
	[]tapsdk.FederationServer, error) {

	f.calls = append(f.calls, "list")

	servers := make([]tapsdk.FederationServer, 0, len(f.servers))
	for _, host := range f.servers {
		servers = append(servers, tapsdk.FederationServer{Host: host})
	}

	return servers, nil
}

func (f *fakeTapdProofClient) SyncUniverse(_ context.Context,
	req *tapsdk.SyncRequest) ([]tapsdk.SyncedUniverse, error) {

	f.calls = append(f.calls, "sync "+req.UniverseHost)
	if req.UniverseHost == f.groupServer {
		f.knownGroup = true
	}

	return nil, nil
}
