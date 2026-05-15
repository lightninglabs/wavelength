package serverconn

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
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
	cfg.InitAuthHeader()
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
	cfg.InitAuthHeader()

	// Caller tries to inject a fake auth signature.
	src := map[string]string{
		AuthHeaderKey: "fake-sig",
		"other":       "value",
	}
	result := cfg.mergeAuthHeaders(src)

	// The real auth signature must win.
	expectedSigHex := hex.EncodeToString(sig.Serialize())

	require.Equal(t, expectedSigHex, result[AuthHeaderKey])
	require.Equal(t, "value", result["other"])
}

// TestMergeAuthHeadersIncludesTLSBindSig verifies that when both
// AuthSignature and TLSBindSignature are configured, mergeAuthHeaders
// emits both headers and the TLS-binding header survives a caller-
// supplied collision (same precedence rule as the auth header).
func TestMergeAuthHeadersIncludesTLSBindSig(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	authSig, err := SignMailboxAuth(privKey, "recipient")
	require.NoError(t, err)

	bindSig, err := SignMailboxTLSBind(privKey, []byte("leaf-spki"))
	require.NoError(t, err)

	cfg := &ConnectorConfig{
		AuthSignature:    authSig,
		TLSBindSignature: bindSig,
	}
	cfg.InitAuthHeader()

	// Fast path: nil src returns the cached singleton with both
	// headers present.
	result := cfg.mergeAuthHeaders(nil)
	require.Contains(t, result, AuthHeaderKey)
	require.Contains(t, result, TLSBindHeaderKey)

	expectedBindHex := hex.EncodeToString(bindSig.Serialize())
	require.Equal(t, expectedBindHex, result[TLSBindHeaderKey])

	// Slow path: caller-supplied headers, with a hostile attempt
	// to override the TLS-binding header. Real binding sig wins.
	src := map[string]string{
		TLSBindHeaderKey: "fake-bind-sig",
		"other":          "value",
	}
	result = cfg.mergeAuthHeaders(src)
	require.Equal(t, expectedBindHex, result[TLSBindHeaderKey])
	require.Equal(t, "value", result["other"])
}

// TestPubKeyMailboxIDNilPanics verifies that a nil key causes a panic.
func TestPubKeyMailboxIDNilPanics(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() {
		PubKeyMailboxID(nil)
	})
}
