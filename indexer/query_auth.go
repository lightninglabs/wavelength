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

// authorizedScriptScopeQuery is the fully-authorized result of a
// script-scope authorization pass. It bundles everything a caller
// needs to perform a downstream query: the validated pkScript set
// and the per-script signer key the proof was bound to.
type authorizedScriptScopeQuery struct {
	// AllowedScriptBytes is the ordered list of pkScripts the caller
	// is authorized to query. These are defensively copied from the
	// inbound scopes.
	AllowedScriptBytes [][]byte

	// ScopedSignerKeys maps hex(pkScript) to the participant signer
	// key the scope proof was signed under. Callers may surface this
	// for audit but should not use it to make further authorization
	// decisions — the full authorization is already encoded in
	// AllowedScriptBytes.
	ScopedSignerKeys map[string]*btcec.PublicKey
}

// authorizeScriptScopeQuery is the single canonical entry point for
// authorizing a script-scope RPC. It runs proof verification and
// row-based authorization together so a caller cannot forget the
// second step (which would silently admit any valid signer-key proof
// regardless of pkScript). All four script-scope RPCs should use this
// function; the lower-level verifyQueryScriptScopes and
// authorizeRegisteredOrPolicyScripts helpers are kept for explicit
// internal composition but must not be called in isolation from a
// handler path.
func (s *Service) authorizeScriptScopeQuery(ctx context.Context,
	q policyScopeReader, now time.Time, principalMailboxID string,
	scopes []*arkrpc.ScriptScope, purpose string) (
	*authorizedScriptScopeQuery, error) {

	allowedScriptBytes, scopedSignerKeys, err := s.verifyQueryScriptScopes(
		now, principalMailboxID, scopes, purpose,
	)
	if err != nil {
		return nil, err
	}

	if err := s.authorizeRegisteredOrPolicyScripts(
		ctx, q, principalMailboxID, purpose, allowedScriptBytes,
		scopedSignerKeys,
	); err != nil {
		return nil, err
	}

	return &authorizedScriptScopeQuery{
		AllowedScriptBytes: allowedScriptBytes,
		ScopedSignerKeys:   scopedSignerKeys,
	}, nil
}

// verifyQueryScriptScopes validates query proofs for all requested scopes and
// returns both the copied scripts and the participant signer key committed to
// each scope.
//
// Handlers should call authorizeScriptScopeQuery rather than invoking
// this directly — it only performs step 1 of the two-step auth flow.
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
func (s *Service) authorizeRegisteredOrPolicyScripts(ctx context.Context,
	q policyScopeReader, principalMailboxID string, purpose string,
	allowedScriptBytes [][]byte,
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
