package psbtutil

import (
	"bytes"
	"encoding/base64"
	"fmt"

	"github.com/btcsuite/btcd/psbt/v2"
)

// Package psbtutil contains small helpers for encoding and decoding PSBTs.
//
// These helpers are intentionally "dumb": they only serialize/parse bytes and
// do not attempt to validate the transaction's semantics (ordering rules,
// script correctness, etc). Callers should run the appropriate validators for
// the protocol they are implementing.

// Serialize encodes a PSBT packet into raw bytes.
func Serialize(pkt *psbt.Packet) ([]byte, error) {
	if pkt == nil {
		return nil, fmt.Errorf("psbt must be provided")
	}

	var b bytes.Buffer
	err := pkt.Serialize(&b)
	if err != nil {
		return nil, fmt.Errorf("serialize psbt: %w", err)
	}

	return b.Bytes(), nil
}

// Parse decodes a PSBT packet from raw bytes.
func Parse(raw []byte) (*psbt.Packet, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("psbt bytes must be provided")
	}

	pkt, err := psbt.NewFromRawBytes(bytes.NewReader(raw), false)
	if err != nil {
		return nil, fmt.Errorf("parse psbt: %w", err)
	}

	return pkt, nil
}

// EncodeBase64 serializes and base64-encodes a PSBT packet.
func EncodeBase64(pkt *psbt.Packet) (string, error) {
	raw, err := Serialize(pkt)
	if err != nil {
		return "", err
	}

	// We use standard base64 (not URL base64) to match common PSBT
	// tooling and to make it easy to copy/paste values into other
	// PSBT-aware utilities.
	return base64.StdEncoding.EncodeToString(raw), nil
}

// DecodeBase64 base64-decodes and parses a PSBT packet.
func DecodeBase64(b64 string) (*psbt.Packet, error) {
	if b64 == "" {
		return nil, fmt.Errorf("base64 string must be provided")
	}

	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}

	return Parse(raw)
}
