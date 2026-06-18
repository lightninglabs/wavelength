package swaps

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/swaprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"golang.org/x/crypto/nacl/secretbox"
)

const (
	swapRecoveryAuthTag = "darepo-swap-recovery-owner-v1"

	swapRecoveryAuthList           = "list-recoverable-swaps"
	swapRecoveryAuthCreateIn       = "create-in-swap"
	swapRecoveryAuthRequestChannel = "request-channel-id"

	swapRecoveryNonceLen    = 16
	swapRecoveryBlobVersion = 1
	swapRecoveryBlobTag     = "darepo-swap-recovery-blob-v1"
)

// newSwapOwnerProof signs one swap recovery owner proof with the daemon
// identity key.
func newSwapOwnerProof(ctx context.Context, daemon DaemonConn,
	clientKey *btcec.PublicKey, kind string, timestampUnix int64,
	fields ...[]byte) (*swaprpc.SwapOwnerProof, error) {

	if daemon == nil {
		return nil, fmt.Errorf("daemon connection is required")
	}
	if clientKey == nil {
		return nil, fmt.Errorf("client identity key is required")
	}

	nonce := make([]byte, swapRecoveryNonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate owner proof nonce: %w", err)
	}

	clientKeyBytes := clientKey.SerializeCompressed()
	msg := swapRecoveryAuthMessage(
		kind, clientKeyBytes, timestampUnix, nonce, fields...,
	)
	sig, err := daemon.SignIdentitySchnorr(
		ctx, msg, []byte(swapRecoveryAuthTag),
	)
	if err != nil {
		return nil, fmt.Errorf("sign owner proof: %w", err)
	}

	return &swaprpc.SwapOwnerProof{
		ClientIdentityPubkey: clientKeyBytes,
		TimestampUnix:        timestampUnix,
		Nonce:                nonce,
		Signature:            sig,
	}, nil
}

// swapRecoveryAuthMessage builds the length-delimited message signed by the
// daemon identity key for swap recovery ownership.
func swapRecoveryAuthMessage(kind string, clientIdentity []byte,
	timestamp int64, nonce []byte, fields ...[]byte) []byte {

	msg := make([]byte, 0, 128)
	msg = appendRecoveryField(msg, []byte(kind))
	msg = appendRecoveryField(msg, clientIdentity)

	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(timestamp))
	msg = appendRecoveryField(msg, ts[:])
	msg = appendRecoveryField(msg, nonce)

	for _, field := range fields {
		msg = appendRecoveryField(msg, field)
	}

	return msg
}

// appendRecoveryField adds one length-prefixed field to the signed recovery
// message so adjacent fields cannot be reinterpreted.
func appendRecoveryField(msg []byte, field []byte) []byte {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(field)))

	msg = append(msg, lenBuf[:]...)
	msg = append(msg, field...)

	return msg
}

// recoveryUint64Field encodes numeric RPC fields in the recovery auth message.
func recoveryUint64Field(v uint64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)

	return buf[:]
}

// sealOutSwapRecoveryBlob encrypts the invoice preimage so a seed-restored
// receiver can recover the claim path without server-side plaintext storage.
func sealOutSwapRecoveryBlob(ctx context.Context, daemon DaemonConn,
	clientKey *btcec.PublicKey, paymentHash lntypes.Hash,
	preimage lntypes.Preimage) ([]byte, error) {

	key, err := outSwapRecoveryBlobKey(ctx, daemon, clientKey, paymentHash)
	if err != nil {
		return nil, err
	}

	var nonce [24]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return nil, fmt.Errorf("generate recovery blob nonce: %w", err)
	}

	ciphertext := secretbox.Seal(nil, preimage[:], &nonce, &key)
	blob := make([]byte, 1, 1+len(nonce)+len(ciphertext))
	blob[0] = swapRecoveryBlobVersion
	blob = append(blob, nonce[:]...)
	blob = append(blob, ciphertext...)

	return blob, nil
}

// openOutSwapRecoveryBlob decrypts and verifies an out-swap recovery preimage.
func openOutSwapRecoveryBlob(ctx context.Context, daemon DaemonConn,
	clientKey *btcec.PublicKey, paymentHash lntypes.Hash,
	blob []byte) (*lntypes.Preimage, error) {

	if len(blob) < 1+24 {
		return nil, fmt.Errorf("recovery blob is too short")
	}
	if blob[0] != swapRecoveryBlobVersion {
		return nil, fmt.Errorf("unsupported recovery blob version %d",
			blob[0])
	}

	key, err := outSwapRecoveryBlobKey(ctx, daemon, clientKey, paymentHash)
	if err != nil {
		return nil, err
	}

	var nonce [24]byte
	copy(nonce[:], blob[1:25])

	plaintext, ok := secretbox.Open(nil, blob[25:], &nonce, &key)
	if !ok {
		return nil, fmt.Errorf("open recovery blob")
	}

	preimage, err := lntypes.MakePreimage(plaintext)
	if err != nil {
		return nil, fmt.Errorf("parse recovery preimage: %w", err)
	}
	if preimage.Hash() != paymentHash {
		return nil, fmt.Errorf("recovery preimage hash mismatch")
	}

	return &preimage, nil
}

// outSwapRecoveryBlobKey derives the symmetric key used to seal one out-swap
// preimage without exposing raw seed material outside the daemon.
func outSwapRecoveryBlobKey(ctx context.Context, daemon DaemonConn,
	clientKey *btcec.PublicKey, paymentHash lntypes.Hash) ([32]byte, error) {

	if daemon == nil {
		return [32]byte{}, fmt.Errorf("daemon connection is required")
	}
	if clientKey == nil {
		return [32]byte{}, fmt.Errorf("client identity key is required")
	}

	sharedSecret, err := daemon.ReceiveAuthECDH(ctx, paymentHash, clientKey)
	if err != nil {
		return [32]byte{}, fmt.Errorf("derive recovery blob secret: %w",
			err)
	}

	key := chainhash.TaggedHash(
		[]byte(swapRecoveryBlobTag),
		sharedSecret[:],
		paymentHash[:],
		clientKey.SerializeCompressed(),
	)

	return [32]byte(*key), nil
}
