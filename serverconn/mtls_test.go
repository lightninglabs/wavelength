package serverconn

import (
	"crypto/x509"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// TestGenerateClientTLSCert verifies that the generated TLS certificate
// has the expected properties: correct CN, client auth extended key
// usage, and a valid cert/key pair.
func TestGenerateClientTLSCert(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKey := privKey.PubKey()

	cert, err := GenerateClientTLSCert(pubKey)
	require.NoError(t, err)

	// The leaf certificate should be populated.
	require.NotNil(t, cert.Leaf)
	require.NotNil(t, cert.PrivateKey)
	require.Len(t, cert.Certificate, 1)

	// CN should be the hex-encoded compressed pubkey.
	expectedCN := PubKeyMailboxID(pubKey)
	require.Equal(t, expectedCN, cert.Leaf.Subject.CommonName)

	// Extended key usage should include client auth.
	require.Contains(
		t, cert.Leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth,
	)

	// The certificate should be parseable and have a valid
	// signature (self-signed with the ephemeral P-256 key).
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)

	err = parsed.CheckSignature(
		parsed.SignatureAlgorithm, parsed.RawTBSCertificate,
		parsed.Signature,
	)
	require.NoError(t, err)
}

// TestGenerateClientTLSCertNilKey verifies that a nil pubkey returns
// a descriptive error.
func TestGenerateClientTLSCertNilKey(t *testing.T) {
	t.Parallel()

	_, err := GenerateClientTLSCert(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must not be nil")
}
