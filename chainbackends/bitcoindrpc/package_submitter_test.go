package bitcoindrpc

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewWithOptionsURLAndTLS verifies endpoint normalization and custom
// CA wiring for TLS-fronted bitcoind RPC deployments.
func TestNewWithOptionsURLAndTLS(t *testing.T) {
	t.Parallel()

	certPath := writeTestCert(t)

	tests := []struct {
		name       string
		host       string
		certPath   string
		wantURL    string
		wantTLS    bool
		wantErrSub string
	}{
		{
			name:    "bare host defaults to http",
			host:    "127.0.0.1:8332",
			wantURL: "http://127.0.0.1:8332",
		},
		{
			name:    "https URL is preserved",
			host:    "https://bitcoind.example:8332",
			wantURL: "https://bitcoind.example:8332",
		},
		{
			name:     "cert path upgrades bare host to https",
			host:     "bitcoind.example:8332",
			certPath: certPath,
			wantURL:  "https://bitcoind.example:8332",
			wantTLS:  true,
		},
		{
			name:     "https URL accepts custom cert",
			host:     "https://bitcoind.example:8332",
			certPath: certPath,
			wantURL:  "https://bitcoind.example:8332",
			wantTLS:  true,
		},
		{
			name:       "custom cert rejects explicit http",
			host:       "http://bitcoind.example:8332",
			certPath:   certPath,
			wantErrSub: "requires https",
		},
		{
			name:       "unsupported scheme",
			host:       "ftp://bitcoind.example:8332",
			wantErrSub: "unsupported",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			submitter, err := NewWithOptions(
				tc.host, "user", "pass",
				WithTLSCertPath(tc.certPath),
			)
			if tc.wantErrSub != "" {
				require.ErrorContains(t, err, tc.wantErrSub)

				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantURL, submitter.url)

			if !tc.wantTLS {
				return
			}

			transport, ok := submitter.client.Transport.(*http.
				Transport)
			require.True(t, ok)
			require.NotSame(t, http.DefaultTransport, transport)
			require.NotNil(t, transport.TLSClientConfig)
			require.NotNil(t, transport.TLSClientConfig.RootCAs)
		})
	}
}

// writeTestCert writes a valid PEM certificate for TLS configuration tests.
func writeTestCert(t *testing.T) string {
	t.Helper()

	server := httptest.NewTLSServer(
		http.HandlerFunc(
			func(http.ResponseWriter, *http.Request) {},
		),
	)
	t.Cleanup(server.Close)

	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: server.Certificate().Raw,
	})

	path := filepath.Join(t.TempDir(), "bitcoind.pem")
	err := os.WriteFile(path, pemBytes, 0o600)
	require.NoError(t, err)

	return path
}
