package indexer

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
)

// sameXOnlyKey returns true when both public keys encode to the same x-only
// Taproot key, regardless of original parity.
func sameXOnlyKey(a, b *btcec.PublicKey) bool {
	if a == nil || b == nil {
		return false
	}

	return bytes.Equal(
		schnorr.SerializePubKey(a),
		schnorr.SerializePubKey(b),
	)
}

// participantKeysFromRow returns the non-operator participant keys authorized
// to query the given VTXO row.
func participantKeysFromRow(row VTXORow,
	operatorKey *btcec.PublicKey) ([]*btcec.PublicKey, error) {

	if len(row.PolicyTemplate) == 0 {
		return nil, fmt.Errorf("vtxo %s missing policy template",
			row.Outpoint)
	}

	template, err := arkscript.DecodePolicyTemplate(row.PolicyTemplate)
	if err != nil {
		return nil, fmt.Errorf("decode policy template for %s: %w",
			row.Outpoint, err)
	}

	keys := template.ParticipantKeys()
	allowed := make([]*btcec.PublicKey, 0, len(keys))
	for i := 0; i < len(keys); i++ {
		key := keys[i]
		if sameXOnlyKey(key, operatorKey) {
			continue
		}

		allowed = append(allowed, key)
	}

	if len(allowed) == 0 {
		return nil, fmt.Errorf("vtxo %s has no queryable participants",
			row.Outpoint)
	}

	return allowed, nil
}

// authorizePolicySignerByRows ensures each requested pkScript is only queried
// by a signer that appears in the persisted policy of every matching VTXO row.
//
// Scripts with no matching rows are allowed to pass through here so callers
// can still return empty results when registration auth already granted access.
func authorizePolicySignerByRows(scopedSignerKeys map[string]*btcec.PublicKey,
	rows []VTXORow, operatorKey *btcec.PublicKey) error {

	rowsByScript := make(map[string][]VTXORow)
	for i := 0; i < len(rows); i++ {
		row := rows[i]
		scriptHex := hex.EncodeToString(row.PkScript)
		rowsByScript[scriptHex] = append(rowsByScript[scriptHex], row)
	}

	for scriptHex, signerKey := range scopedSignerKeys {
		if signerKey == nil {
			return fmt.Errorf("missing signer key for script %s",
				scriptHex)
		}

		scriptRows := rowsByScript[scriptHex]
		if len(scriptRows) == 0 {
			continue
		}

		for i := 0; i < len(scriptRows); i++ {
			allowedKeys, err := participantKeysFromRow(
				scriptRows[i], operatorKey,
			)
			if err != nil {
				return err
			}

			authorized := false
			for j := 0; j < len(allowedKeys); j++ {
				if sameXOnlyKey(allowedKeys[j], signerKey) {
					authorized = true
					break
				}
			}
			if authorized {
				continue
			}

			return fmt.Errorf(
				"signer key not authorized for script %s",
				scriptHex,
			)
		}
	}

	return nil
}
