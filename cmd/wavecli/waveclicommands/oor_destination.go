package waveclicommands

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/wavelength/waverpc"
)

const (
	// oorPubKeyHexLen is the hex length of a 32-byte x-only pubkey.
	oorPubKeyHexLen = schnorr.PubKeyBytesLen * 2
)

// buildOORRecipientOutput converts the user-facing OOR destination flags or
// MCP args into the waverpc Output used by SendOOR.
func buildOORRecipientOutput(address, pubKeyHex string,
	amount int64) (*waverpc.Output, error) {

	if amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}

	address = strings.TrimSpace(address)
	pubKeyHex = strings.TrimSpace(pubKeyHex)

	destinations := 0
	if address != "" {
		destinations++
	}
	if pubKeyHex != "" {
		destinations++
	}

	if destinations != 1 {
		return nil, fmt.Errorf("exactly one of to or pubkey is " +
			"required")
	}

	output := &waverpc.Output{
		AmountSat: amount,
	}

	switch {
	case address != "":
		output.Destination = &waverpc.Output_Address{
			Address: address,
		}

	case pubKeyHex != "":
		pubKey, err := parseOORPubKeyHex(pubKeyHex)
		if err != nil {
			return nil, err
		}

		output.Destination = &waverpc.Output_Pubkey{
			Pubkey: pubKey,
		}
	}

	return output, nil
}

// parseOORPubKeyHex validates and decodes a 32-byte x-only schnorr pubkey.
func parseOORPubKeyHex(pubKeyHex string) ([]byte, error) {
	if len(pubKeyHex) != oorPubKeyHexLen {
		return nil, fmt.Errorf("pubkey must be %d hex chars (32-byte "+
			"x-only key)", oorPubKeyHexLen)
	}

	pubKeyBytes, err := hex.DecodeString(pubKeyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid pubkey hex: %w", err)
	}

	pubKey, err := schnorr.ParsePubKey(pubKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid x-only pubkey: %w", err)
	}

	return schnorr.SerializePubKey(pubKey), nil
}
