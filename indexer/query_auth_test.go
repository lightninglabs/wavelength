package indexer

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/stretchr/testify/require"
)

type recordingScriptAuthorizer struct {
	requests []ScriptAuthorizationRequest
	err      error
}

func (a *recordingScriptAuthorizer) AuthorizeScripts(_ context.Context,
	req ScriptAuthorizationRequest) error {

	copyReq := ScriptAuthorizationRequest{
		PrincipalMailboxID: req.PrincipalMailboxID,
		Purpose:            req.Purpose,
		Now:                req.Now,
		PkScripts:          make([][]byte, len(req.PkScripts)),
	}
	for i := range req.PkScripts {
		copyReq.PkScripts[i] = append([]byte(nil), req.PkScripts[i]...)
	}

	a.requests = append(a.requests, copyReq)

	return a.err
}

type stubPolicyScopeReader struct {
	rows []VTXORow
	err  error
}

func (r *stubPolicyScopeReader) ListVTXOsByPkScripts(_ context.Context,
	pkScripts [][]byte) ([]VTXORow, error) {

	if r.err != nil {
		return nil, r.err
	}

	rows := make([]VTXORow, 0, len(r.rows))
	for i := range pkScripts {
		scriptHex := hex.EncodeToString(pkScripts[i])
		for j := range r.rows {
			if hex.EncodeToString(r.rows[j].PkScript) != scriptHex {
				continue
			}

			rows = append(rows, r.rows[j])
		}
	}

	return rows, nil
}

func testStandardQueryRow(t *testing.T) (VTXORow, *btcec.PrivateKey,
	*btcec.PrivateKey) {

	t.Helper()

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	template, err := arkscript.StandardVTXOTemplate(
		ownerPriv.PubKey(), operatorPriv.PubKey(), 144,
	)
	require.NoError(t, err)

	policyTemplate, err := template.Encode()
	require.NoError(t, err)

	pkScript, err := template.PkScript()
	require.NoError(t, err)

	return VTXORow{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				1,
			},
			Index: 0,
		},
		PkScript:       pkScript,
		PolicyTemplate: policyTemplate,
		Status:         storeVTXOStatusLive,
	}, ownerPriv, operatorPriv
}

func TestAuthorizeRegisteredOrPolicyScriptsUsesRegistrationFallback(
	t *testing.T) {

	t.Parallel()

	row, ownerPriv, operatorPriv := testStandardQueryRow(t)

	missingPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	missingScript, err := txscript.PayToTaprootScript(missingPriv.PubKey())
	require.NoError(t, err)

	authz := &recordingScriptAuthorizer{}
	svc := NewService("srv-test", nil)
	svc.SetScriptAuthorizer(authz)
	svc.SetVTXOProofPolicy(operatorPriv.PubKey(), 144)

	err = svc.authorizeRegisteredOrPolicyScripts(
		t.Context(),
		&stubPolicyScopeReader{rows: []VTXORow{row}},
		"principal-1",
		purposeGetSubtreeByScripts,
		[][]byte{row.PkScript, missingScript},
		map[string]*btcec.PublicKey{
			hex.EncodeToString(row.PkScript):  ownerPriv.PubKey(),
			hex.EncodeToString(missingScript): missingPriv.PubKey(),
		},
	)
	require.NoError(t, err)

	require.Len(t, authz.requests, 1)
	require.Equal(t, "principal-1", authz.requests[0].PrincipalMailboxID)
	require.Equal(t, purposeGetSubtreeByScripts, authz.requests[0].Purpose)
	require.Len(t, authz.requests[0].PkScripts, 1)
	require.Equal(t, missingScript, authz.requests[0].PkScripts[0])
}

func TestAuthorizeRegisteredOrPolicyScriptsRejectsUnauthorizedSigner(
	t *testing.T) {

	t.Parallel()

	row, _, operatorPriv := testStandardQueryRow(t)

	authz := &recordingScriptAuthorizer{}
	svc := NewService("srv-test", nil)
	svc.SetScriptAuthorizer(authz)
	svc.SetVTXOProofPolicy(operatorPriv.PubKey(), 144)

	err := svc.authorizeRegisteredOrPolicyScripts(
		t.Context(),
		&stubPolicyScopeReader{rows: []VTXORow{row}},
		"principal-1",
		purposeListVTXOEventsByScripts,
		[][]byte{row.PkScript},
		map[string]*btcec.PublicKey{
			hex.EncodeToString(row.PkScript): operatorPriv.PubKey(),
		},
	)
	require.ErrorContains(t, err, "not authorized")
	require.Empty(t, authz.requests)
}
