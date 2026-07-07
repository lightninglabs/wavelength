package chainfees

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	defaultMempoolSpaceTimeout  = 5 * time.Second
	defaultMempoolSpaceCacheTTL = 30 * time.Second
	maxMempoolSpaceSatPerVByte  = 1_000_000
	satPerVByteToSatPerKWeight  = 250

	// maxMempoolSpaceResponseBytes bounds the recommended-fee response body
	// we are willing to read. A valid response is a small JSON object with
	// five integer fields (well under 1 KiB); the generous 64 KiB cap
	// guards against a malicious or misbehaving endpoint streaming an
	// unbounded body to exhaust memory, since http.Client.Timeout bounds
	// wall-clock time but not the number of bytes read.
	maxMempoolSpaceResponseBytes = 64 << 10

	mempoolSpaceMainnetURL = "https://mempool.space/api/v1/fees/recommended"
	mempoolSpaceTestnetURL = "https://mempool.space/testnet/api/v1/fees/" +
		"recommended"
	mempoolSpaceTestnet4URL = "https://mempool.space/testnet4/api/v1/fees" +
		"/recommended"
	mempoolSpaceSignetURL = "https://mempool.space/signet/api/v1/fees/" +
		"recommended"
)

// MempoolSpaceConfig configures a MempoolSpaceEstimator.
type MempoolSpaceConfig struct {
	// URL optionally overrides the network-default mempool.space endpoint.
	URL string

	// Params selects the default endpoint when URL is empty.
	Params *chaincfg.Params

	// Log is an optional structured logger.
	Log btclog.Logger

	// Timeout bounds each HTTP request.
	Timeout time.Duration

	// CacheTTL controls how long a successful recommended-fee response is
	// reused before the estimator queries mempool.space again.
	CacheTTL time.Duration
}

// MempoolSpaceEstimator estimates fees from mempool.space's recommended-fee
// endpoint.
type MempoolSpaceEstimator struct {
	url    string
	client *http.Client
	log    btclog.Logger

	mu           sync.Mutex
	cacheTTL     time.Duration
	cacheExpires time.Time
	cachedFees   mempoolSpaceRecommendedFees
	now          func() time.Time
}

type mempoolSpaceRecommendedFees struct {
	FastestFee  int64 `json:"fastestFee"`  //nolint:tagliatelle
	HalfHourFee int64 `json:"halfHourFee"` //nolint:tagliatelle
	HourFee     int64 `json:"hourFee"`     //nolint:tagliatelle
	EconomyFee  int64 `json:"economyFee"`  //nolint:tagliatelle
	MinimumFee  int64 `json:"minimumFee"`  //nolint:tagliatelle
}

// NewMempoolSpaceEstimator creates a fee estimator backed by mempool.space.
func NewMempoolSpaceEstimator(cfg MempoolSpaceConfig) (*MempoolSpaceEstimator,
	error) {

	endpoint := cfg.URL
	if endpoint == "" {
		var err error
		endpoint, err = DefaultMempoolSpaceURL(cfg.Params)
		if err != nil {
			return nil, err
		}
	}

	// Reject a non-TLS override before it can leak fee data to a network
	// attacker who could tamper with the response (plaintext http is
	// allowed only for loopback, which the local httptest server relies
	// on).
	if err := validateMempoolSpaceURL(endpoint); err != nil {
		return nil, err
	}

	log := cfg.Log
	if log == nil {
		log = btclog.Disabled
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultMempoolSpaceTimeout
	}

	cacheTTL := cfg.CacheTTL
	if cacheTTL <= 0 {
		cacheTTL = defaultMempoolSpaceCacheTTL
	}

	return &MempoolSpaceEstimator{
		url: endpoint,
		client: &http.Client{
			Timeout: timeout,
		},
		log:      log,
		cacheTTL: cacheTTL,
		now:      time.Now,
	}, nil
}

// DefaultMempoolSpaceURL returns the recommended-fee endpoint for params.
func DefaultMempoolSpaceURL(params *chaincfg.Params) (string, error) {
	if params == nil {
		return "", fmt.Errorf("chain params are required")
	}

	switch {
	case params.Net == wire.MainNet:
		return mempoolSpaceMainnetURL, nil

	case params.Net == wire.TestNet3:
		return mempoolSpaceTestnetURL, nil

	case params.Net == wire.TestNet4:
		return mempoolSpaceTestnet4URL, nil

	case params.Name == chaincfg.SigNetParams.Name:
		return mempoolSpaceSignetURL, nil

	default:
		return "", fmt.Errorf("unsupported mempool.space network %q",
			params.Name)
	}
}

// validateMempoolSpaceURL rejects an endpoint that would expose fee data to
// tampering. It requires an absolute https URL, permitting plaintext http only
// for a loopback host (used by the local test server).
func validateMempoolSpaceURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid mempool.space URL %q: %w", raw, err)
	}

	if parsed.Host == "" {
		return fmt.Errorf("mempool.space URL %q must be absolute "+
			"with a host", raw)
	}

	switch parsed.Scheme {
	case "https":
		return nil

	case "http":
		if isLoopbackHost(parsed.Hostname()) {
			return nil
		}

		return fmt.Errorf("mempool.space URL %q must use https; "+
			"plaintext http is only allowed for loopback", raw)

	default:
		return fmt.Errorf("mempool.space URL %q has unsupported "+
			"scheme %q", raw, parsed.Scheme)
	}
}

// isLoopbackHost reports whether host is "localhost" or a loopback IP literal.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}

	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}

	return false
}

// EstimateFeePerKW returns a mempool.space recommended fee for confTarget.
func (e *MempoolSpaceEstimator) EstimateFeePerKW(confTarget uint32) (
	chainfee.SatPerKWeight, error) {

	fees, err := e.recommendedFees()
	if err != nil {
		return 0, err
	}

	satPerVByte := fees.forTarget(confTarget)
	if satPerVByte <= 0 {
		return 0, fmt.Errorf("mempool.space returned non-positive fee "+
			"rate %d sat/vB for target %d", satPerVByte, confTarget)
	}
	if satPerVByte > maxMempoolSpaceSatPerVByte {
		return 0, fmt.Errorf("mempool.space returned excessive fee "+
			"rate %d sat/vB for target %d", satPerVByte, confTarget)
	}

	// 1 vbyte is 4 weight units, so 1 sat/vB is exactly 250 sat/kW.
	rate := chainfee.SatPerKWeight(satPerVByte * satPerVByteToSatPerKWeight)
	if rate < chainfee.FeePerKwFloor {
		rate = chainfee.FeePerKwFloor
	}

	e.log.DebugS(context.Background(), "mempool.space fee estimate",
		slog.Uint64("conf_target", uint64(confTarget)),
		slog.Int64("rate_sat_kw", int64(rate)),
		slog.Int64("rate_sat_vbyte", int64(rate.FeePerVByte())),
	)

	return rate, nil
}

// Start is a no-op because MempoolSpaceEstimator fetches on demand.
func (e *MempoolSpaceEstimator) Start() error { return nil }

// Stop is a no-op because MempoolSpaceEstimator fetches on demand.
func (e *MempoolSpaceEstimator) Stop() error { return nil }

// RelayFeePerKW returns the floor relay fee.
func (e *MempoolSpaceEstimator) RelayFeePerKW() chainfee.SatPerKWeight {
	return chainfee.FeePerKwFloor
}

func (e *MempoolSpaceEstimator) fetchRecommendedFees() (
	mempoolSpaceRecommendedFees, error) {

	resp, err := e.client.Get(e.url)
	if err != nil {
		return mempoolSpaceRecommendedFees{}, fmt.Errorf("query "+
			"mempool.space fees: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mempoolSpaceRecommendedFees{}, fmt.Errorf("query "+
			"mempool.space fees: status %s", resp.Status)
	}

	// Bound the body we read so a malicious or misbehaving endpoint cannot
	// exhaust memory with an unbounded response.
	body := io.LimitReader(resp.Body, maxMempoolSpaceResponseBytes)

	var fees mempoolSpaceRecommendedFees
	if err := json.NewDecoder(body).Decode(&fees); err != nil {
		return mempoolSpaceRecommendedFees{}, fmt.Errorf("decode "+
			"mempool.space fees: %w", err)
	}

	return fees, nil
}

func (e *MempoolSpaceEstimator) recommendedFees() (mempoolSpaceRecommendedFees,
	error) {

	now := e.now()
	e.mu.Lock()
	if now.Before(e.cacheExpires) {
		fees := e.cachedFees
		e.mu.Unlock()

		return fees, nil
	}
	e.mu.Unlock()

	fees, err := e.fetchRecommendedFees()
	if err != nil {
		return mempoolSpaceRecommendedFees{}, err
	}

	e.mu.Lock()
	e.cachedFees = fees
	e.cacheExpires = e.now().Add(e.cacheTTL)
	e.mu.Unlock()

	return fees, nil
}

func (f mempoolSpaceRecommendedFees) forTarget(target uint32) int64 {
	switch {
	case target <= 1:
		return f.FastestFee

	case target <= 3:
		return f.HalfHourFee

	case target <= 6:
		return f.HourFee

	case target <= 144:
		return f.EconomyFee

	default:
		return f.MinimumFee
	}
}

var _ chainfee.Estimator = (*MempoolSpaceEstimator)(nil)
