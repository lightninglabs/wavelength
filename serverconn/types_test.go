package serverconn

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/stretchr/testify/require"
)

// TestMergeAuthHeadersNilSig verifies that when AuthSignature is nil,
// the source headers are returned unchanged.
func TestMergeAuthHeadersNilSig(t *testing.T) {
	t.Parallel()

	cfg := &ConnectorConfig{}
	src := map[string]string{"foo": "bar"}
	result := cfg.mergeAuthHeaders(src)
	require.Equal(t, src, result)
}

// TestMergeAuthHeadersNilSrc verifies that when src is nil, the result
// contains only the auth header.
func TestMergeAuthHeadersNilSrc(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	sig, err := SignMailboxAuth(privKey, "recipient")
	require.NoError(t, err)

	cfg := &ConnectorConfig{AuthSignature: sig}
	result := cfg.mergeAuthHeaders(nil)
	require.Len(t, result, 1)
	require.Contains(t, result, AuthHeaderKey)
}

// TestMergeAuthHeadersAuthWins verifies that the auth header always
// takes precedence over a caller-provided header with the same key.
func TestMergeAuthHeadersAuthWins(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	sig, err := SignMailboxAuth(privKey, "recipient")
	require.NoError(t, err)

	cfg := &ConnectorConfig{AuthSignature: sig}

	// Caller tries to inject a fake auth signature.
	src := map[string]string{
		AuthHeaderKey: "fake-sig",
		"other":       "value",
	}
	result := cfg.mergeAuthHeaders(src)

	// The real auth signature must win.
	expectedSigHex := schnorr.SerializePubKey(
		privKey.PubKey(),
	)
	_ = expectedSigHex

	require.NotEqual(t, "fake-sig", result[AuthHeaderKey])
	require.Equal(t, "value", result["other"])
}

// TestPubKeyMailboxIDNilPanics verifies that a nil key causes a panic.
func TestPubKeyMailboxIDNilPanics(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() {
		PubKeyMailboxID(nil)
	})
}
