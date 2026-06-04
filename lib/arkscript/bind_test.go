package arkscript

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// newTestKey returns a fresh random pubkey for binding tests.
func newTestKey(t *testing.T) *btcec.PublicKey {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return priv.PubKey()
}

// TestBindOperatorKeyStandard checks that a standard template built with the
// operator-key placeholder binds to a concrete operator key and produces the
// exact same template (and pkScript) as one built with that key directly.
func TestBindOperatorKeyStandard(t *testing.T) {
	t.Parallel()

	owner := newTestKey(t)
	operator := newTestKey(t)
	const exitDelay = 144

	placeholderTmpl, err := StandardVTXOTemplate(
		owner, &OperatorKeyPlaceholder, exitDelay,
	)
	require.NoError(t, err)

	bound, err := placeholderTmpl.BindOperatorKey(operator)
	require.NoError(t, err)

	directTmpl, err := StandardVTXOTemplate(owner, operator, exitDelay)
	require.NoError(t, err)

	// The bound template must yield the same pkScript as one built with
	// the operator key from the start.
	boundScript, err := bound.PkScript()
	require.NoError(t, err)

	directScript, err := directTmpl.PkScript()
	require.NoError(t, err)

	require.Equal(t, directScript, boundScript)

	// The bound operator key must be recoverable from the bound template.
	params, err := DecodeStandardVTXOParams(bound)
	require.NoError(t, err)
	require.Equal(
		t, operator.SerializeCompressed(),
		params.OperatorKey.SerializeCompressed(),
	)
}

// TestBindOperatorKeyNoPlaceholder rejects a template that never references
// the operator placeholder — every VTXO output must commit to the operator.
func TestBindOperatorKeyNoPlaceholder(t *testing.T) {
	t.Parallel()

	owner := newTestKey(t)
	operator := newTestKey(t)

	// A template already built with a concrete (non-placeholder) operator
	// key has no placeholder to bind.
	concrete, err := StandardVTXOTemplate(owner, operator, 144)
	require.NoError(t, err)

	_, err = concrete.BindOperatorKey(newTestKey(t))
	require.ErrorContains(t, err, "no operator key placeholder")
}

// TestBindOperatorKeyNilOperator rejects a nil operator key.
func TestBindOperatorKeyNilOperator(t *testing.T) {
	t.Parallel()

	owner := newTestKey(t)
	tmpl, err := StandardVTXOTemplate(owner, &OperatorKeyPlaceholder, 144)
	require.NoError(t, err)

	_, err = tmpl.BindOperatorKey(nil)
	require.ErrorContains(t, err, "operator key is nil")
}
