package wallet

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestComposeRefreshTemplate is the wallet-side regression, parallel to
// vtxo.TestRefreshEmissionUsesJoinTimeOperatorKey. handleRefreshVTXOs
// hoists a single GetInfo fetch out of its per-outpoint loop and feeds
// the resolved key into composeRefreshTemplate per VTXO; this test
// exercises that helper directly.
func TestComposeRefreshTemplate(t *testing.T) {
	t.Parallel()

	t.Run("rebuilds against resolved key", func(t *testing.T) {
		t.Parallel()

		ownerKey := newTestPubKey(t)
		k1 := newTestPubKey(t)
		k2 := newTestPubKey(t)

		const exitDelay = uint32(144)
		storedTemplate, err := arkscript.EncodeStandardVTXOTemplate(
			ownerKey, k1, exitDelay,
		)
		require.NoError(t, err)

		descriptor := &VTXODescriptor{PolicyTemplate: storedTemplate}

		rebuilt, err := composeRefreshTemplate(descriptor, k2)
		require.NoError(t, err)

		params := decodeWalletStandardParams(t, rebuilt)
		require.True(
			t, xOnlyPubKeyEqual(params.OperatorKey, k2),
			"emitted template must commit to the resolved key K2",
		)
		require.False(
			t, xOnlyPubKeyEqual(params.OperatorKey, k1),
			"emitted template must no longer carry K1",
		)
		require.True(
			t, xOnlyPubKeyEqual(params.OwnerKey, ownerKey),
			"owner key must be preserved across the rebuild",
		)
		require.Equal(
			t, exitDelay, params.ExitDelay,
			"exit delay must be preserved across the rebuild",
		)
	})

	t.Run("returns stored bytes when key unresolved", func(t *testing.T) {
		t.Parallel()

		ownerKey := newTestPubKey(t)
		k1 := newTestPubKey(t)

		storedTemplate, err := arkscript.EncodeStandardVTXOTemplate(
			ownerKey, k1, 144,
		)
		require.NoError(t, err)

		descriptor := &VTXODescriptor{PolicyTemplate: storedTemplate}

		out, err := composeRefreshTemplate(descriptor, nil)
		require.NoError(t, err)
		require.True(
			t, bytes.Equal(out, storedTemplate),
			"with no resolved key the helper must return the "+
				"stored template verbatim (harness path)",
		)
	})
}

// newTestPubKey returns a freshly generated secp256k1 public key.
func newTestPubKey(t *testing.T) *btcec.PublicKey {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return priv.PubKey()
}

// xOnlyPubKeyEqual compares two secp256k1 keys by their Schnorr (x-only)
// serialization. Ark policy templates commit to the x-only form, so y-
// parity differences between the encoded and freshly-derived pubkey must
// not be treated as a mismatch.
func xOnlyPubKeyEqual(a, b *btcec.PublicKey) bool {
	if a == nil || b == nil {
		return false
	}

	return bytes.Equal(
		schnorr.SerializePubKey(a), schnorr.SerializePubKey(b),
	)
}

// decodeWalletStandardParams decodes a serialized standard VTXO policy
// template into its (owner, operator, exit-delay) triple.
func decodeWalletStandardParams(t *testing.T,
	raw []byte) *arkscript.StandardVTXOParams {

	t.Helper()

	template, err := arkscript.DecodePolicyTemplate(raw)
	require.NoError(t, err, "decode policy template")

	params, err := arkscript.DecodeStandardVTXOParams(template)
	require.NoError(t, err, "decode standard VTXO params")

	return params
}
