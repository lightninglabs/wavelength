package tapassets

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
)

type fakeInventory struct {
	verification *tapsdk.VerifyProofResponse

	// verifications resolves a proof file to its tip by content, for
	// fixtures holding the asset across more than one anchor. It wins
	// over verification whenever it has an entry.
	verifications map[string]*tapsdk.VerifyProofResponse

	utxos map[string]*tapsdk.ManagedUtxo
	err   error

	// derived counts key derivations, so a test can prove a pinned key is
	// derived exactly once.
	derived int
}

func (f *fakeInventory) VerifyProof(_ context.Context, proofFile []byte) (
	*tapsdk.VerifyProofResponse, error) {

	if f.err != nil {
		return nil, f.err
	}
	if verification, ok := f.verifications[string(proofFile)]; ok {
		return verification, nil
	}

	return f.verification, nil
}

// DeriveScriptKey returns a deterministic wallet script key, distinct on
// every call so a test can tell a pinned key from a re-derived one.
func (f *fakeInventory) DeriveScriptKey(context.Context) (*tapsdk.ScriptKey,
	error) {

	if f.err != nil {
		return nil, f.err
	}
	f.derived++
	seed := sha256.Sum256([]byte(fmt.Sprintf("script-key-%d", f.derived)))
	_, pubKey := btcec.PrivKeyFromBytes(seed[:])
	key, err := tapsdk.ParsePubKey(pubKey.SerializeCompressed())
	if err != nil {
		return nil, err
	}

	return &tapsdk.ScriptKey{
		PubKey: key,
		KeyDesc: tapsdk.KeyDescriptor{
			RawKeyBytes: key,
			KeyLocator: tapsdk.KeyLocator{
				Family: 212,
				Index:  uint32(f.derived),
			},
		},
	}, nil
}

// DeriveInternalKey returns a deterministic wallet anchor internal key,
// distinct on every call.
func (f *fakeInventory) DeriveInternalKey(context.Context) (*tapsdk.InternalKey,
	error) {

	if f.err != nil {
		return nil, f.err
	}
	f.derived++
	seed := sha256.Sum256([]byte(fmt.Sprintf("internal-key-%d", f.derived)))
	_, pubKey := btcec.PrivKeyFromBytes(seed[:])
	key, err := tapsdk.ParsePubKey(pubKey.SerializeCompressed())
	if err != nil {
		return nil, err
	}

	return &tapsdk.InternalKey{
		PubKey: key,
		KeyLocator: tapsdk.KeyLocator{
			Family: 213,
			Index:  uint32(f.derived),
		},
	}, nil
}

func (f *fakeInventory) ListUtxos(context.Context, *tapsdk.ListUtxosRequest) (
	map[string]*tapsdk.ManagedUtxo, error) {

	if f.err != nil {
		return nil, f.err
	}

	return f.utxos, nil
}

// onlyAnchor returns the sole managed anchor in this fixture.
func (f *fakeInventory) onlyAnchor() *tapsdk.ManagedUtxo {
	for _, anchor := range f.utxos {
		return anchor
	}

	return nil
}

func cloneCommitResult(result *commitResult) *commitResult {
	clone := *result
	clone.packageBytes = append([]byte(nil), result.packageBytes...)
	clone.anchorPSBT = append([]byte(nil), result.anchorPSBT...)
	clone.inputs = append([]commitInput(nil), result.inputs...)
	for idx := range clone.inputs {
		clone.inputs[idx].proofSource.blob = append(
			[]byte(nil), result.inputs[idx].proofSource.blob...,
		)
	}
	clone.outputs = append([]commitOutput(nil), result.outputs...)
	for idx := range clone.outputs {
		clone.outputs[idx].opTrueWitness = cloneByteSlices(
			result.outputs[idx].opTrueWitness,
		)
		clone.outputs[idx].proofBlob = append(
			[]byte(nil), result.outputs[idx].proofBlob...,
		)
	}

	return &clone
}
