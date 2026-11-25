//go:build !js || !wasm

package waved

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"github.com/lightninglabs/tap-sdk/embedded"
	"github.com/lightninglabs/tap-sdk/embedded/courier"
	"github.com/lightninglabs/wavelength/db"
)

// assetReceiveConfig returns what the asset engine needs to create Taproot
// Asset addresses and to receive at them: the address store, the wallet's
// keys and a dialer of proof couriers over gRPC. A daemon with no courier
// configured still gets it, so it keeps receiving at the addresses an
// earlier configuration created.
func (s *Server) assetReceiveConfig(store *db.AssetEngineStore,
	keys *assetKeyRing) (*embedded.ReceiveConfig, error) {

	defaultCourier, err := s.cfg.Assets.Courier.addr()
	if err != nil {
		return nil, err
	}
	overrides, err := s.cfg.Assets.Courier.endpoints()
	if err != nil {
		return nil, err
	}

	endpoints := make(map[string]courier.Endpoint, len(overrides))
	for host, override := range overrides {
		endpoint := courier.Endpoint{Target: override.target}
		if override.tlsCertPath != "" {
			endpoint.TLS, err = courierTLSConfig(
				override.tlsCertPath,
			)
			if err != nil {
				return nil, fmt.Errorf(
					"assets.courier.endpoints: courier "+
						"%q: %w", host, err)
			}
		}
		endpoints[host] = endpoint
	}

	return &embedded.ReceiveConfig{
		Store:          store,
		Keys:           keys,
		Dial:           courier.NewDialer(endpoints),
		DefaultCourier: defaultCourier,
	}, nil
}

// courierTLSConfig returns a TLS configuration that trusts only the
// certificates of a PEM file.
func courierTLSConfig(certPath string) (*tls.Config, error) {
	pem, err := os.ReadFile(certPath) // #nosec G304
	if err != nil {
		return nil, fmt.Errorf("read TLS certificate: %w", err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no PEM certificate", certPath)
	}

	return &tls.Config{
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
	}, nil
}
