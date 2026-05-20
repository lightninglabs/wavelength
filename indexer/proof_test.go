package indexer

import (
	"bytes"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightningnetwork/lnd/tlv"
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
func buildOwnerKeyVTXOReceiveProof(t *testing.T, purpose string) ([]byte,
	*arkrpc.TaprootSchnorrProof, taprootProofVerificationConfig,
	time.Time) {

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
func buildOwnerKeyVTXOScopeProof(t *testing.T, purpose string) ([]byte, any,
	*btcec.PublicKey, time.Time) {

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

// validProofMessageForTest returns a fully-populated proofMessage
// that passes every rule in validateProofMessageCommon so the
// per-variant tests below can isolate the rule they intend to hit.
func validProofMessageForTest(expectedType string,
	now time.Time) *proofMessage {

	return &proofMessage{
		Type:      expectedType,
		Version:   0,
		ServerID:  testProofServerID,
		Principal: testProofPrincipal,
		Purpose:   purposeOORRecipientEvents,
		IssuedAt:  uint64(now.Add(-time.Minute).Unix()),
		ExpiresAt: uint64(now.Add(time.Minute).Unix()),
		Nonce: []byte{
			0xDE,
			0xAD,
			0xBE,
			0xEF,
		},
	}
}

// TestValidateProofMessageForScriptRequiresPkScript asserts that
// passing an empty pkScript to the script-bound validator is a
// caller-side programming error, not a silent permit of every
// proof. This is the core H-4 fix: previously
// validateProofMessage(pkScript=nil) silently disabled the binding
// check.
func TestValidateProofMessageForScriptRequiresPkScript(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	msg := validProofMessageForTest(proofTypeReceiveScriptRegistration, now)
	msg.PkScript = []byte{0x51, 0x20, 0x01}

	err := validateProofMessageForScript(
		now, msg, proofTypeReceiveScriptRegistration, testProofServerID,
		testProofPrincipal, purposeOORRecipientEvents, nil,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires non-empty pkScript")

	err = validateProofMessageForScript(
		now, msg, proofTypeReceiveScriptRegistration, testProofServerID,
		testProofPrincipal, purposeOORRecipientEvents, []byte{},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires non-empty pkScript")
}

// TestValidateProofMessageForScriptMismatch asserts that a proof
// whose msg.PkScript differs from the caller's expected pkScript is
// rejected.
func TestValidateProofMessageForScriptMismatch(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	msg := validProofMessageForTest(proofTypeReceiveScriptRegistration, now)
	msg.PkScript = []byte{0x51, 0x20, 0x01}

	err := validateProofMessageForScript(
		now, msg, proofTypeReceiveScriptRegistration, testProofServerID,
		testProofPrincipal, purposeOORRecipientEvents,
		[]byte{0x51, 0x20, 0x02},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "pk_script mismatch")
}

// TestValidateProofMessageForScriptMatch asserts the happy path when
// msg.PkScript equals the expected pkScript.
func TestValidateProofMessageForScriptMatch(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	pkScript := []byte{0x51, 0x20, 0x01}
	msg := validProofMessageForTest(proofTypeReceiveScriptRegistration, now)
	msg.PkScript = pkScript

	err := validateProofMessageForScript(
		now, msg, proofTypeReceiveScriptRegistration, testProofServerID,
		testProofPrincipal, purposeOORRecipientEvents, pkScript,
	)
	require.NoError(t, err)
}

// TestValidateProofMessageScopedRejectsPkScript asserts that a
// scoped-variant proof that smuggles a pkScript on the wire is
// rejected. Without this rule, the legacy script-bound validator
// could be confused with the new policy-backed scope variant.
func TestValidateProofMessageScopedRejectsPkScript(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	msg := validProofMessageForTest(proofTypeScriptScope, now)
	msg.PkScript = []byte{0x51, 0x20, 0x01}

	err := validateProofMessageScoped(
		now, msg, proofTypeScriptScope, testProofServerID,
		testProofPrincipal, purposeOORRecipientEvents,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must not commit a pk_script")
}

// TestValidateProofMessageScopedAcceptsEmptyPkScript asserts the
// happy path for the scoped variant.
func TestValidateProofMessageScopedAcceptsEmptyPkScript(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	msg := validProofMessageForTest(proofTypeScriptScope, now)

	err := validateProofMessageScoped(
		now, msg, proofTypeScriptScope, testProofServerID,
		testProofPrincipal, purposeOORRecipientEvents,
	)
	require.NoError(t, err)
}

// encodeTLVBigSize encodes a uint64 using the same BigSize varint format
// the lnd/tlv package uses for record types and lengths. It exists so the
// DoS-vector tests below can hand-craft pathological TLV streams that the
// regular encoder would never emit.
func encodeTLVBigSize(t *testing.T, val uint64) []byte {
	t.Helper()

	var buf bytes.Buffer
	var scratch [8]byte
	require.NoError(t, tlv.WriteVarInt(&buf, val, &scratch))

	return buf.Bytes()
}

// TestParseReceiveScriptProofMessageRejectsOversizedEnvelope asserts that
// a proof message whose total wire size exceeds maxProofMessageSize is
// rejected before any TLV record is parsed. This is the defense-in-depth
// outer bound that complements the per-record cap from DecodeP2P.
func TestParseReceiveScriptProofMessageRejectsOversizedEnvelope(t *testing.T) {
	t.Parallel()

	// Exactly-at-limit must not be rejected by the size check (it may
	// still fail downstream parsing, but not with "too large").
	atLimit := make([]byte, maxProofMessageSize)
	_, err := parseReceiveScriptProofMessage(atLimit)
	if err != nil {
		require.NotContains(t, err.Error(), "proof message too large")
	}

	// One byte over the limit must be rejected immediately with the
	// size error, without invoking the TLV decoder.
	overLimit := make([]byte, maxProofMessageSize+1)
	_, err = parseReceiveScriptProofMessage(overLimit)
	require.Error(t, err)
	require.Contains(t, err.Error(), "proof message too large")
}

// TestParseReceiveScriptProofMessageRejectsOversizedRecord asserts that
// a tiny TLV envelope which declares a multi-gigabyte record length is
// rejected promptly (issue #368). Without DecodeP2P the underlying
// tlv.DVarBytes decoder would attempt to allocate a buffer the size of
// the declared length, which is the exact DoS vector this fix closes.
func TestParseReceiveScriptProofMessageRejectsOversizedRecord(t *testing.T) {
	t.Parallel()

	// Build a TLV stream containing a single record of type
	// proofTLVTypePkScript with a declared length of 1 PiB. The actual
	// value bytes are omitted entirely: a hardened decoder must refuse
	// the record based on its declared length and never attempt the
	// allocation.
	//
	// 1 PiB is intentionally large enough to make Go's runtime panic
	// with "makeslice: len out of range" if the decoder ever calls
	// make([]byte, length) directly (as the unfixed tlv.DVarBytes path
	// does). DecodeP2P short-circuits the length check, returning
	// ErrRecordTooLarge before any allocation is attempted.
	var payload bytes.Buffer
	payload.Write(encodeTLVBigSize(t, uint64(proofTLVTypePkScript)))
	payload.Write(encodeTLVBigSize(t, 1<<50))

	// Sanity: confirm the malicious envelope is tiny so it would slip
	// past any naive byte-length check that only inspects the outer
	// payload.
	require.Less(t, payload.Len(), 16)

	_, err := parseReceiveScriptProofMessage(payload.Bytes())
	require.Error(t, err)

	// The underlying error from DecodeP2P is ErrRecordTooLarge; we
	// match on the wrapping context so the assertion is robust to
	// upstream error-text tweaks.
	require.Contains(t, err.Error(), "decode TLV proof")
}

// TestParseReceiveScriptProofMessageAcceptsEmpty asserts an empty
// payload decodes cleanly (zero records). All TLV fields are optional
// at the decoder layer; semantic validation rejects missing fields
// later in the pipeline, but the decoder itself must not error on
// zero-length input.
func TestParseReceiveScriptProofMessageAcceptsEmpty(t *testing.T) {
	t.Parallel()

	msg, err := parseReceiveScriptProofMessage(nil)
	require.NoError(t, err)
	require.NotNil(t, msg)
}
