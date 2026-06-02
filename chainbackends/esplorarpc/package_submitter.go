// Package esplorarpc provides an Esplora-backed implementation of
// chainbackends.PackageSubmitter. It POSTs a v3/TRUC parent+child package to
// an Esplora/electrs `/txs/package` endpoint, letting chain backends that
// cannot do package relay themselves (lnd's WalletKit, neutrino SPV) still
// broadcast the zero-fee ephemeral-anchor parents that unilateral exit /
// fraud response produce.
//
// Sibling to chainbackends/bitcoindrpc: one concrete PackageSubmitter per
// relay source. Esplora differs from a direct bitcoind connection in two
// ways this implementation accounts for: the endpoint is a plain REST POST
// of a JSON array of raw tx hex (not a JSON-RPC envelope), and it returns
// bitcoind's bare submitpackage result object directly rather than wrapped
// in a `{result, error}` envelope.
//
// Trust note: routing a package through a third-party Esplora server reveals
// those transactions to that server. For an otherwise-trustless SPV wallet
// this is a privacy regression, so this submitter is intended to be opt-in.
package esplorarpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// submitTimeout bounds the package POST so a wedged or slow Esplora server
// cannot stall the caller for the full parent context timeout. It matches
// the bitcoindrpc submitter's backstop so both relay sources behave the
// same under a hung endpoint.
const submitTimeout = 30 * time.Second

// packagePath is the Esplora REST path for atomic v3 package relay.
const packagePath = "/txs/package"

// PackageSubmitter implements chainbackends.PackageSubmitter by POSTing a
// parent+child package to an Esplora/electrs `/txs/package` endpoint.
type PackageSubmitter struct {
	// url is the normalized Esplora REST root (no trailing slash); the
	// package path is appended per request.
	url string

	// client is a dedicated HTTP client so we never share state with
	// http.DefaultClient, and it carries a timeout as a backstop for
	// callers that pass a never-cancelled context.
	client *http.Client

	// log is optional; without it the submitter logs nothing.
	log fn.Option[btclog.Logger]
}

// Option configures a PackageSubmitter.
type Option func(*PackageSubmitter)

// WithHTTPClient overrides the default HTTP client. Intended for tests and
// for operators that need custom TLS, proxy, or timeout behaviour.
func WithHTTPClient(client *http.Client) Option {
	return func(s *PackageSubmitter) {
		s.client = client
	}
}

// WithLog attaches a logger used to emit the raw endpoint response at debug
// level, which is the only place an Esplora-side package reject reason is
// visible. Without it the submitter logs nothing.
func WithLog(log btclog.Logger) Option {
	return func(s *PackageSubmitter) {
		s.log = fn.Some(log)
	}
}

// New creates a submitter that POSTs packages to the Esplora REST API rooted
// at baseURL (e.g. "https://blockstream.info/api" or a bare host). A base URL
// without a scheme defaults to https:// since public Esplora servers speak
// TLS; a trailing slash is trimmed so the package path joins cleanly.
func New(baseURL string, opts ...Option) (*PackageSubmitter, error) {
	endpoint, err := normalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}

	s := &PackageSubmitter{
		url: endpoint,
		client: &http.Client{
			Timeout: submitTimeout,
		},
		log: fn.None[btclog.Logger](),
	}
	for _, opt := range opts {
		opt(s)
	}

	return s, nil
}

// normalizeBaseURL validates and canonicalizes the operator-supplied Esplora
// base URL into a concrete http:// or https:// root without a trailing slash.
func normalizeBaseURL(baseURL string) (string, error) {
	if strings.TrimSpace(baseURL) == "" {
		return "", fmt.Errorf("esplora base URL is required")
	}

	raw := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	endpoint, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse esplora base URL %q: %w", baseURL,
			err)
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return "", fmt.Errorf("unsupported esplora URL scheme %q",
			endpoint.Scheme)
	}
	if endpoint.Host == "" {
		return "", fmt.Errorf("esplora base URL %q has no host",
			baseURL)
	}

	return strings.TrimRight(endpoint.String(), "/"), nil
}

// SubmitPackage implements chainbackends.PackageSubmitter by POSTing the
// serialized parent(s) and child to the Esplora `/txs/package` endpoint.
// Transactions are ordered parents-first, child-last, matching the
// dependency order Esplora (and bitcoind's submitpackage) require.
//
// The maxFeeRate argument is accepted for interface compatibility but
// ignored: the Esplora `/txs/package` endpoint exposes no per-call
// maxfeerate override, so the server's own default governs. This mirrors the
// lwwallet ChainBackend Esplora path. If a CPFP child's high standalone
// feerate is ever rejected by a server enforcing the default 0.10 BTC/kvB
// cap, the endpoint would need a query-param escape hatch; bitcoind-direct
// callers sidestep this by passing maxfeerate=0 (see chainbackends/
// bitcoindrpc).
func (s *PackageSubmitter) SubmitPackage(ctx context.Context,
	parents []*wire.MsgTx, child *wire.MsgTx, _ *float64) (
	*btcjson.SubmitPackageResult, error) {

	if child == nil {
		return nil, fmt.Errorf("child transaction is required")
	}

	txHexes := make([]string, 0, len(parents)+1)
	for i, tx := range parents {
		raw, err := serializeTx(tx)
		if err != nil {
			return nil, fmt.Errorf("serialize parent %d: %w", i,
				err)
		}

		txHexes = append(txHexes, raw)
	}

	childHex, err := serializeTx(child)
	if err != nil {
		return nil, fmt.Errorf("serialize child: %w", err)
	}
	txHexes = append(txHexes, childHex)

	body, err := json.Marshal(txHexes)
	if err != nil {
		return nil, fmt.Errorf("marshal package txs: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, s.url+packagePath, bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("create http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// URL is the operator-configured Esplora endpoint, not caller-
	// controlled input.
	resp, err := s.client.Do(httpReq) //nolint:gosec // G704: trusted URL
	if err != nil {
		return nil, fmt.Errorf("submit package: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read package response: %w", err)
	}

	s.log.WhenSome(func(l btclog.Logger) {
		l.DebugS(ctx, "Esplora package response",
			slog.Int("status", resp.StatusCode),
			slog.String("body", string(respBody)),
		)
	})

	// Esplora returns bitcoind's bare submitpackage result object (no
	// JSON-RPC envelope). A response with a parseable body is the normal
	// path even when the body reports per-tx rejections: the caller's
	// backend interprets PackageMsg / TxResults (mirroring bitcoindrpc,
	// which likewise returns the result and lets the backend classify
	// it). Only fall back to an HTTP-level error when the body is missing
	// or unparseable, e.g. an HTML 4xx/5xx page from a proxy.
	var result btcjson.SubmitPackageResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("submit package HTTP %d: %s",
				resp.StatusCode, string(respBody))
		}

		return nil, fmt.Errorf("decode package response: %w (body: %s)",
			err, string(respBody))
	}

	return &result, nil
}

// serializeTx returns the hex-encoded wire serialization of tx.
func serializeTx(tx *wire.MsgTx) (string, error) {
	if tx == nil {
		return "", fmt.Errorf("transaction is nil")
	}

	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return "", err
	}

	return hex.EncodeToString(buf.Bytes()), nil
}
