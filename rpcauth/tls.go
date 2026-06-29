package rpcauth

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	lndcert "github.com/lightningnetwork/lnd/cert"
	"google.golang.org/grpc/credentials"
)

const tlsCertValidity = 14 * 30 * 24 * time.Hour

// EnsureTLSCert loads an existing TLS cert/key pair or creates a self-signed
// pair when both paths are empty on disk.
func EnsureTLSCert(certPath, keyPath, organization string) error {
	if certPath == "" || keyPath == "" {
		return fmt.Errorf("tls cert and key paths are required")
	}

	certExists, err := fileExists(certPath)
	if err != nil {
		return err
	}
	keyExists, err := fileExists(keyPath)
	if err != nil {
		return err
	}

	switch {
	case certExists && keyExists:
		return nil

	case certExists || keyExists:
		return fmt.Errorf("partial tls keypair at %s and %s", certPath,
			keyPath)
	}

	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return fmt.Errorf("create tls cert dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return fmt.Errorf("create tls key dir: %w", err)
	}

	cert, key, err := lndcert.GenCertPair(
		organization, nil, nil, false, tlsCertValidity,
	)
	if err != nil {
		return fmt.Errorf("generate tls cert: %w", err)
	}

	if err := lndcert.WriteCertPair(
		certPath, keyPath, cert, key,
	); err != nil {
		return fmt.Errorf("write tls cert: %w", err)
	}
	if err := os.Chmod(certPath, 0o600); err != nil {
		return fmt.Errorf("chmod tls cert: %w", err)
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		return fmt.Errorf("chmod tls key: %w", err)
	}

	return nil
}

// ServerTLSCredentials returns server credentials for a TLS cert/key pair.
func ServerTLSCredentials(certPath,
	keyPath string) (credentials.TransportCredentials, error) {

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load tls keypair: %w", err)
	}

	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}), nil
}

// ClientTLSCredentials returns client credentials rooted at certPath.
func ClientTLSCredentials(certPath string) (credentials.TransportCredentials,
	error) {

	pool, err := rootPool(certPath)
	if err != nil {
		return nil, err
	}

	return credentials.NewTLS(&tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}), nil
}

// HTTPClientForCert returns an HTTP client rooted at certPath.
func HTTPClientForCert(certPath string) (*http.Client, error) {
	pool, err := rootPool(certPath)
	if err != nil {
		return nil, err
	}

	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}, nil
}

// rootPool returns a certificate pool rooted at certPath or system roots.
func rootPool(certPath string) (*x509.CertPool, error) {
	if certPath == "" {
		systemPool, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system cert pool: %w", err)
		}

		return systemPool, nil
	}

	pool := x509.NewCertPool()
	certBytes, err := os.ReadFile(certPath) // #nosec G304
	if err != nil {
		return nil, fmt.Errorf("read tls cert: %w", err)
	}
	if !pool.AppendCertsFromPEM(certBytes) {
		return nil, fmt.Errorf("parse tls cert: %s", certPath)
	}

	return pool, nil
}

// fileExists returns whether path exists.
func fileExists(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return true, nil
	} else if os.IsNotExist(err) {
		return false, nil
	} else {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
}
