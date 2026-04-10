package indexer

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/stretchr/testify/require"
)

const (
	testProofServerID  = "srv-proof"
	testProofPrincipal = "client:proof"
	testProofExitDelay = 144
)

// buildOwnerKeyVTXOReceiveProof builds a receive-script proof for a
// standardized VTXO tapscript signed by the owner key rather than the taproot
// output key.
func buildOwnerKeyVTXOReceiveProof(t *testing.T,
	purpose string) ([]byte, *arkrpc.TaprootSchnorrProof,
	taprootProofVerificationConfig, time.Time) {

	t.Helper()

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	tapKey, err := arkscript.VTXOTapKey(
		ownerPriv.PubKey(), operatorPriv.PubKey(), testProofExitDelay,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(tapKey)
	require.NoError(t, err)

	issuedAt := time.Unix(1_700_000_000, 0)
	msgBytes, err := BuildReceiveScriptProofMessageWithOwner(
		testProofServerID, testProofPrincipal, purpose, pkScript,
		ownerPriv.PubKey().SerializeCompressed(),
		[]byte{0x01, 0x02, 0x03, 0x04}, issuedAt,
		issuedAt.Add(10*time.Minute),
	)
	require.NoError(t, err)

	msgHash := chainhash.TaggedHash(ProofTagHash, msgBytes)
	sig, err := schnorr.Sign(ownerPriv, msgHash[:])
	require.NoError(t, err)

	return pkScript, &arkrpc.TaprootSchnorrProof{
			Message: msgBytes,
			Sig64:   sig.Serialize(),
		}, taprootProofVerificationConfig{
			vtxoOperatorKey: operatorPriv.PubKey(),
			vtxoExitDelay:   testProofExitDelay,
		}, issuedAt.Add(time.Minute)
}

// buildParticipantScopeProof builds a script-scope proof signed by one explicit
// participant key.
func buildOwnerKeyVTXOScopeProof(t *testing.T,
	purpose string) ([]byte, any, *btcec.PublicKey, time.Time) {

	t.Helper()

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	queryScriptPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(queryScriptPriv.PubKey())
	require.NoError(t, err)

	issuedAt := time.Unix(1_700_000_500, 0)
	msgBytes, err := BuildScriptScopeProofMessageWithSigner(
		testProofServerID, testProofPrincipal, purpose,
		ownerPriv.PubKey().SerializeCompressed(),
		[]byte{0x05, 0x06, 0x07, 0x08}, issuedAt,
		issuedAt.Add(10*time.Minute),
	)
	require.NoError(t, err)

	msgHash := chainhash.TaggedHash(ProofTagHash, msgBytes)
	sig, err := schnorr.Sign(ownerPriv, msgHash[:])
	require.NoError(t, err)

	return pkScript, &arkrpc.ScriptScope_TaprootSchnorr{
		TaprootSchnorr: &arkrpc.TaprootSchnorrProof{
			Message: msgBytes,
			Sig64:   sig.Serialize(),
		},
	}, ownerPriv.PubKey(), issuedAt.Add(time.Minute)
}

// TestVerifyTaprootSchnorrProofOwnerKeyVTXOScript verifies that receive-script
// proofs for standardized VTXO tapscripts can be signed by the owner key when
// the message commits to that owner key.
func TestVerifyTaprootSchnorrProofOwnerKeyVTXOScript(t *testing.T) {
	t.Parallel()

	pkScript, proof, cfg, now := buildOwnerKeyVTXOReceiveProof(
		t, purposeRegisterReceiveScript,
	)

	err := verifyTaprootSchnorrProof(
		now, pkScript, proof, testProofServerID, testProofPrincipal,
		purposeRegisterReceiveScript, cfg,
	)
	require.NoError(t, err)

	err = verifyTaprootSchnorrProof(
		now, pkScript, proof, testProofServerID, testProofPrincipal,
		purposeRegisterReceiveScript, taprootProofVerificationConfig{},
	)
	require.ErrorContains(t, err, "owner pubkey")
}

// TestVerifyScriptScopeProofOwnerKeyVTXOScript verifies that script-scope
// proofs for standardized VTXO tapscripts can be signed by the owner key when
// the message commits to that owner key.
func TestVerifyScriptScopeProofOwnerKeyVTXOScript(t *testing.T) {
	t.Parallel()

	_, proof, signerKey, now := buildOwnerKeyVTXOScopeProof(
		t, purposeOORRecipientEvents,
	)

	gotSignerKey, err := verifyScriptScopeProof(
		now, proof, testProofServerID, testProofPrincipal,
		purposeOORRecipientEvents,
	)
	require.NoError(t, err)
	require.True(t, sameXOnlyKey(signerKey, gotSignerKey))
}
