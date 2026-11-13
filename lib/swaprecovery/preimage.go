// Package swaprecovery defines public parameters for seed-restorable receive
// preimages. It never serializes private keys or ECDH secrets.
package swaprecovery

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"golang.org/x/crypto/hkdf"
)

const (
	// OwnerProofTag is the sole identity-signing domain exposed to recovery
	// callers. In particular, transaction signing tags must never be
	// accepted.
	OwnerProofTag = "wavelength-swap-recovery-owner-v1"

	// KeyFamily isolates preimage derivation from signing and identity
	// keys.
	KeyFamily keychain.KeyFamily = 46

	// ParamsSize is version(1) + key index(4) + random nonce(32).
	ParamsSize = 37

	// Version fixes the key family, encoding, and derivation algorithm.
	Version byte = 1

	preimageDomain = "wavelength-swap-preimage-v1"
)

// NewParams creates public metadata for a new invoice. A fresh nonce prevents
// preimage reuse if a key index repeats after seed restore or on another
// device. Index zero is valid and does not require a persistent allocation
// counter.
func NewParams(index uint32) ([]byte, error) {
	params := make([]byte, ParamsSize)
	params[0] = Version
	binary.BigEndian.PutUint32(params[1:5], index)
	if _, err := ParseParams(params); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(rand.Reader, params[5:]); err != nil {
		return nil, fmt.Errorf("generate preimage nonce: %w", err)
	}

	return params, nil
}

// ParseParams rejects unknown formats and indices outside the non-hardened
// range supported by lnd's signed int32 RPC key locator.
func ParseParams(params []byte) (keychain.KeyLocator, error) {
	if len(params) != ParamsSize {
		return keychain.KeyLocator{}, fmt.Errorf("preimage recovery "+
			"params must be %d bytes", ParamsSize)
	}
	if params[0] != Version {
		return keychain.KeyLocator{}, fmt.Errorf("unsupported "+
			"preimage recovery version %d", params[0])
	}
	index := binary.BigEndian.Uint32(params[1:5])
	if index >= 1<<31 {
		return keychain.KeyLocator{}, fmt.Errorf("preimage recovery " +
			"index exceeds int32")
	}

	return keychain.KeyLocator{Family: KeyFamily, Index: index}, nil
}

// Derive turns SHA256(compressed(k * PubKey(k))) from wallet self-ECDH into
// a preimage. The public parameters and Bitcoin genesis hash bind the result
// to one invoice and network. Revealing the output does not reveal the ECDH
// base used for other nonces. Callers must keep sharedSecret private.
func Derive(params []byte, genesis chainhash.Hash,
	sharedSecret [32]byte) (lntypes.Preimage, error) {

	if _, err := ParseParams(params); err != nil {
		return lntypes.Preimage{}, err
	}
	info := append([]byte(preimageDomain), genesis[:]...)
	info = append(info, params...)
	kdf := hkdf.New(sha256.New, sharedSecret[:], nil, info)
	var preimage lntypes.Preimage
	if _, err := io.ReadFull(kdf, preimage[:]); err != nil {
		return lntypes.Preimage{}, fmt.Errorf("derive preimage: %w",
			err)
	}

	return preimage, nil
}
