// Package bitcoindrpc provides a direct-to-bitcoind JSON-RPC
// implementation of chainbackends.PackageSubmitter, used by the
// production daemon (and by integration tests) when the LND v3
// submitpackage path is not available.
//
// The shape of the helper is deliberately narrow: it knows how to call
// bitcoind's submitpackage method and nothing else, so it can be wired
// into waved.Config without pulling in harness-only code or opening
// the rest of a bitcoind RPC surface.
package bitcoindrpc

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/wire/v2"
)

// submitTimeout bounds the submitpackage RPC so a wedged bitcoind
// can't stall the caller for the full parent context timeout (test or
// production).
const submitTimeout = 30 * time.Second

// PackageSubmitter implements chainbackends.PackageSubmitter via
// direct JSON-RPC calls to a bitcoind node.
type PackageSubmitter struct {
	url      string
	user     string
	password string

	// client is a dedicated HTTP client so we don't share state with
	// http.DefaultClient, and carries a timeout as a backstop for
	// callers that pass in a never-cancelled context.
	client *http.Client
}

// Option configures a PackageSubmitter.
type Option func(*options)

type options struct {
	tlsCertPath string
}

// WithTLSCertPath configures the HTTPS submitter to trust the CA
// certificate at path. It is intended for operators fronting bitcoind's
// plain HTTP RPC server with a local TLS reverse proxy.
func WithTLSCertPath(path string) Option {
	return func(o *options) {
		o.tlsCertPath = path
	}
}

// New creates a submitter that talks to the given bitcoind JSON-RPC
// endpoint. Bare host:port inputs default to http:// for compatibility.
// Use NewWithOptions when endpoint parsing or TLS configuration errors
// need to be surfaced to the caller.
func New(host, user, password string) *PackageSubmitter {
	submitter, err := NewWithOptions(host, user, password)
	if err != nil {

		// New preserves the legacy no-error constructor for callers
		// that pass the historical bare host:port form. The fallback is
		// kept intentionally narrow: option-using callers should use
		// NewWithOptions so malformed URLs and TLS config errors
		// surface.
		return &PackageSubmitter{
			url:      fmt.Sprintf("http://%s", host),
			user:     user,
			password: password,
			client: &http.Client{
				Timeout: submitTimeout,
			},
		}
	}

	return submitter
}

// NewWithOptions creates a submitter that talks to the given bitcoind
// JSON-RPC endpoint. Bare host:port inputs default to http:// unless a
// TLS certificate path is provided, in which case they default to
// https://. Full http:// and https:// URLs are accepted as-is.
func NewWithOptions(host, user, password string,
	opts ...Option) (*PackageSubmitter, error) {

	var cfg options
	for _, opt := range opts {
		opt(&cfg)
	}

	endpoint, err := endpointURL(host, cfg.tlsCertPath != "")
	if err != nil {
		return nil, err
	}
	if cfg.tlsCertPath != "" && endpoint.Scheme != "https" {
		return nil, fmt.Errorf("bitcoind TLS cert requires https "+
			"endpoint, got %q", endpoint.Scheme)
	}

	client, err := httpClient(endpoint.Scheme, cfg.tlsCertPath)
	if err != nil {
		return nil, err
	}

	return &PackageSubmitter{
		url:      endpoint.String(),
		user:     user,
		password: password,
		client:   client,
	}, nil
}

// endpointURL normalizes the operator-supplied bitcoind endpoint into a
// concrete http:// or https:// URL.
func endpointURL(host string, preferHTTPS bool) (*url.URL, error) {
	if strings.TrimSpace(host) == "" {
		return nil, fmt.Errorf("bitcoind host is required")
	}

	raw := host
	if !strings.Contains(raw, "://") {
		scheme := "http"
		if preferHTTPS {
			scheme = "https"
		}
		raw = scheme + "://" + raw
	}

	endpoint, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse bitcoind endpoint %q: %w", host,
			err)
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, fmt.Errorf("unsupported bitcoind endpoint "+
			"scheme %q", endpoint.Scheme)
	}
	if endpoint.Host == "" {
		return nil, fmt.Errorf("bitcoind endpoint %q has no host", host)
	}

	return endpoint, nil
}

// httpClient returns the HTTP client used for bitcoind RPC calls.
func httpClient(scheme, tlsCertPath string) (*http.Client, error) {
	// Always start from a clone of http.DefaultTransport so the
	// submitter keeps connection pooling, keep-alives, dialer, and
	// idle timeouts on every path (plain HTTP, HTTPS with the system
	// trust store, or HTTPS with a custom CA) and never shares the
	// process-global transport. The fallback only matters if a future
	// stdlib change swaps the default-transport concrete type.
	var transport *http.Transport
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = dt.Clone()
	} else {
		transport = &http.Transport{}
	}

	// Only an https endpoint with a custom CA needs a bespoke TLS
	// config; an https endpoint without one verifies against the
	// clone's default (system) trust store.
	if scheme == "https" && tlsCertPath != "" {
		//nolint:gosec // G304: cert path comes from operator config.
		certBytes, err := os.ReadFile(tlsCertPath)
		if err != nil {
			return nil, fmt.Errorf("read bitcoind TLS cert %q: %w",
				tlsCertPath, err)
		}

		// Augment the system trust store rather than replacing it so
		// a custom CA is trusted in addition to the public roots.
		// Fall back to an empty pool only if the system pool is
		// unavailable.
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(certBytes) {
			return nil, fmt.Errorf("bitcoind TLS cert %q contains "+
				"no PEM certificates", tlsCertPath)
		}

		transport.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		}
	}

	return &http.Client{
		Timeout:   submitTimeout,
		Transport: transport,
	}, nil
}

// SubmitPackage implements chainbackends.PackageSubmitter by calling
// bitcoind's submitpackage JSON-RPC method.
func (s *PackageSubmitter) SubmitPackage(ctx context.Context,
	parents []*wire.MsgTx, child *wire.MsgTx, _ *float64) (
	*btcjson.SubmitPackageResult, error) {

	// Serialize all transactions to hex.
	txHexes := make([]string, 0, len(parents)+1)
	for _, tx := range parents {
		var buf bytes.Buffer
		if err := tx.Serialize(&buf); err != nil {
			return nil, fmt.Errorf("serialize parent: %w", err)
		}

		txHexes = append(txHexes, hex.EncodeToString(buf.Bytes()))
	}

	var childBuf bytes.Buffer
	if err := child.Serialize(&childBuf); err != nil {
		return nil, fmt.Errorf("serialize child: %w", err)
	}

	txHexes = append(txHexes, hex.EncodeToString(childBuf.Bytes()))

	// Build the JSON-RPC request. Set maxfeerate=0 to disable the
	// per-transaction feerate limit. The CPFP child intentionally has
	// a high feerate to pay for the zero-fee parent, and the default
	// maxfeerate (0.10 BTC/kvB) would reject it.
	rpcReq := map[string]any{
		"jsonrpc": "1.0",
		"id":      "wavelength",
		"method":  "submitpackage",
		"params": []any{
			txHexes,
			0,
		},
	}

	body, err := json.Marshal(rpcReq)
	if err != nil {
		return nil, fmt.Errorf("marshal rpc request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, s.url, bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("create http request: %w", err)
	}

	httpReq.SetBasicAuth(s.user, s.password)
	httpReq.Header.Set("Content-Type", "application/json")

	// URL is the operator-configured bitcoind JSON-RPC endpoint,
	// not caller-controlled input; it is the same s.url used for
	// every other request on this submitter.
	resp, err := s.client.Do(httpReq) //nolint:gosec // G704: trusted URL
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	// Parse the JSON-RPC response. bitcoind returns HTTP 500 with a
	// JSON-RPC error envelope for method-level errors, so we try to
	// decode the body first and only fall back to an HTTP-level error
	// when the body does not parse (e.g. HTML error pages from 401/
	// 403/proxy errors).
	var rpcResp struct {
		Result *btcjson.SubmitPackageResult `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("submit package HTTP %d: %s",
				resp.StatusCode, string(respBody))
		}

		return nil, fmt.Errorf("unmarshal response: %w (body: %s)", err,
			string(respBody))
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("bitcoind error %d: %s",
			rpcResp.Error.Code, rpcResp.Error.Message)
	}

	if rpcResp.Result == nil {
		return nil, fmt.Errorf("nil result from submitpackage "+
			"(body: %s)", string(respBody))
	}

	return rpcResp.Result, nil
}
