//go:build js && wasm

// Package btcwbackend is unavailable in browser WASM builds.
package btcwbackend

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/darepo-client/walletcore"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// Subsystem defines the logging code for this subsystem.
	Subsystem = "BTCW"

	// DefaultFeeMinUpdateTimeout is the default minimum interval between
	// fee estimation API queries.
	DefaultFeeMinUpdateTimeout = 5 * time.Minute

	// DefaultFeeMaxUpdateTimeout is the default maximum interval between
	// fee estimation API queries.
	DefaultFeeMaxUpdateTimeout = 20 * time.Minute
)

// Config holds the configuration for the native neutrino-backed wallet.
type Config struct {
	walletcore.Config

	NeutrinoDataDir      string
	ConnectPeers         []string
	AddPeers             []string
	BlockHeadersSource   string
	FilterHeadersSource  string
	FeeURL               string
	FeeMinUpdateTimeout  time.Duration
	FeeMaxUpdateTimeout  time.Duration
	PackageSubmitter     chainbackends.PackageSubmitter
	PersistFilters       bool
	DisableGlobalLoggers bool
}

// WithLogger returns a new config with the given logger set.
func (c Config) WithLogger(log btclog.Logger) Config {
	c.Log = fn.Some(log)

	return c
}

// NeutrinoServiceOption configures a NeutrinoService.
type NeutrinoServiceOption func(*NeutrinoService)

// NeutrinoService is a browser stub for the native neutrino service.
type NeutrinoService struct{}

// WithoutGlobalDependencyLoggers disables native package-global loggers.
func WithoutGlobalDependencyLoggers() NeutrinoServiceOption {
	return func(*NeutrinoService) {}
}

// NewNeutrinoService reports that neutrino is unavailable in browser builds.
func NewNeutrinoService(string, *chaincfg.Params, []string, []string, bool,
	string, string, btclog.Logger,
	...NeutrinoServiceOption) (*NeutrinoService, error) {

	return nil, fmt.Errorf("btcwallet backend is not available in wasm")
}

// Start reports that neutrino is unavailable in browser builds.
func (n *NeutrinoService) Start(context.Context) error {
	return fmt.Errorf("btcwallet backend is not available in wasm")
}

// Stop is a no-op for the browser stub.
func (n *NeutrinoService) Stop() error {
	return nil
}

// Wallet is a browser stub for the native neutrino-backed wallet.
type Wallet struct {
	walletcore.Wallet
}

// ErrWalletNotFound mirrors the native sentinel for browser builds.
var ErrWalletNotFound = errors.New("no wallet database found")

// ErrWalletExists mirrors the native sentinel for browser builds.
var ErrWalletExists = errors.New("wallet database already exists")

// New reports that btcwallet is unavailable in browser builds.
func New(Config) (*Wallet, error) {
	return nil, fmt.Errorf("btcwallet backend is not available in wasm")
}

// WalletExists reports that btcwallet is unavailable in browser builds.
func WalletExists(Config) (bool, error) {
	return false, fmt.Errorf("btcwallet backend is not available in wasm")
}

// NewWithNeutrino reports that btcwallet is unavailable in browser builds.
func NewWithNeutrino(Config, *NeutrinoService) (*Wallet, error) {
	return nil, fmt.Errorf("btcwallet backend is not available in wasm")
}

// Start reports that btcwallet is unavailable in browser builds.
func (w *Wallet) Start() error {
	return fmt.Errorf("btcwallet backend is not available in wasm")
}

// Stop is a no-op for the browser stub.
func (w *Wallet) Stop() {}

// BoardingBackend reports that no btcwallet boarding backend exists in WASM.
func (w *Wallet) BoardingBackend() wallet.BoardingBackend {
	return nil
}

// ChainBackend reports that no btcwallet chain backend exists in WASM.
func (w *Wallet) ChainBackend() chainsource.ChainBackend {
	return nil
}

// KeyRing returns the embedded walletcore key ring, if one was set by tests.
func (w *Wallet) KeyRing() keychain.SecretKeyRing {
	return w.Wallet.KeyRing
}

// IsSynced reports that the unavailable browser stub is not synced.
func (w *Wallet) IsSynced() (bool, int64, error) {
	return false, 0, fmt.Errorf("btcwallet backend is not available in " +
		"wasm")
}
