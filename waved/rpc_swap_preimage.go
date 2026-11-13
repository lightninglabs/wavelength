package waved

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/lib/swaprecovery"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/keychain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SignIdentitySchnorr signs only recovery owner proofs. Restricting the tag
// prevents swap permissions from becoming a transaction-signing oracle.
func (r *RPCServer) SignIdentitySchnorr(ctx context.Context,
	req *waverpc.SignIdentitySchnorrRequest) (
	*waverpc.SignIdentitySchnorrResponse, error) {

	if !bytes.Equal(req.GetTag(), []byte(swaprecovery.OwnerProofTag)) {
		return nil, status.Error(
			codes.InvalidArgument,
			"unsupported identity signing tag",
		)
	}
	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	sig, err := r.server.signTaggedSchnorr(
		ctx, req.GetMessage(), req.GetTag(),
		"identity schnorr",
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign identity "+
			"schnorr: %v", err)
	}

	return &waverpc.SignIdentitySchnorrResponse{
		Signature: sig.Serialize(),
	}, nil
}

// DeriveSwapPreimage reproduces an invoice preimage using only public
// recovery parameters and the active wallet's seed. In lnd mode the private
// key stays in lnd and only the ECDH result crosses the signer RPC boundary.
func (r *RPCServer) DeriveSwapPreimage(ctx context.Context,
	req *waverpc.DeriveSwapPreimageRequest) (
	*waverpc.DeriveSwapPreimageResponse, error) {

	loc, err := swaprecovery.ParseParams(req.GetRecoveryParams())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}
	if r.server.chainParams == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"wallet network is unavailable",
		)
	}

	sharedSecret, err := r.swapRecoverySharedSecret(ctx, loc)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "derive swap "+
			"recovery secret: %v", err)
	}
	preimage, err := swaprecovery.Derive(
		req.GetRecoveryParams(), *r.server.chainParams.GenesisHash,
		sharedSecret,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "derive swap "+
			"preimage: %v", err)
	}

	return &waverpc.DeriveSwapPreimageResponse{
		Preimage: preimage[:],
	}, nil
}

// swapRecoverySharedSecret performs self-ECDH in the selected wallet. The
// peer point must be the wallet-derived public key: constructing it as a
// publicly known scalar times G would make the result publicly computable.
func (r *RPCServer) swapRecoverySharedSecret(ctx context.Context,
	loc keychain.KeyLocator) ([32]byte, error) {

	switch r.server.cfg.Wallet.Type {
	case WalletTypeLnd:
		if !r.server.lnd.IsSome() {
			return [32]byte{}, fmt.Errorf("lnd wallet not " +
				"connected")
		}
		svc := r.server.lnd.UnsafeFromSome()
		desc, err := svc.WalletKit.DeriveKey(ctx, &loc)
		if err != nil {
			return [32]byte{}, err
		}
		if desc == nil || desc.PubKey == nil {
			return [32]byte{}, fmt.Errorf("missing recovery " +
				"public key")
		}

		return svc.Signer.DeriveSharedKey(ctx, desc.PubKey, &loc)

	case WalletTypeLwwallet:
		if !r.server.lwWallet.IsSome() {
			return [32]byte{}, fmt.Errorf("lwwallet not " +
				"initialized")
		}

		return recoveryKeyRingECDH(
			r.server.lwWallet.UnsafeFromSome().KeyRing(), loc,
		)

	case WalletTypeBtcwallet:
		if !r.server.btcwWallet.IsSome() {
			return [32]byte{}, fmt.Errorf("btcwallet not " +
				"initialized")
		}

		return recoveryKeyRingECDH(
			r.server.btcwWallet.UnsafeFromSome().KeyRing(), loc,
		)

	default:
		return [32]byte{}, fmt.Errorf("swap preimage derivation "+
			"unsupported for wallet type %q",
			r.server.cfg.Wallet.Type)
	}
}

// recoveryECDHKeyRing is the minimum local-wallet surface needed for
// preimage derivation; no private-key export operation is required.
type recoveryECDHKeyRing interface {
	DeriveKey(keychain.KeyLocator) (keychain.KeyDescriptor, error)

	ECDH(keychain.KeyDescriptor, *btcec.PublicKey) ([32]byte, error)
}

// recoveryKeyRingECDH matches lnd's DeriveKey + DeriveSharedKey RPC sequence
// using the equivalent local key-ring methods.
func recoveryKeyRingECDH(ring recoveryECDHKeyRing,
	loc keychain.KeyLocator) ([32]byte, error) {

	desc, err := ring.DeriveKey(loc)
	if err != nil {
		return [32]byte{}, err
	}
	if desc.PubKey == nil {
		return [32]byte{}, fmt.Errorf("missing recovery public key")
	}

	return ring.ECDH(desc, desc.PubKey)
}
