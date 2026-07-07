package bip322

import (
	"bytes"
	"encoding/base64"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
)

// Sig represents a full-format BIP-322 signature.
//
// In full format, the signature payload is the full serialized to_sign
// transaction.
type Sig struct {
	// ToSign is the signed virtual transaction defined by BIP-322.
	ToSign *wire.MsgTx
}

// Encode serializes the full-format signature payload.
func (s *Sig) Encode() ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("signature must be provided")
	}

	if s.ToSign == nil {
		return nil, fmt.Errorf("full signature transaction must be " +
			"provided")
	}

	var b bytes.Buffer
	err := s.ToSign.Serialize(&b)
	if err != nil {
		return nil, fmt.Errorf("encode full signature tx: %w", err)
	}

	return b.Bytes(), nil
}

// EncodeBase64 serializes the signature and base64-encodes it.
func (s *Sig) EncodeBase64() (string, error) {
	raw, err := s.Encode()
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(raw), nil
}

// DecodeSig decodes a raw full-format signature payload.
func DecodeSig(raw []byte) (*Sig, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("signature bytes must be provided")
	}

	toSign, err := decodeFullSigTx(raw)
	if err != nil {
		return nil, err
	}

	return &Sig{
		ToSign: toSign,
	}, nil
}

// DecodeSigBase64 base64-decodes and parses a full-format signature payload.
func DecodeSigBase64(b64 string) (*Sig, error) {
	if b64 == "" {
		return nil, fmt.Errorf("base64 signature must be provided")
	}

	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode base64 signature: %w", err)
	}

	return DecodeSig(raw)
}

// decodeFullSigTx parses a raw full-format signature payload into a
// transaction and rejects trailing bytes to keep parsing strict.
func decodeFullSigTx(raw []byte) (*wire.MsgTx, error) {
	reader := bytes.NewReader(raw)

	tx := wire.NewMsgTx(0)
	err := tx.Deserialize(reader)
	if err != nil {
		return nil, fmt.Errorf("decode full signature tx: %w", err)
	}

	if reader.Len() != 0 {
		return nil, fmt.Errorf("decode full signature tx: %d "+
			"trailing bytes", reader.Len())
	}

	return tx, nil
}

// cloneBytes copies a byte slice so callers can safely mutate their input
// buffers after calling into this package.
func cloneBytes(src []byte) []byte {
	if src == nil {
		return nil
	}

	dst := make([]byte, len(src))
	copy(dst, src)

	return dst
}

// cloneWitness performs a deep copy of witness stack elements.
func cloneWitness(src wire.TxWitness) wire.TxWitness {
	if src == nil {
		return nil
	}

	dst := make(wire.TxWitness, len(src))
	for i := 0; i < len(src); i++ {
		dst[i] = cloneBytes(src[i])
	}

	return dst
}
