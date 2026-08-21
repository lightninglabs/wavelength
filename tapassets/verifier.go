package tapassets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"

	tapsdk "github.com/lightninglabs/tap-sdk"
)

type proofInventoryClient interface {
	VerifyProof(context.Context,
		[]byte) (*tapsdk.VerifyProofResponse, error)

	ListUtxos(context.Context,
		*tapsdk.ListUtxosRequest) (
		map[string]*tapsdk.ManagedUtxo,
		error,
	)
}

// proofLineageClient adds the public Universe operations needed by a fresh
// receiver to learn the issuance that anchors a grouped asset proof. It
// deliberately omits wallet transfer registration because Wavelength's
// unconfirmed OP_TRUE asset is not owned by the receiver's tapd wallet.
type proofLineageClient interface {
	VerifyProof(context.Context,
		[]byte) (*tapsdk.VerifyProofResponse, error)

	UnpackProofFile(context.Context, []byte) ([][]byte, error)

	DecodeProof(context.Context, []byte) (*tapsdk.DecodedProof, error)

	InsertProof(context.Context, []byte, *tapsdk.DecodedProof) error
}

type expectedUnconfirmedAnchor struct {
	previousOutpoint tapsdk.Outpoint
	anchorOutpoint   tapsdk.Outpoint
	transaction      []byte
}

type proofInventoryVerifier struct {
	client    proofInventoryClient
	assetRef  tapsdk.AssetRef
	amount    uint64
	anchor    tapsdk.Outpoint
	assetRoot tapsdk.Hash
}

// VerifyConfirmedProof asks tapd to verify the proof chain and then binds its
// tip to tapd's complete managed-anchor inventory. Compact unconfirmed paths
// are only safe when that confirmed anchor contains no passive assets.
func (v *proofInventoryVerifier) VerifyConfirmedProof(ctx context.Context,
	proofFile []byte) (*tapsdk.ConfirmedProofVerification, error) {

	if v == nil || v.client == nil {
		return nil, fmt.Errorf("tapd proof inventory client is " +
			"required")
	}

	verified, err := v.client.VerifyProof(ctx, proofFile)
	if err != nil {
		return nil, fmt.Errorf("verify confirmed proof with tapd: %w",
			err)
	}
	if verified == nil || !verified.Valid || verified.DecodedProof == nil {
		return nil, fmt.Errorf("tapd rejected confirmed proof")
	}
	tip := verified.DecodedProof
	if !tip.AssetRef.Equivalent(v.assetRef) || tip.Amount != v.amount ||
		tip.Outpoint != v.anchor {
		return nil, fmt.Errorf("confirmed proof tip does not match " +
			"OOR input")
	}

	utxos, err := v.client.ListUtxos(ctx, &tapsdk.ListUtxosRequest{
		IncludeLeased: true,
	})
	if err != nil {
		return nil, fmt.Errorf("list tapd anchor inventory: %w", err)
	}
	var anchor *tapsdk.ManagedUtxo
	for _, candidate := range utxos {
		if candidate != nil && candidate.OutPoint == v.anchor {
			anchor = candidate
			break
		}
	}
	if anchor == nil {
		return nil, fmt.Errorf("confirmed proof anchor is not " +
			"managed by tapd")
	}
	if anchor.TaprootAssetRoot != v.assetRoot {
		return nil, fmt.Errorf("tapd asset root does not match " +
			"Wavelength VTXO")
	}
	if len(anchor.Assets) == 0 {
		return nil, fmt.Errorf("tapd anchor inventory is empty")
	}

	var selected int
	for _, asset := range anchor.Assets {
		if asset == nil {
			continue
		}
		if asset.Genesis.IssuanceID == tip.IssuanceID &&
			asset.Amount == tip.Amount &&
			asset.ScriptKey.PubKey == tip.ScriptKey {

			selected++
		}
	}
	if selected != 1 {
		return nil, fmt.Errorf("tapd anchor inventory matched "+
			"selected asset %d times", selected)
	}

	return &tapsdk.ConfirmedProofVerification{
		AnchorAssetInventoryComplete: true,
		PassiveAssetCount:            uint32(len(anchor.Assets) - 1),
	}, nil
}

// proofLineageVerifier verifies a chained path's confirmed base with tapd and
// delegates cryptographic validation of every sealed unconfirmed step to
// tap-sdk. The exact package that created the local VTXO is the authority for
// passive isolation; a receiver must not need the sender's base anchor in its
// own ListUtxos inventory.
type proofLineageVerifier struct {
	client proofLineageClient
}

// VerifyConfirmedProof verifies the confirmed base through tapd. Complete
// passive isolation is inherited from the exact, operator-accepted package
// lineage that supplied the compact path.
func (v *proofLineageVerifier) VerifyConfirmedProof(ctx context.Context,
	proofFile []byte) (*tapsdk.ConfirmedProofVerification, error) {

	if v == nil || v.client == nil {
		return nil, fmt.Errorf("tapd proof client is required")
	}
	if err := bootstrapProofIssuance(ctx, v.client, proofFile); err != nil {
		return nil, fmt.Errorf("bootstrap chained proof issuance: %w",
			err)
	}
	verified, err := v.client.VerifyProof(ctx, proofFile)
	if err != nil {
		return nil, fmt.Errorf("verify chained proof base with "+
			"tapd: %w", err)
	}
	if verified == nil || !verified.Valid || verified.DecodedProof == nil {
		return nil, fmt.Errorf("tapd rejected chained proof base")
	}

	return &tapsdk.ConfirmedProofVerification{
		AnchorAssetInventoryComplete: true,
		PassiveAssetCount:            0,
	}, nil
}

// bootstrapProofIssuance teaches a fresh receiver's local Universe about the
// public issuance at the start of the received proof chain. InsertProof
// validates and idempotently persists that issuance. Importing or registering
// the later transfer proofs would incorrectly claim the sender's asset in the
// receiver's tapd wallet.
func bootstrapProofIssuance(ctx context.Context, client proofLineageClient,
	proofFile []byte) error {

	rawProofs, err := client.UnpackProofFile(ctx, proofFile)
	if err != nil {
		return fmt.Errorf("unpack proof file: %w", err)
	}
	if len(rawProofs) == 0 {
		return fmt.Errorf("proof file contains no proofs")
	}
	if len(rawProofs[0]) == 0 {
		return fmt.Errorf("issuance proof is empty")
	}

	issuance, err := client.DecodeProof(ctx, rawProofs[0])
	if err != nil {
		return fmt.Errorf("decode issuance proof: %w", err)
	}
	if issuance == nil {
		return fmt.Errorf("decoded issuance proof is empty")
	}
	if !issuance.IsIssuance {
		return fmt.Errorf("first proof is not an issuance")
	}
	if err := client.InsertProof(ctx, rawProofs[0], issuance); err != nil {
		return fmt.Errorf("insert issuance proof: %w", err)
	}

	return nil
}

// VerifyUnconfirmedAnchor accepts sealed historical steps after tap-sdk has
// verified their transactions and asset transitions. Binding a freshly
// appended checkpoint step to Wavelength's exact committed transaction is
// the transition verifier's job, which routes by anchor edge instead of the
// per-path step index that repeats inside co-input paths.
func (v *proofLineageVerifier) VerifyUnconfirmedAnchor(context.Context,
	tapsdk.UnconfirmedAnchorVerification) error {

	return nil
}

// assetAnchorEdge identifies one unconfirmed transition by the asset-bearing
// outpoint it consumes and the one it creates. Every outpoint is consumed at
// most once across the transfer DAG, so the pair routes verification
// callbacks unambiguously, unlike per-path step indices, which repeat inside
// co-input paths.
type assetAnchorEdge struct {
	previous tapsdk.Outpoint
	anchor   tapsdk.Outpoint
}

// assetTransitionVerifier fans one SDK verifier callback across every asset
// input of the Ark transition build. Confirmed base proofs route to their
// input's own verifier by exact content, failing closed on proofs outside
// the declared set. Appended checkpoint steps are bound to the committed
// checkpoint transactions by their (previous, anchor) edge; steps matching
// no edge are historical, already cryptographically verified by tap-sdk and
// bound to their transactions when the packages that created them committed.
type assetTransitionVerifier struct {
	confirmed map[[sha256.Size]byte]tapsdk.ConfirmedProofVerifier
	expected  map[assetAnchorEdge]*expectedUnconfirmedAnchor
}

// newAssetTransitionVerifier indexes the sources' confirmed bases, including
// every base inside recursive co-input paths, and the expected checkpoint
// anchor edges of the pending Ark build.
func newAssetTransitionVerifier(sources []*assetSpendSource,
	expected []*expectedUnconfirmedAnchor) (*assetTransitionVerifier,
	error) {

	verifier := &assetTransitionVerifier{
		confirmed: make(
			map[[sha256.Size]byte]tapsdk.ConfirmedProofVerifier,
		),
		expected: make(
			map[assetAnchorEdge]*expectedUnconfirmedAnchor,
			len(expected),
		),
	}
	for idx, source := range sources {
		if source == nil || source.verifier == nil {
			return nil, fmt.Errorf("asset source %d verifier is "+
				"required", idx)
		}
		if len(source.proofFile) != 0 {
			verifier.addConfirmedBase(
				source.proofFile, source.verifier,
			)
		}
		if source.proofPath != nil {
			verifier.addPathBases(
				source.proofPath, source.verifier,
			)
		}
	}
	for _, entry := range expected {
		edge := assetAnchorEdge{
			previous: entry.previousOutpoint,
			anchor:   entry.anchorOutpoint,
		}
		if _, ok := verifier.expected[edge]; ok {
			return nil, fmt.Errorf("duplicate expected "+
				"anchor edge %v", edge)
		}
		verifier.expected[edge] = entry
	}

	return verifier, nil
}

// addPathBases registers every confirmed base of the path's co-input tree.
// Inputs sharing one base may overwrite each other: chained verifiers are
// scoped to the proof file itself, never to a particular input.
func (v *assetTransitionVerifier) addPathBases(path *tapsdk.AssetProofPath,
	verifier tapsdk.ConfirmedProofVerifier) {

	v.addConfirmedBase(path.ConfirmedBaseProof, verifier)
	for _, base := range path.AdditionalBaseProofs {
		v.addConfirmedBase(base, verifier)
	}
	for i := range path.Steps {
		for _, coPath := range path.Steps[i].CoInputPaths {
			v.addPathBases(coPath, verifier)
		}
	}
}

// addConfirmedBase indexes one confirmed proof file by content.
func (v *assetTransitionVerifier) addConfirmedBase(base []byte,
	verifier tapsdk.ConfirmedProofVerifier) {

	if len(base) == 0 {
		return
	}
	v.confirmed[sha256.Sum256(base)] = verifier
}

// VerifyConfirmedProof routes one confirmed base to the verifier of the
// input that declared it.
func (v *assetTransitionVerifier) VerifyConfirmedProof(ctx context.Context,
	proofFile []byte) (*tapsdk.ConfirmedProofVerification, error) {

	delegate, ok := v.confirmed[sha256.Sum256(proofFile)]
	if !ok {
		return nil, fmt.Errorf("confirmed base proof does not belong " +
			"to this transfer")
	}

	return delegate.VerifyConfirmedProof(ctx, proofFile)
}

// VerifyUnconfirmedAnchor binds an appended checkpoint step to the exact
// committed checkpoint transaction. tap-sdk derives the presented outpoints
// from the step's own proof, so a substituted transition cannot reach its
// expected edge without also failing the SDK's continuity checks.
func (v *assetTransitionVerifier) VerifyUnconfirmedAnchor(_ context.Context,
	transition tapsdk.UnconfirmedAnchorVerification) error {

	edge := assetAnchorEdge{
		previous: transition.PreviousAnchorOutpoint,
		anchor:   transition.AnchorOutpoint,
	}
	expected, ok := v.expected[edge]
	if !ok {
		return nil
	}

	// A checkpoint transition consumes exactly its single predecessor;
	// a merging step can never bind to a checkpoint edge.
	if len(transition.PreviousAnchorOutpoints) > 1 {
		return fmt.Errorf("checkpoint transition consumes %d anchors, "+
			"want one", len(transition.PreviousAnchorOutpoints))
	}
	if len(transition.PreviousAnchorOutpoints) == 1 &&
		transition.PreviousAnchorOutpoints[0] !=
			expected.previousOutpoint {
		return fmt.Errorf("checkpoint transition previous anchor " +
			"mismatch")
	}
	if !bytes.Equal(transition.AnchorTransaction, expected.transaction) {
		return fmt.Errorf("unconfirmed proof anchor transaction " +
			"mismatch")
	}

	return nil
}
