package darepoclicommands

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/darepo-client/daemonrpc"
)

const (
	// oorPubKeyHexLen is the hex length of a 32-byte x-only pubkey.
	oorPubKeyHexLen = schnorr.PubKeyBytesLen * 2
)

// buildOORRecipientOutput converts the user-facing OOR destination flags or
// MCP args into the daemonrpc Output used by SendOOR.
func buildOORRecipientOutput(address, pubKeyHex, pkScriptHex string,
	amount int64) (*daemonrpc.Output, error) {

	if amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}

	address = strings.TrimSpace(address)
	pubKeyHex = strings.TrimSpace(pubKeyHex)
	pkScriptHex = strings.TrimSpace(pkScriptHex)

	destinations := 0
	if address != "" {
		destinations++
	}
	if pubKeyHex != "" {
		destinations++
	}
	if pkScriptHex != "" {
		destinations++
	}

	if destinations != 1 {
		return nil, fmt.Errorf("exactly one of to, pubkey, or " +
			"pk_script is required")
	}

	output := &daemonrpc.Output{
		AmountSat: amount,
	}

	switch {
	case address != "":
		output.Destination = &daemonrpc.Output_Address{
			Address: address,
		}

	case pubKeyHex != "":
		pubKey, err := parseOORPubKeyHex(pubKeyHex)
		if err != nil {
			return nil, err
		}

		output.Destination = &daemonrpc.Output_Pubkey{
			Pubkey: pubKey,
		}

	case pkScriptHex != "":
		pkScript, err := parseOORPkScriptHex(pkScriptHex)
		if err != nil {
			return nil, err
		}

		output.Destination = &daemonrpc.Output_PkScript{
			PkScript: pkScript,
		}
	}

	return output, nil
}

// parseOORPubKeyHex validates and decodes a 32-byte x-only schnorr pubkey.
func parseOORPubKeyHex(pubKeyHex string) ([]byte, error) {
	if len(pubKeyHex) != oorPubKeyHexLen {
		return nil, fmt.Errorf("pubkey must be %d hex chars "+
			"(32-byte x-only key)", oorPubKeyHexLen)
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

// parseOORPkScriptHex validates and decodes a raw output script hex string.
func parseOORPkScriptHex(pkScriptHex string) ([]byte, error) {
	pkScript, err := hex.DecodeString(pkScriptHex)
	if err != nil {
		return nil, fmt.Errorf("invalid pk_script hex: %w", err)
	}

	if len(pkScript) == 0 {
		return nil, fmt.Errorf("pk_script must not be empty")
	}

	return pkScript, nil
}
