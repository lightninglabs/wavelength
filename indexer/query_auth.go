package indexer

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
)

// verifyQueryScriptScopes validates query proofs for all requested scopes and
// returns both the copied scripts and the participant signer key committed to
// each scope.
func (s *Service) verifyQueryScriptScopes(now time.Time,
	principalMailboxID string, scopes []*arkrpc.ScriptScope,
	purpose string) ([][]byte, map[string]*btcec.PublicKey, error) {

	allowedScriptBytes := make([][]byte, 0, len(scopes))
	scopedSignerKeys := make(map[string]*btcec.PublicKey, len(scopes))

	for i := 0; i < len(scopes); i++ {
		scope := scopes[i]
		if scope == nil || len(scope.PkScript) == 0 {
			return nil, nil, fmt.Errorf("missing pk_script")
		}

		signerKey, err := verifyScriptScopeProof(
			now, scope.Proof, s.serverID, principalMailboxID,
			purpose,
		)
		if err != nil {
			return nil, nil, err
		}

		pkScriptCopy := append([]byte(nil), scope.PkScript...)
		allowedScriptBytes = append(allowedScriptBytes, pkScriptCopy)
		scopedSignerKeys[hex.EncodeToString(scope.PkScript)] = signerKey
	}

	return allowedScriptBytes, scopedSignerKeys, nil
}

// authorizePolicyScopes uses the persisted VTXO policy bytes for the
// requested scripts to ensure each scope signer is one of the queryable
// participants for all matching rows.
func (s *Service) authorizePolicyScopes(
	ctx context.Context, q Store, allowedScriptBytes [][]byte,
	scopedSignerKeys map[string]*btcec.PublicKey) error {

	rows, err := q.ListVTXOsByPkScripts(ctx, allowedScriptBytes)
	if err != nil {
		return err
	}

	return authorizePolicySignerByRows(
		scopedSignerKeys, rows, s.loadProofConfig().vtxoOperatorKey,
	)
}
