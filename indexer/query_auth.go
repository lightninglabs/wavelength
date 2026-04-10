package indexer

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
)

type policyScopeReader interface {
	ListVTXOsByPkScripts(ctx context.Context,
		pkScripts [][]byte) ([]VTXORow, error)
}

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

// authorizeRegisteredOrPolicyScripts authorizes scripts by consulting the
// persisted VTXO policy whenever rows already exist and falling back to the
// registration authorizer only for scripts that are not yet indexed.
func (s *Service) authorizeRegisteredOrPolicyScripts(
	ctx context.Context, q policyScopeReader, principalMailboxID string,
	purpose string, allowedScriptBytes [][]byte,
	scopedSignerKeys map[string]*btcec.PublicKey) error {

	rows, err := q.ListVTXOsByPkScripts(ctx, allowedScriptBytes)
	if err != nil {
		return err
	}

	if err := authorizePolicySignerByRows(
		scopedSignerKeys, rows, s.loadProofConfig().vtxoOperatorKey,
	); err != nil {
		return err
	}

	rowsByScript := make(map[string]struct{}, len(rows))
	for i := 0; i < len(rows); i++ {
		rowsByScript[hex.EncodeToString(rows[i].PkScript)] = struct{}{}
	}

	missingScripts := make([][]byte, 0, len(allowedScriptBytes))
	for i := 0; i < len(allowedScriptBytes); i++ {
		scriptHex := hex.EncodeToString(allowedScriptBytes[i])
		if _, ok := rowsByScript[scriptHex]; ok {
			continue
		}

		missingScripts = append(missingScripts, allowedScriptBytes[i])
	}

	if len(missingScripts) == 0 {
		return nil
	}

	return s.authorizeScripts(
		ctx, principalMailboxID, purpose, missingScripts,
	)
}
