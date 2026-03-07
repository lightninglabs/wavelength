package darepod

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lightninglabs/darepo-client/rpc/roundpb"
)

const (
	// DefaultDataDir is the default root data directory for darepod. It
	// lives under the user's home directory.
	DefaultDataDir = "~/.darepod"

	// DefaultNetwork is the default bitcoin network the daemon operates
	// on.
	DefaultNetwork = "mainnet"

	// DefaultRPCHost is the default listen address for the daemon's own
	// gRPC server.
	DefaultRPCHost = "localhost:10029"

	// DefaultLndHost is the default address for connecting to the local
	// lnd instance.
	DefaultLndHost = "localhost:10009"

	// DefaultServerHost is the default address for the ark operator's
	// mailbox edge server.
	DefaultServerHost = "localhost:10010"

	// DefaultRPCTimeout is the default timeout for RPC calls to lnd.
	DefaultRPCTimeout = 30 * time.Second

	// DefaultDebugLevel is the default logging verbosity.
	DefaultDebugLevel = "info"

	// DefaultShutdownTimeout is the maximum duration to wait for
	// graceful shutdown of the actor system and subsystems.
	DefaultShutdownTimeout = 10 * time.Second

	// DefaultForfeitCollectionTimeout is the default wall-clock
	// deadline for collecting forfeit signatures from VTXO actors
	// during a round.
	DefaultForfeitCollectionTimeout = 2 * time.Minute
)

// Config holds all configuration for the darepod daemon.
type Config struct {
	// DataDir is the root data directory for all daemon state. Database
	// files, logs, and TLS material are stored under this directory.
	DataDir string `mapstructure:"datadir"`

	// Network selects the bitcoin network: mainnet, testnet, regtest, or
	// simnet.
	Network string `mapstructure:"network"`

	// DebugLevel controls the verbosity of daemon logging. A single value
	// sets the global level for all subsystems (e.g. "info"). A
	// comma-separated list of subsystem=level pairs sets per-subsystem
	// levels (e.g. "ROND=debug,OORC=trace,info"). The last bare level in
	// the list (without a '=') sets the default for unlisted subsystems.
	// Valid levels: trace, debug, info, warn, error, critical, off.
	DebugLevel string `mapstructure:"debuglevel"`

	// Lnd configures the connection to the backing lnd node.
	Lnd *LndConfig `mapstructure:"lnd"`

	// Server configures the connection to the ark operator's mailbox
	// edge server.
	Server *ServerConfig `mapstructure:"server"`

	// RPC configures the daemon's own gRPC server that external tools
	// (CLI, GUI) connect to.
	RPC *RPCConfig `mapstructure:"rpc"`

	// ForfeitCollectionTimeout is the maximum wall-clock
	// duration to wait for forfeit signatures during a round.
	// If zero, the default of 2 minutes is used.
	//nolint:ll
	ForfeitCollectionTimeout time.Duration `mapstructure:"forfeitcollectiontimeout"`
}

// LndConfig holds connection parameters for the backing lnd node.
type LndConfig struct {
	// Host is the network address of the lnd gRPC interface.
	Host string `mapstructure:"host"`

	// TLSPath is the path to lnd's TLS certificate file. If empty, the
	// default lnd TLS cert location is used.
	TLSPath string `mapstructure:"tlspath"`

	// MacaroonPath is the full path to the lnd admin macaroon. If empty,
	// the default lnd macaroon location for the active network is used.
	MacaroonPath string `mapstructure:"macaroonpath"`

	// RPCTimeout is the maximum duration for individual RPC calls to
	// lnd. If zero, DefaultRPCTimeout is used.
	RPCTimeout time.Duration `mapstructure:"rpctimeout"`
}

// ServerConfig holds connection parameters for the ark operator's mailbox
// edge server.
type ServerConfig struct {
	// Host is the gRPC address of the ark operator's mailbox edge
	// service.
	Host string `mapstructure:"host"`

	// TLSCertPath is the path to the operator's TLS certificate for
	// verifying the server connection. If empty, the system cert pool
	// is used.
	TLSCertPath string `mapstructure:"tlscertpath"`

	// Insecure disables TLS for the server connection. This should only
	// be used in regtest or development environments.
	Insecure bool `mapstructure:"insecure"`

	// MaxTreeNodes caps the number of nodes accepted in a VTXO tree
	// received from the server. This prevents memory exhaustion from
	// oversized tree payloads. If zero, the default of
	// roundpb.DefaultMaxTreeNodes (50,000) is used.
	MaxTreeNodes int `mapstructure:"maxtreenodes"`

	// LocalMailboxID is this client's mailbox identifier within the
	// mailbox edge transport. Inbound envelopes are pulled from this
	// mailbox and outbound envelopes carry it as the sender.
	LocalMailboxID string `mapstructure:"localmailboxid"`

	// RemoteMailboxID is the remote server's mailbox identifier.
	// Outbound envelopes are addressed to this mailbox.
	RemoteMailboxID string `mapstructure:"remotemailboxid"`
}

// RPCConfig holds configuration for the daemon's own gRPC server.
type RPCConfig struct {
	// ListenAddr is the network address the gRPC server binds to.
	ListenAddr string `mapstructure:"listenaddr"`

	// TLSCertPath is the path to the daemon's TLS certificate. If empty,
	// one is auto-generated in the data directory.
	TLSCertPath string `mapstructure:"tlscertpath"`

	// TLSKeyPath is the path to the daemon's TLS private key. If empty,
	// one is auto-generated in the data directory.
	TLSKeyPath string `mapstructure:"tlskeypath"`
}

// DefaultConfig returns a Config populated with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		DataDir:    DefaultDataDir,
		Network:    DefaultNetwork,
		DebugLevel: DefaultDebugLevel,
		Lnd: &LndConfig{
			Host:       DefaultLndHost,
			RPCTimeout: DefaultRPCTimeout,
		},
		Server: &ServerConfig{
			Host:         DefaultServerHost,
			MaxTreeNodes: roundpb.DefaultMaxTreeNodes,
		},
		RPC: &RPCConfig{
			ListenAddr: DefaultRPCHost,
		},
	}
}

// Validate checks the config for internal consistency and returns an error
// if any required fields are missing or invalid.
func (c *Config) Validate() error {
	switch c.Network {
	case "mainnet", "testnet", "regtest", "simnet", "signet":
	default:
		return fmt.Errorf("unknown network %q", c.Network)
	}

	if c.Lnd == nil {
		return fmt.Errorf("lnd config is required")
	}
	if c.Lnd.Host == "" {
		return fmt.Errorf("lnd host is required")
	}

	if c.Server == nil {
		return fmt.Errorf("server config is required")
	}
	if c.Server.Host == "" {
		return fmt.Errorf("server host is required")
	}
	if c.Server.LocalMailboxID == "" {
		return fmt.Errorf("server local mailbox ID is " +
			"required")
	}
	if c.Server.RemoteMailboxID == "" {
		return fmt.Errorf("server remote mailbox ID is " +
			"required")
	}

	if c.RPC == nil {
		return fmt.Errorf("rpc config is required")
	}
	if c.RPC.ListenAddr == "" {
		return fmt.Errorf("rpc listen address is required")
	}

	return nil
}

// NetworkDir returns the network-scoped data directory (e.g.,
// ~/.darepod/data/regtest).
func (c *Config) NetworkDir() string {
	return filepath.Join(expandTilde(c.DataDir), "data", c.Network)
}

// LogDir returns the network-scoped log directory.
func (c *Config) LogDir() string {
	return filepath.Join(expandTilde(c.DataDir), "logs", c.Network)
}

// expandTilde replaces a leading ~ or ~/ with the user's home
// directory. For example, "~/.darepod" becomes "/home/user/.darepod".
func expandTilde(path string) string {
	if len(path) == 0 || path[0] != '~' {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}

	// Strip the leading "~" and any path separator that follows
	// it, so filepath.Join receives a relative suffix. Without
	// this, "~/.darepod" would produce path[1:] == "/.darepod"
	// which is absolute and causes Join to discard home.
	suffix := path[1:]
	if len(suffix) > 0 && os.IsPathSeparator(suffix[0]) {
		suffix = suffix[1:]
	}

	return filepath.Join(home, suffix)
}
