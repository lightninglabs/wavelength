// Package lndrest provides REST implementations of the lndclient service
// interfaces the waved wallet backend uses, so the daemon can talk to lnd over
// its grpc-gateway HTTP/JSON interface instead of native gRPC. It reuses the
// shared rpc/restclient transport (HTTP + protoJSON + streaming) and speaks to
// lnd's published REST paths (the pattern_* routes generated into lnd's
// *.pb.gw.go files) using the lnd proto request/response types for marshaling.
//
// Only the method surface the wallet backend actually exercises is
// implemented; every other interface method returns errUnsupportedOverREST so
// an unexpected caller fails loudly rather than silently misbehaving.
package lndrest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/rpc/restclient"
	"github.com/lightningnetwork/lnd/routing/route"
	"google.golang.org/protobuf/proto"
)

// defaultRPCTimeout bounds each unary REST call to lnd. It mirrors the daemon's
// DefaultRPCTimeout so a REST-backed backend behaves like the gRPC one when the
// caller leaves the timeout unset; streaming notification calls deliberately
// bypass it since they are long-lived.
const defaultRPCTimeout = 30 * time.Second

// macaroonHeader is the HTTP header lnd's REST gateway reads the hex-encoded
// macaroon from for request authentication. lnd matches it case-insensitively;
// the canonical form is used so it round-trips cleanly through net/http.
const macaroonHeader = "Grpc-Metadata-Macaroon"

// errUnsupportedOverREST is returned by interface methods the REST backend does
// not implement. The wallet backend never calls these; the stubs exist only to
// satisfy the wide lndclient interfaces, and returning a clear error means an
// unexpected new caller fails loudly instead of silently misbehaving.
var errUnsupportedOverREST = fmt.Errorf("lnd method not supported over the " +
	"REST transport")

// Config parameterizes a REST-backed lnd connection.
type Config struct {
	// Host is the lnd REST gateway endpoint. A bare host:port is assumed to
	// be TLS (https) since lnd serves REST over TLS by default; supply an
	// explicit http:// scheme to talk to a gateway with REST TLS disabled.
	Host string

	// TLSPath is the path to lnd's TLS certificate. When set it anchors a
	// dedicated cert pool; when empty the system cert pool is used (which
	// only works if lnd's cert is publicly trusted).
	TLSPath string

	// MacaroonPath is the path to the macaroon authorizing the used RPCs.
	// It is required: unlike the gRPC lndclient, the REST path cannot
	// resolve lnd's per-network default macaroon location on its own.
	MacaroonPath string

	// RPCTimeout bounds each unary REST call. Zero selects
	// defaultRPCTimeout.
	RPCTimeout time.Duration

	// ChainParams identifies the active network and is used for address
	// decoding.
	ChainParams *chaincfg.Params

	// Network is the configured network in lnd's naming convention
	// ("mainnet", "testnet", "testnet4", "regtest", "simnet", "signet").
	// It is cross-checked against the network lnd reports from GetInfo.
	// This is deliberately not derived from ChainParams.Name: btcd's
	// chaincfg calls the testnet3 network "testnet3" whereas lnd reports it
	// as "testnet", so comparing against ChainParams.Name would spuriously
	// reject a correctly-configured testnet node. Empty disables the check.
	Network string
}

// conn is the shared REST transport used by every lndrest sub-client. It wraps
// the rpc/restclient transport with the per-call timeout policy and the active
// chain params needed for address decoding.
type conn struct {
	rc      *restclient.Client
	http    *http.Client
	timeout time.Duration
	params  *chaincfg.Params
}

// post issues one unary POST call, applying the per-call timeout.
func (c *conn) post(ctx context.Context, path string,
	in, out proto.Message) error {

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	return c.rc.Post(ctx, path, in, out)
}

// get issues one unary GET call, applying the per-call timeout. The path must
// already carry any query string the endpoint binds its request from.
func (c *conn) get(ctx context.Context, path string, out proto.Message) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	return c.rc.Get(ctx, path, out)
}

// stream opens one server-streaming call. It deliberately does not apply the
// per-call timeout: notification streams are long-lived and are torn down by
// the caller cancelling the supplied context instead.
func (c *conn) stream(ctx context.Context, path string, in proto.Message) (
	*http.Response, error) {

	return c.rc.Stream(ctx, path, in)
}

// Backend is a REST-backed lnd connection. It satisfies the daemon's lnd
// services seam by exposing a populated *lndclient.LndServices whose subsystem
// clients are the REST implementations in this package.
type Backend struct {
	services *lndclient.LndServices
	conn     *conn
}

// Services returns the populated lndclient service payload backed by REST.
func (b *Backend) Services() *lndclient.LndServices {
	return b.services
}

// Close releases the idle HTTP connections held by the REST transport. Active
// notification streams are owned by their callers and are torn down when those
// callers cancel the context they registered with.
func (b *Backend) Close() {
	b.conn.http.CloseIdleConnections()
}

// New dials lnd over REST, verifies connectivity via a GetInfo round-trip, and
// returns a Backend whose lndclient service fields are REST implementations.
// The GetInfo response seeds NodeAlias/NodePubkey (and validates the reported
// network) exactly as the gRPC lndclient does at connect time.
func New(ctx context.Context, cfg Config) (*Backend, error) {
	if cfg.ChainParams == nil {
		return nil, fmt.Errorf("lndrest: chain params are required")
	}
	if cfg.MacaroonPath == "" {
		return nil, fmt.Errorf("lndrest: lnd.macaroonpath is " +
			"required for the REST transport")
	}

	macHex, err := readMacaroonHex(cfg.MacaroonPath)
	if err != nil {
		return nil, err
	}

	httpClient, err := newHTTPClient(cfg.TLSPath)
	if err != nil {
		return nil, err
	}

	timeout := cfg.RPCTimeout
	if timeout == 0 {
		timeout = defaultRPCTimeout
	}

	rc := restclient.New(
		baseURL(cfg.Host), restclient.WithHTTPClient(httpClient),
		restclient.WithHeader(macaroonHeader, macHex),
	)
	c := &conn{
		rc:      rc,
		http:    httpClient,
		timeout: timeout,
		params:  cfg.ChainParams,
	}

	// Populate the service payload with the REST implementations. The
	// wallet backend reads these interface fields; the concrete types live
	// in the sibling files in this package.
	services := &lndclient.LndServices{
		Signer: &signerClient{
			conn: c,
		},
		WalletKit: &walletKitClient{
			conn: c,
		},
		ChainKit: &chainKitClient{
			conn: c,
		},
		ChainNotifier: &chainNotifierClient{
			conn: c,
		},
		Client: &lightningClient{
			conn: c,
		},
		ChainParams: cfg.ChainParams,
	}

	// A GetInfo round-trip both proves the connection works and seeds the
	// node metadata the daemon reads off the services payload.
	info, err := services.Client.GetInfo(ctx)
	if err != nil {
		httpClient.CloseIdleConnections()

		return nil, fmt.Errorf("lndrest: initial GetInfo failed: %w",
			err)
	}

	if err := networkMismatch(info.Network, cfg.Network); err != nil {
		httpClient.CloseIdleConnections()

		return nil, err
	}

	services.NodeAlias = info.Alias
	services.NodePubkey = route.Vertex(info.IdentityPubkey)

	return &Backend{
		services: services,
		conn:     c,
	}, nil
}

// networkMismatch reports a non-nil error when the network lnd reports from
// GetInfo does not match the configured network. Both values use lnd's naming
// convention ("mainnet", "testnet", "testnet4", "regtest", "simnet",
// "signet"). Comparing in this convention (rather than against
// chaincfg.Params.Name) is what makes testnet work: lnd reports the testnet3
// network as "testnet", while chaincfg names it "testnet3". An empty value on
// either side disables the check (lnd omitted the chain, or no network was
// configured).
func networkMismatch(lndNetwork, configured string) error {
	if lndNetwork == "" || configured == "" {
		return nil
	}

	if lndNetwork != configured {
		return fmt.Errorf("lndrest: lnd network %q does not match "+
			"configured network %q", lndNetwork, configured)
	}

	return nil
}

// baseURL returns the REST base URL for the configured lnd host. lnd serves
// REST over TLS by default, so a schemeless host is treated as https rather
// than relying on the restclient loopback heuristic (which would pick http for
// a localhost lnd and fail the TLS handshake). An explicit scheme is honored so
// operators running lnd with REST TLS disabled can still opt into http.
func baseURL(host string) string {
	if strings.HasPrefix(host, "http://") ||
		strings.HasPrefix(host, "https://") {
		return host
	}

	return "https://" + host
}

// readMacaroonHex reads the macaroon file and returns its hex encoding for the
// Grpc-Metadata-macaroon header.
func readMacaroonHex(path string) (string, error) {
	//nolint:gosec // The macaroon path is operator-supplied configuration.
	macBytes, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("lndrest: read macaroon %q: %w", path,
			err)
	}

	return hex.EncodeToString(macBytes), nil
}

// newHTTPClient builds the HTTP client used for every REST call. When a TLS
// certificate path is supplied it anchors a dedicated cert pool so lnd's
// self-signed certificate is trusted; otherwise the system cert pool is used.
func newHTTPClient(tlsPath string) (*http.Client, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	if tlsPath != "" {
		//nolint:gosec // The TLS cert path is operator-supplied config.
		certBytes, err := os.ReadFile(tlsPath)
		if err != nil {
			return nil, fmt.Errorf("lndrest: read lnd TLS cert "+
				"%q: %w", tlsPath, err)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(certBytes) {
			return nil, fmt.Errorf("lndrest: parse lnd TLS cert %q",
				tlsPath)
		}
		tlsCfg.RootCAs = pool
	}

	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}, nil
}
