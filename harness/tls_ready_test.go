package harness

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestLndTLSReady verifies partial certificate files are not treated as ready.
func TestLndTLSReady(t *testing.T) {
	t.Parallel()

	certPath := filepath.Join(t.TempDir(), "tls.cert")

	require.NoError(
		t,
		os.WriteFile(
			certPath, []byte("-----BEGIN CERTIFICATE-----\n"),
			0o600,
		),
	)
	require.False(t, lndTLSReady(certPath))

	require.NoError(
		t,
		os.WriteFile(
			certPath, testCertificatePEM(t), 0o600,
		),
	)
	require.True(t, lndTLSReady(certPath))
}

// testCertificatePEM returns a valid self-signed certificate in PEM form.
func testCertificatePEM(t *testing.T) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(time.Hour),
	}

	der, err := x509.CreateCertificate(
		rand.Reader, template, template, &key.PublicKey, key,
	)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: der,
	})
}
