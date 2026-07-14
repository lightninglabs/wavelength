package arkscript

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/internal/testutils"
	"github.com/stretchr/testify/require"
)

// requireSameXOnlyKey asserts that two public keys agree on their x-only
// form. The standard VTXO policy template encodes keys as 32-byte x-only
// values, so any odd-parity input is lifted to even parity on decode;
// comparing via IsEqual would spuriously fail for odd-y inputs, while the
// x-only projection is the actual invariant the protocol preserves.
func requireSameXOnlyKey(t *testing.T, want, got *btcec.PublicKey) {
	t.Helper()

	require.Equal(
		t, schnorr.SerializePubKey(want), schnorr.SerializePubKey(got),
	)
}

// TestEncodeStandardVTXOArtifacts covers the helper that returns the
// (policyTemplate, pkScript) pair for the standard Ark VTXO shape. The
// helper is now on the production wallet send path (via buildSendVTXORequests
// → EncodeStandardVTXOArtifacts), so regression coverage on both the happy
// path and the three fail-closed precondition paths is essential.
func TestEncodeStandardVTXOArtifacts(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)
	const exitDelay uint32 = 144

	// Happy path: the returned policyTemplate must round-trip through
	// DecodeStandardVTXOParams back to the original tuple, and the
	// returned pkScript must equal the P2TR script derived from the
	// compiled policy's output key. This is the dual-invariant check
	// that callers depend on.
	happyName := "happy path round-trips and matches policy pkScript"
	t.Run(happyName, func(t *testing.T) {
		t.Parallel()

		template, pkScript, err := EncodeStandardVTXOArtifacts(
			ownerKey, operatorKey, exitDelay,
		)
		require.NoError(t, err)
		require.NotEmpty(t, template)
		require.NotEmpty(t, pkScript)

		// The encoded template must decode back to the exact
		// (owner, operator, exitDelay) tuple the helper was
		// called with.
		decoded, err := DecodePolicyTemplate(template)
		require.NoError(t, err)

		params, err := DecodeStandardVTXOParams(decoded)
		require.NoError(t, err)
		requireSameXOnlyKey(t, ownerKey, params.OwnerKey)
		requireSameXOnlyKey(t, operatorKey, params.OperatorKey)
		require.Equal(t, exitDelay, params.ExitDelay)

		// The returned pkScript must equal the canonical P2TR
		// derived from the compiled policy's output key. If these
		// diverge, the wallet would quote one script to the
		// counterparty and the compiler would expect a different
		// one.
		policy, err := NewVTXOPolicy(
			ownerKey, operatorKey, exitDelay,
		)
		require.NoError(t, err)

		expectedPkScript, err := txscript.PayToTaprootScript(
			policy.OutputKey(),
		)
		require.NoError(t, err)
		require.Equal(t, expectedPkScript, pkScript)
	})

	// A nil owner key must be rejected fail-closed: returning a
	// well-formed pkScript built from a zero-valued key would let the
	// wallet quote a recipient policy no one can spend.
	t.Run("nil owner key rejected", func(t *testing.T) {
		t.Parallel()

		template, pkScript, err := EncodeStandardVTXOArtifacts(
			nil, operatorKey, exitDelay,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "owner key is nil")
		require.Nil(t, template)
		require.Nil(t, pkScript)
	})

	// Likewise for a nil operator key.
	t.Run("nil operator key rejected", func(t *testing.T) {
		t.Parallel()

		template, pkScript, err := EncodeStandardVTXOArtifacts(
			ownerKey, nil, exitDelay,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "operator key is nil")
		require.Nil(t, template)
		require.Nil(t, pkScript)
	})

	// A zero exit delay is rejected fail-closed because a 1-block CSV
	// would break the forfeit incentive; the helper must not silently
	// substitute a default.
	t.Run("zero exit delay rejected", func(t *testing.T) {
		t.Parallel()

		template, pkScript, err := EncodeStandardVTXOArtifacts(
			ownerKey, operatorKey, 0,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "exit delay must be non-zero")
		require.Nil(t, template)
		require.Nil(t, pkScript)
	})

	// Changing any input parameter must change the pkScript. This is a
	// light fuzz-like sanity check that the helper is actually
	// threading every parameter into the compiled output key and not,
	// e.g., hashing only the owner key.
	t.Run("all inputs influence the pkScript", func(t *testing.T) {
		t.Parallel()

		_, baselinePkScript, err := EncodeStandardVTXOArtifacts(
			ownerKey, operatorKey, exitDelay,
		)
		require.NoError(t, err)

		otherOwner, _ := testutils.CreateKey(3)
		_, pkScriptOwner, err := EncodeStandardVTXOArtifacts(
			otherOwner, operatorKey, exitDelay,
		)
		require.NoError(t, err)
		require.NotEqual(t, baselinePkScript, pkScriptOwner)

		otherOperator, _ := testutils.CreateKey(4)
		_, pkScriptOperator, err := EncodeStandardVTXOArtifacts(
			ownerKey, otherOperator, exitDelay,
		)
		require.NoError(t, err)
		require.NotEqual(t, baselinePkScript, pkScriptOperator)

		_, pkScriptDelay, err := EncodeStandardVTXOArtifacts(
			ownerKey, operatorKey, exitDelay+1,
		)
		require.NoError(t, err)
		require.NotEqual(t, baselinePkScript, pkScriptDelay)
	})
}

// TestStandardVTXOTemplateRoundTrip exercises the round trip through the
// encode/decode pair at the template level: serialize via
// EncodeStandardVTXOTemplate, decode via DecodePolicyTemplate, extract
// parameters via DecodeStandardVTXOParams.
func TestStandardVTXOTemplateRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		owner     int32
		operator  int32
		exitDelay uint32
	}{
		{
			"typical",
			1,
			2,
			144,
		},
		{
			"short delay",
			5,
			6,
			1,
		},
		{
			"large delay",
			7,
			8,
			100_000,
		},
		{
			"distinct high indices",
			42,
			99,
			2016,
		},
	}

	for _, tc := range cases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ownerKey, _ := testutils.CreateKey(tc.owner)
			operatorKey, _ := testutils.CreateKey(tc.operator)

			encoded, err := EncodeStandardVTXOTemplate(
				ownerKey, operatorKey, tc.exitDelay,
			)
			require.NoError(t, err)

			decoded, err := DecodePolicyTemplate(encoded)
			require.NoError(t, err)

			params, err := DecodeStandardVTXOParams(decoded)
			require.NoError(t, err)
			requireSameXOnlyKey(t, ownerKey, params.OwnerKey)
			requireSameXOnlyKey(
				t, operatorKey, params.OperatorKey,
			)
			require.Equal(t, tc.exitDelay, params.ExitDelay)

			// IsStandardVTXOTemplate must agree with the decode
			// result — they are the two entry points and must not
			// drift.
			require.True(t, IsStandardVTXOTemplate(decoded))
		})
	}
}

// TestEncodeStandardVTXOArtifactsPkScriptMatchesWallet asserts that the
// pkScript returned by the artifact helper equals the compiled policy's
// PkScript(). The wallet path quotes the helper's pkScript to counterparties
// and later verification paths compile the template via PolicyTemplate →
// PkScript(); if the two ever diverge the wallet would send funds to a
// script the operator refuses to accept.
func TestEncodeStandardVTXOArtifactsPkScriptMatchesWallet(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(11)
	operatorKey, _ := testutils.CreateKey(12)

	templateBytes, pkScript, err := EncodeStandardVTXOArtifacts(
		ownerKey, operatorKey, 144,
	)
	require.NoError(t, err)

	template, err := DecodePolicyTemplate(templateBytes)
	require.NoError(t, err)

	templatePkScript, err := template.PkScript()
	require.NoError(t, err)

	require.Equal(t, templatePkScript, pkScript)
}
