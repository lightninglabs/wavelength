package wallet

import (
	"bytes"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestComposeRefreshTemplate verifies that the wallet-side refresh helper
// rebuilds the new VTXO output template with the operator-key placeholder
// rather than any concrete operator key. The server binds its current key at
// admission, which removes the refresh-after-rotation problem at the root —
// the new output never commits to the old (or any) concrete operator key on
// the client side. The owner key and exit delay are carried over from the
// input descriptor.
func TestComposeRefreshTemplate(t *testing.T) {
	t.Parallel()

	t.Run("rebuilds with operator placeholder", func(t *testing.T) {
		t.Parallel()

		ownerKey := newTestPubKey(t)
		k1 := newTestPubKey(t)

		const exitDelay = uint32(144)
		storedTemplate, err := arkscript.EncodeStandardVTXOTemplate(
			ownerKey, k1, exitDelay,
		)
		require.NoError(t, err)

		descriptor := &VTXODescriptor{PolicyTemplate: storedTemplate}

		rebuilt, err := composeRefreshTemplate(descriptor)
		require.NoError(t, err)

		params := decodeWalletStandardParams(t, rebuilt)
		require.True(
			t, xOnlyPubKeyEqual(
				params.OperatorKey,
				&arkscript.OperatorKeyPlaceholder,
			),
			"emitted template must commit to the operator "+
				"placeholder, not a concrete key",
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

	t.Run("rejects custom shape", func(t *testing.T) {
		t.Parallel()

		// A descriptor whose stored template is not the standard VTXO
		// shape cannot be safely rewritten from a historical concrete
		// operator key to the unbound placeholder.
		descriptor := &VTXODescriptor{
			PolicyTemplate: []byte{
				0x00,
			},
		}

		_, err := composeRefreshTemplate(descriptor)
		require.True(
			t, errors.Is(err, ErrRefreshOperatorKeyUnsupported),
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
