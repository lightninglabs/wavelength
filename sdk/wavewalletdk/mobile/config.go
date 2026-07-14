//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
)

// mobileConfig is the flat, JSON-serializable subset of wavewalletdk.Config
// that a mobile host can express. It deliberately omits the reference-typed
// fields of wavewalletdk.Config (DaemonConfig *waved.Config and LogWriter
// io.Writer) that cannot cross a JSON / gomobile boundary; hosts that need
// fine-grained daemon knobs should use the Go SDK directly. Durations are
// seconds and amounts are int64 to stay JSON-host friendly (no uint, no
// time.Duration).
type mobileConfig struct {
	DataDir    string `json:"data_dir"`
	Network    string `json:"network"`
	DebugLevel string `json:"debug_level"`

	// AllowMainnet must be true to run on mainnet.
	AllowMainnet bool `json:"allow_mainnet"`

	// Ark operator / mailbox edge server.
	ServerAddress     string `json:"server_address"`
	ServerTransport   string `json:"server_transport"`
	ServerTLSCertPath string `json:"server_tls_cert_path"`
	ServerInsecure    bool   `json:"server_insecure"`

	// Backing wallet.
	WalletType                string `json:"wallet_type"`
	WalletEsploraURL          string `json:"wallet_esplora_url"`
	WalletPasswordFile        string `json:"wallet_password_file"`
	WalletPollIntervalSeconds int64  `json:"wallet_poll_interval_seconds"`
	WalletRecoveryWindow      int64  `json:"wallet_recovery_window"`
	WalletFeeURL              string `json:"wallet_fee_url"`
	WalletBlockHeadersSource  string `json:"wallet_block_headers_source"`
	WalletFilterHeadersSource string `json:"wallet_filter_headers_source"`

	// Swap server.
	SwapServerAddress     string `json:"swap_server_address"`
	SwapServerTransport   string `json:"swap_server_transport"`
	SwapServerTLSCertPath string `json:"swap_server_tls_cert_path"`
	SwapServerInsecure    bool   `json:"swap_server_insecure"`
	SwapDatabaseFileName  string `json:"swap_database_file_name"`

	MaxOperatorFeeSat int64 `json:"max_operator_fee_sat"`
	SigningWorkers    int64 `json:"signing_workers"`
	EagerRoundJoin    bool  `json:"eager_round_join"`
	BufferSize        int   `json:"buffer_size"`
}

// parseConfig decodes the host JSON config into a wavewalletdk.Config. An empty
// string yields wavewalletdk.DefaultConfig so a host can boot with all
// defaults.
func parseConfig(cfgJSON string) (wavewalletdk.Config, error) {
	cfg := wavewalletdk.DefaultConfig()
	if cfgJSON == "" {
		return cfg, nil
	}

	var mc mobileConfig
	if err := json.Unmarshal([]byte(cfgJSON), &mc); err != nil {
		return wavewalletdk.Config{}, fmt.Errorf("decode mobile "+
			"config: %w", err)
	}

	if err := mc.validate(); err != nil {
		return wavewalletdk.Config{}, err
	}

	applyMobileConfig(&cfg, mc)

	return cfg, nil
}

// validate rejects malformed scalar config before it reaches the daemon. The
// gomobile boundary uses signed integers, so a host can pass a negative where
// the daemon expects a non-negative count or duration. A negative
// wallet_poll_interval_seconds in particular becomes a negative time.Duration
// that panics the lwwallet tip poller's time.NewTicker in a background
// goroutine after startup; catching it here turns malformed JSON into a clean
// startup error instead of crashing the host process. The same guard protects
// the unchecked int64->uint32 conversion of the recovery window.
func (mc mobileConfig) validate() error {
	nonNegative := []struct {
		name string
		v    int64
	}{
		{
			"wallet_poll_interval_seconds",
			mc.WalletPollIntervalSeconds,
		},
		{
			"wallet_recovery_window",
			mc.WalletRecoveryWindow,
		},
		{
			"max_operator_fee_sat",
			mc.MaxOperatorFeeSat,
		},
		{
			"signing_workers",
			mc.SigningWorkers,
		},
	}
	for _, f := range nonNegative {
		if f.v < 0 {
			return fmt.Errorf("%s must not be negative: %d", f.name,
				f.v)
		}
	}
	if mc.BufferSize < 0 {
		return fmt.Errorf("buffer_size must not be negative: %d",
			mc.BufferSize)
	}

	// The recovery window is narrowed to uint32 in applyMobileConfig, so an
	// absurdly large positive value would silently wrap. Reject it here to
	// keep the conversion total.
	if mc.WalletRecoveryWindow > math.MaxUint32 {
		return fmt.Errorf("wallet_recovery_window exceeds "+
			"uint32 max: %d", mc.WalletRecoveryWindow)
	}
	if mc.SigningWorkers > int64(wavewalletdk.MaxSigningWorkers) {
		return fmt.Errorf("signing_workers exceeds maximum %d: %d",
			wavewalletdk.MaxSigningWorkers, mc.SigningWorkers)
	}

	return nil
}

// applyMobileConfig overlays only the fields the host actually set onto the
// default config, mirroring wavewalletdk's own enable-only / non-empty
// convenience merge semantics so the zero value defers to the wavewalletrpc
// build defaults.
func applyMobileConfig(cfg *wavewalletdk.Config, mc mobileConfig) {
	if mc.DataDir != "" {
		cfg.DataDir = mc.DataDir
	}
	if mc.Network != "" {
		cfg.Network = mc.Network
	}
	if mc.DebugLevel != "" {
		cfg.DebugLevel = mc.DebugLevel
	}
	if mc.AllowMainnet {
		cfg.AllowMainnet = true
	}

	if mc.ServerAddress != "" {
		cfg.ServerAddress = mc.ServerAddress
	}
	if mc.ServerTransport != "" {
		cfg.ServerTransport = wavewalletdk.Transport(mc.ServerTransport)
	}
	if mc.ServerTLSCertPath != "" {
		cfg.ServerTLSCertPath = mc.ServerTLSCertPath
	}
	if mc.ServerInsecure {
		cfg.ServerInsecure = true
	}

	if mc.WalletType != "" {
		cfg.WalletType = mc.WalletType
	}
	if mc.WalletEsploraURL != "" {
		cfg.WalletEsploraURL = mc.WalletEsploraURL
	}
	if mc.WalletPasswordFile != "" {
		cfg.WalletPasswordFile = mc.WalletPasswordFile
	}
	if mc.WalletPollIntervalSeconds != 0 {
		cfg.WalletPollInterval = time.Duration(
			mc.WalletPollIntervalSeconds,
		) * time.Second
	}
	if mc.WalletRecoveryWindow != 0 {
		cfg.WalletRecoveryWindow = uint32(mc.WalletRecoveryWindow)
	}
	if mc.WalletFeeURL != "" {
		cfg.WalletFeeURL = mc.WalletFeeURL
	}
	if mc.WalletBlockHeadersSource != "" {
		cfg.WalletBtcwalletBlockHeadersSource = mc.WalletBlockHeadersSource
	}
	if mc.WalletFilterHeadersSource != "" {
		cfg.WalletBtcwalletFilterHeadersSource =
			mc.WalletFilterHeadersSource
	}

	if mc.SwapServerAddress != "" {
		cfg.SwapServerAddress = mc.SwapServerAddress
	}
	if mc.SwapServerTransport != "" {
		cfg.SwapServerTransport = wavewalletdk.Transport(
			mc.SwapServerTransport,
		)
	}
	if mc.SwapServerTLSCertPath != "" {
		cfg.SwapServerTLSCertPath = mc.SwapServerTLSCertPath
	}
	if mc.SwapServerInsecure {
		cfg.SwapServerInsecure = true
	}
	if mc.SwapDatabaseFileName != "" {
		cfg.SwapDatabaseFileName = mc.SwapDatabaseFileName
	}

	if mc.MaxOperatorFeeSat != 0 {
		cfg.MaxOperatorFeeSat = mc.MaxOperatorFeeSat
	}
	if mc.SigningWorkers != 0 {
		cfg.SigningWorkers = int(mc.SigningWorkers)
	}
	if mc.EagerRoundJoin {
		cfg.EagerRoundJoin = true
	}
	if mc.BufferSize != 0 {
		cfg.BufferSize = mc.BufferSize
	}
}
