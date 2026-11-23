package waved

import (
	"context"
	"errors"
	"testing"

	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/tap-sdk/embedded"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeProofBackend is an asset backend that imports a proof file of two
// proofs. It refuses to verify the file unless valid is set.
type fakeProofBackend struct {
	tapsdk.Client

	tip   *tapsdk.DecodedProof
	valid bool

	verified   int
	inserted   int
	registered int
}

// UnpackProofFile returns an issuance proof and a transfer proof.
func (f *fakeProofBackend) UnpackProofFile(context.Context, []byte) ([][]byte,
	error) {

	return [][]byte{{1}, {2}}, nil
}

// DecodeProof returns the scripted proof.
func (f *fakeProofBackend) DecodeProof(context.Context, []byte) (
	*tapsdk.DecodedProof, error) {

	return f.tip, nil
}

// VerifyProof accepts the file when valid is set, and otherwise refuses it
// as a file whose group the backend does not trust.
func (f *fakeProofBackend) VerifyProof(context.Context, []byte) (
	*tapsdk.VerifyProofResponse, error) {

	f.verified++
	if !f.valid {
		return nil, errors.New("group key is not known")
	}

	return &tapsdk.VerifyProofResponse{Valid: true}, nil
}

// InsertProof counts an inserted proof.
func (f *fakeProofBackend) InsertProof(context.Context, []byte,
	*tapsdk.DecodedProof) error {

	f.inserted++

	return nil
}

// RegisterTransfer counts a registered transfer.
func (f *fakeProofBackend) RegisterTransfer(context.Context, tapsdk.AssetRef,
	tapsdk.PubKey, tapsdk.Outpoint) (*tapsdk.RegisteredAsset, error) {

	f.registered++

	return &tapsdk.RegisteredAsset{}, nil
}

// ListUtxos returns no managed output.
func (f *fakeProofBackend) ListUtxos(context.Context,
	*tapsdk.ListUtxosRequest) (map[string]*tapsdk.ManagedUtxo, error) {

	return nil, nil
}

// newProofImportFixture returns an RPC server on a fake asset backend and
// the request that imports the backend's proof.
func newProofImportFixture(t *testing.T) (*RPCServer, *fakeProofBackend,
	*waverpc.ImportAssetProofRequest) {

	t.Helper()

	groupKeys, err := (&AssetsConfig{
		GroupKeys: []string{
			testGroupKeyHex,
		},
	}).groupKeys()
	require.NoError(t, err)
	ref := tapsdk.AssetRefFromGroupKey(groupKeys[0])

	backend := &fakeProofBackend{
		tip: &tapsdk.DecodedProof{
			AssetRef: ref,
			IssuanceID: tapsdk.AssetID{
				7,
			},
			Amount: 21,
		},
	}
	server := newAssetChainTestServer(nil)
	server.assetClient = backend

	return &RPCServer{server: server}, backend,
		&waverpc.ImportAssetProofRequest{
			ProofFile: []byte{
				1,
			},
			AssetRef: ref.String(),
		}
}

// TestImportAssetProof checks that a proof file that verifies is imported:
// the backend receives its proofs and registers the transfer.
func TestImportAssetProof(t *testing.T) {
	t.Parallel()

	rpcServer, backend, req := newProofImportFixture(t)
	backend.valid = true

	owned, err := rpcServer.ImportAssetProof(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, req.AssetRef, owned.AssetRef)
	require.EqualValues(t, 21, owned.Amount)

	require.Equal(t, 1, backend.verified)
	require.Equal(t, 2, backend.inserted)
	require.Equal(t, 1, backend.registered)
}

// TestImportAssetProofWrongAsset checks that a proof of another asset than
// the caller named is refused before anything is inserted.
func TestImportAssetProofWrongAsset(t *testing.T) {
	t.Parallel()

	rpcServer, backend, req := newProofImportFixture(t)
	req.AssetRef = tapsdk.AssetRefFromAssetID(tapsdk.AssetID{9}).String()

	_, err := rpcServer.ImportAssetProof(t.Context(), req)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Zero(t, backend.inserted)
	require.Zero(t, backend.registered)
}

// TestImportAssetProofVerifiesFirst checks that the asset backend is given
// no proof of a file that does not verify.
func TestImportAssetProofVerifiesFirst(t *testing.T) {
	t.Parallel()

	rpcServer, backend, req := newProofImportFixture(t)

	engine, err := embedded.NewClient(&embedded.Config{
		Network: tapsdk.NetworkRegtest,
		Store:   embedded.NewMemoryStore(),
		KeyRing: &assetKeyRing{server: rpcServer.server},
		Signer:  &assetKeyRing{server: rpcServer.server},
		Chain:   &assetChain{server: rpcServer.server},
	})
	require.NoError(t, err)
	rpcServer.server.embeddedAssets = engine

	_, err = rpcServer.ImportAssetProof(t.Context(), req)
	require.Error(t, err)
	require.Equal(t, 1, backend.verified)
	require.Zero(t, backend.inserted)
	require.Zero(t, backend.registered)
}
