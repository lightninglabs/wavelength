package waved

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/lib/swaprecovery"
	"github.com/lightninglabs/wavelength/waverpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSignIdentitySchnorrRejectsForeignDomains prevents a swap macaroon from
// obtaining transaction or unrelated protocol signatures from the identity key.
func TestSignIdentitySchnorrRejectsForeignDomains(t *testing.T) {
	t.Parallel()

	rpc := &RPCServer{server: &Server{}}
	for _, tag := range []string{"", "TapSighash", "other-protocol"} {
		_, err := rpc.SignIdentitySchnorr(
			t.Context(), &waverpc.SignIdentitySchnorrRequest{
				Tag: []byte(tag), Message: []byte("message"),
			},
		)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
	_, err := rpc.SignIdentitySchnorr(t.Context(), nil)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// recoveryTestRing implements only public derivation and ECDH so using any
// private-key-export API is impossible in this fixture.
type recoveryTestRing struct {
	t    *testing.T
	priv *btcec.PrivateKey
	loc  keychain.KeyLocator
}

// DeriveKey verifies the requested recovery locator and returns its public key.
func (r *recoveryTestRing) DeriveKey(loc keychain.KeyLocator) (
	keychain.KeyDescriptor, error) {

	require.Equal(r.t, r.loc, loc)

	return keychain.KeyDescriptor{
		KeyLocator: loc, PubKey: r.priv.PubKey(),
	}, nil
}

// ECDH checks self-ECDH and computes lnd's compressed-point SHA256 result.
func (r *recoveryTestRing) ECDH(desc keychain.KeyDescriptor,
	peer *btcec.PublicKey) ([32]byte, error) {

	require.Equal(r.t, r.loc, desc.KeyLocator)
	require.True(r.t, r.priv.PubKey().IsEqual(peer))

	return receiveAuthECDH(r.priv, peer), nil
}

type recoveryTestWalletKit struct {
	lndclient.WalletKitClient
	ring *recoveryTestRing
}

// DeriveKey implements the external-wallet public derivation RPC.
func (w *recoveryTestWalletKit) DeriveKey(_ context.Context,
	loc *keychain.KeyLocator) (*keychain.KeyDescriptor, error) {

	desc, err := w.ring.DeriveKey(*loc)

	return &desc, err
}

type recoveryTestSigner struct {
	lndclient.SignerClient
	ring *recoveryTestRing
}

// DeriveSharedKey checks the RPC arguments and returns lnd-format ECDH bytes.
func (s *recoveryTestSigner) DeriveSharedKey(_ context.Context,
	peer *btcec.PublicKey, loc *keychain.KeyLocator) ([32]byte, error) {

	return s.ring.ECDH(keychain.KeyDescriptor{KeyLocator: *loc}, peer)
}

// TestDeriveSwapPreimageLnd verifies that external lnd and the local key-ring
// adapter produce the same preimage, without private key derivation/export.
func TestDeriveSwapPreimageLnd(t *testing.T) {
	t.Parallel()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	params, err := swaprecovery.NewParams(7)
	require.NoError(t, err)
	loc, err := swaprecovery.ParseParams(params)
	require.NoError(t, err)
	ring := &recoveryTestRing{t: t, priv: priv, loc: loc}
	svc := &lndclient.GrpcLndServices{
		LndServices: lndclient.LndServices{
			WalletKit: &recoveryTestWalletKit{
				ring: ring,
			},
			Signer: &recoveryTestSigner{
				ring: ring,
			},
		},
	}
	ready := make(chan struct{})
	close(ready)
	rpc := &RPCServer{server: &Server{
		cfg: &Config{
			Wallet: &WalletConfig{
				Type: WalletTypeLnd,
			},
		},
		lnd: fn.Some(svc), walletReady: ready,
		chainParams: &chaincfg.RegressionNetParams,
	}}
	resp, err := rpc.DeriveSwapPreimage(
		t.Context(), &waverpc.DeriveSwapPreimageRequest{
			RecoveryParams: params,
		},
	)
	require.NoError(t, err)
	base, err := recoveryKeyRingECDH(ring, loc)
	require.NoError(t, err)
	want, err := swaprecovery.Derive(
		params, *chaincfg.RegressionNetParams.GenesisHash, base,
	)
	require.NoError(t, err)
	require.Equal(t, want[:], resp.GetPreimage())

	// A new RPC instance with the same restored wallet can reproduce it.
	restored := &RPCServer{server: rpc.server}
	again, err := restored.DeriveSwapPreimage(
		t.Context(), &waverpc.DeriveSwapPreimageRequest{
			RecoveryParams: params,
		},
	)
	require.NoError(t, err)
	require.Equal(t, resp.GetPreimage(), again.GetPreimage())
}

// TestDeriveSwapPreimageRejectsInvalid verifies malformed metadata is rejected
// before wallet operations and a locked wallet cannot reveal a preimage.
func TestDeriveSwapPreimageRejectsInvalid(t *testing.T) {
	t.Parallel()

	rpc := &RPCServer{server: &Server{}}
	_, err := rpc.DeriveSwapPreimage(t.Context(), nil)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	params, err := swaprecovery.NewParams(0)
	require.NoError(t, err)
	_, err = rpc.DeriveSwapPreimage(
		t.Context(), &waverpc.DeriveSwapPreimageRequest{
			RecoveryParams: params,
		},
	)
	require.Error(t, err)
}
