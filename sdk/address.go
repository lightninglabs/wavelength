package sdk

import (
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/bech32"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/lib/scripts"
)

const (
	receiveAddressVersionV1 = uint8(1)

	receiveAddressPayloadLen = 1 + 32 + 32 + 4
)

type decodedReceiveAddress struct {
	hrp string

	operatorKey  *btcec.PublicKey
	recipientKey *btcec.PublicKey

	exitDelay uint32
}

// encodeReceiveAddress creates a bech32m-encoded receive address from
// the operator key, recipient key, and exit delay.
func encodeReceiveAddress(hrp string, operatorKey,
	recipientKey *btcec.PublicKey, exitDelay uint32) (string, error) {

	if hrp == "" {
		return "", fmt.Errorf("receive address hrp is required")
	}
	if operatorKey == nil {
		return "", fmt.Errorf("operator key is required")
	}
	if recipientKey == nil {
		return "", fmt.Errorf("recipient key is required")
	}
	if exitDelay == 0 {
		return "", fmt.Errorf(
			"receive address exit delay must be positive",
		)
	}

	payload := make([]byte, 0, receiveAddressPayloadLen)
	payload = append(payload, receiveAddressVersionV1)
	payload = append(payload, schnorr.SerializePubKey(operatorKey)...)
	payload = append(payload, schnorr.SerializePubKey(recipientKey)...)

	delayBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(delayBytes, exitDelay)
	payload = append(payload, delayBytes...)

	addrData, err := bech32.ConvertBits(payload, 8, 5, true)
	if err != nil {
		return "", err
	}

	address, err := bech32.EncodeM(hrp, addrData)
	if err != nil {
		return "", err
	}

	return address, nil
}

// decodeReceiveAddress parses a bech32m-encoded receive address into
// its component parts.
func decodeReceiveAddress(
	address string) (*decodedReceiveAddress, error) {

	if address == "" {
		return nil, fmt.Errorf("recipient address is required")
	}

	hrp, addrData, err := bech32.DecodeNoLimit(address)
	if err != nil {
		return nil, fmt.Errorf(
			"decode recipient address: %w", err,
		)
	}

	payload, err := bech32.ConvertBits(addrData, 5, 8, false)
	if err != nil {
		return nil, fmt.Errorf(
			"decode recipient address payload: %w", err,
		)
	}

	if len(payload) != receiveAddressPayloadLen {
		return nil, fmt.Errorf("invalid recipient address payload "+
			"length: expected=%d got=%d",
			receiveAddressPayloadLen, len(payload))
	}

	version := payload[0]
	if version != receiveAddressVersionV1 {
		return nil, fmt.Errorf("unsupported recipient address "+
			"version: %d", version)
	}

	operatorKey, err := schnorr.ParsePubKey(payload[1:33])
	if err != nil {
		return nil, fmt.Errorf("parse recipient address operator "+
			"key: %w", err)
	}

	recipientKey, err := schnorr.ParsePubKey(payload[33:65])
	if err != nil {
		return nil, fmt.Errorf("parse recipient address recipient "+
			"key: %w", err)
	}

	exitDelay := binary.BigEndian.Uint32(payload[65:])
	if exitDelay == 0 {
		return nil, fmt.Errorf(
			"recipient address exit delay must be positive",
		)
	}

	return &decodedReceiveAddress{
		hrp:          hrp,
		operatorKey:  operatorKey,
		recipientKey: recipientKey,
		exitDelay:    exitDelay,
	}, nil
}

// recipientVTXOPkScript derives the taproot pkScript for a VTXO
// output owned by the given recipient key and operator key with
// the specified exit delay.
func recipientVTXOPkScript(ownerKey, operatorKey *btcec.PublicKey,
	exitDelay uint32) ([]byte, error) {

	tapKey, err := scripts.VTXOTapKey(
		ownerKey, operatorKey, exitDelay,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"derive recipient tap key: %w", err,
		)
	}

	pkScript, err := txscript.PayToTaprootScript(tapKey)
	if err != nil {
		return nil, fmt.Errorf(
			"derive recipient pkScript: %w", err,
		)
	}

	return pkScript, nil
}
