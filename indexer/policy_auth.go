package indexer

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"

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
		schnorr.SerializePubKey(a), schnorr.SerializePubKey(b),
	)
}

// participantKeysFromRow returns the non-operator participant keys authorized
// to query the given VTXO row.
//
// Authorization here follows the spec's "valid settlement pair" rule
// (docs/custom_scripting_state.md L79-82, L286-292): a key is a
// queryable participant iff it appears in at least one settlement
// pair with the operator — i.e. there is both a participant-only
// auth leaf and an operator-backed forfeit sibling referencing the
// key. Using arkscript.PolicyTemplate.ParticipantKeys() (every key
// in every leaf) is strictly broader than the spec rule and was
// the over-authorization half of the PR-187 H-1 finding; a key
// that only appears in a non-operator-backed leaf (e.g. a pure
// CSV exit branch with an unrelated watcher) should NOT be able
// to read the VTXO's metadata.
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

	if operatorKey == nil {
		return nil, fmt.Errorf("operator key is required to derive " +
			"queryable participants")
	}

	// Keep keys that (a) are non-operator and (b) have at least one
	// valid settlement pair with the operator. Dedup by x-only key
	// to keep the returned list stable when a key appears in
	// multiple leaves.
	keys := template.ParticipantKeys()
	seen := make(map[string]struct{}, len(keys))
	allowed := make([]*btcec.PublicKey, 0, len(keys))
	for i := 0; i < len(keys); i++ {
		key := keys[i]
		if key == nil {
			continue
		}
		if sameXOnlyKey(key, operatorKey) {
			continue
		}

		xOnly := string(schnorr.SerializePubKey(key))
		if _, dup := seen[xOnly]; dup {
			continue
		}

		pairs, err := template.SettlementPairsForParticipant(
			key, operatorKey,
		)
		switch {
		case err == nil && len(pairs) > 0:
			// Happy path: at least one settlement pair, so
			// the key is a real queryable participant.

		case err != nil && strings.Contains(
			err.Error(),
			"no settlement pairs",
		):

			// This is the "legitimately not a queryable
			// participant" signal from arkscript: the key
			// appears in the template but does not have a
			// valid (unilateral-auth, operator-backed-
			// forfeit) pair. Filter it out rather than
			// propagating; propagation would mean a single
			// stalker key in a poisoned template could DoS
			// all queries against that row.
			continue

		default:
			return nil, fmt.Errorf("settlement pairs for "+
				"participant in vtxo %s: %w", row.Outpoint, err)
		}

		seen[xOnly] = struct{}{}
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

			return fmt.Errorf("signer key not authorized for "+
				"script %s", scriptHex)
		}
	}

	return nil
}
