package darepoclient

import (
	"path/filepath"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/jessevdk/go-flags"
	"github.com/lightninglabs/darepo-client/db"
)

var (
	// defaultDataDir is the default directory where arkd tries to find its
	// configuration file and store its data. This is a directory in the
	// user's application data, for example:
	//   C:\Users\<username>\AppData\Local\Arkd on Windows
	//   ~/.arkd on Linux
	//   ~/Library/Application Support/Arkd on MacOS
	defaultDataDir = btcutil.AppDataDir("arkd", false)

	// defaultLogDir is the default directory where arkd will store its log
	// file.
	defaultLogDir = filepath.Join(defaultDataDir, defaultLogDirname)

	defaultNetwork = "regtest"
)

const (
	defaultLogLevel    = "info"
	defaultLogDirname  = "logs"
	defaultLogFilename = "arkd.log"
)

// Config is the main configuration struct for the operator server.
//
//nolint:ll
type Config struct {
	// DB contains the database configuration (sqlite or postgres).
	DB *db.Config `group:"db" namespace:"db"`

	// AdminRPC contains the admin RPC server configuration.
	AdminRPC *AdminRPCConfig `group:"admin-rpc" namespace:"admin-rpc"`

	// RPC contains the client-facing RPC server configuration.
	RPC *RPCConfig `group:"rpc" namespace:"rpc"`

	DataDir string `long:"datadir" description:"The base directory that contains arkd's data, logs, configuration file, etc. This option overwrites all other directory options."`

	Network string `long:"network" description:"Network to run on" choice:"mainnet" choice:"regtest" choice:"testnet" choice:"signet"`

	// Logging contains the logging configuration.
	LogLevel string `long:"loglevel" description:"Logging level for all subsystems {trace, debug, info, warn, error, critical}"`

	LogFilePath string `long:"logfile" description:"Path to write the log file"`

	Shutdown func()
}

func DefaultConfig() Config {
	dataDir := defaultDataDir

	return Config{
		DB:          db.DefaultConfig(dataDir),
		AdminRPC:    DefaultAdminRPCConfig(),
		RPC:         DefaultRPCConfig(),
		DataDir:     dataDir,
		Network:     defaultNetwork,
		LogLevel:    defaultLogLevel,
		LogFilePath: defaultLogDir,
		Shutdown:    func() {},
	}
}

func LoadConfig() (*Config, error) {
	// Pre-parse the command line options to pick up an alternative config
	// file.
	cfg := DefaultConfig()
	if _, err := flags.Parse(&cfg); err != nil {
		return nil, err
	}

	// Finally, parse the remaining command line options again to ensure
	// they take precedence.
	flagParser := flags.NewParser(&cfg, flags.Default)
	if _, err := flagParser.Parse(); err != nil {
		return nil, err
	}

	// Make sure everything we just loaded makes sense.
	return ValidateConfig(cfg)
}

func ValidateConfig(cfg Config) (*Config, error) {
	// TODO: Add validation logic here

	return &cfg, nil
}
