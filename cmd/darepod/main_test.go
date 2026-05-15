package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// TestReadConfigFileLoadsProperties checks that an explicit config file is
// merged into Viper and unmarshaled into the daemon config.
func TestReadConfigFileLoadsProperties(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "darepod.conf")
	err := os.WriteFile(
		configPath, []byte(
			"network=regtest # local "+
				"development\nwallet.type=lnd\n",
		),
		0o600,
	)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	v, cmd := newTestConfigCommand(t, "")
	if err := cmd.Flags().Set("configfile", configPath); err != nil {
		t.Fatalf("set configfile flag: %v", err)
	}

	if err := readConfigFile(v, cmd); err != nil {
		t.Fatalf("read config file: %v", err)
	}

	if got := v.GetString("network"); got != "regtest" {
		t.Fatalf("expected network regtest, got %q", got)
	}
	if got := v.GetString("wallet.type"); got != "lnd" {
		t.Fatalf("expected wallet type lnd, got %q", got)
	}

	cfg := darepod.DefaultConfig()
	if err := v.Unmarshal(cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if cfg.Network != "regtest" {
		t.Fatalf("expected config network regtest, got %q", cfg.Network)
	}
	if cfg.Wallet.Type != "lnd" {
		t.Fatalf("expected config wallet type lnd, got %q",
			cfg.Wallet.Type)
	}
}

// TestReadConfigFileIgnoresMissingDefault checks that a missing default config
// file does not block first startup.
func TestReadConfigFileIgnoresMissingDefault(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "missing.conf")
	v, cmd := newTestConfigCommand(t, configPath)

	if err := readConfigFile(v, cmd); err != nil {
		t.Fatalf("missing default config file should be ignored: %v",
			err)
	}
}

// TestReadConfigFileRejectsMissingExplicitPath checks that a missing path
// passed via --configfile is reported as an error.
func TestReadConfigFileRejectsMissingExplicitPath(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "missing.conf")
	v, cmd := newTestConfigCommand(t, "")
	if err := cmd.Flags().Set("configfile", configPath); err != nil {
		t.Fatalf("set configfile flag: %v", err)
	}

	if err := readConfigFile(v, cmd); err == nil {
		t.Fatalf("expected missing explicit config file to fail")
	}
}

// TestReadConfigFileRejectsMissingEnvPath checks that a missing path passed via
// DAREPOD_CONFIGFILE is reported as an error.
func TestReadConfigFileRejectsMissingEnvPath(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "missing.conf")
	t.Setenv("DAREPOD_CONFIGFILE", configPath)

	v, cmd := newTestConfigCommand(t, "")
	v.SetEnvPrefix("DAREPOD")
	v.AutomaticEnv()

	if err := readConfigFile(v, cmd); err == nil {
		t.Fatalf("expected missing env config file to fail")
	}
}

// newTestConfigCommand returns a minimal command and Viper instance wired with
// the configfile flag.
func newTestConfigCommand(t *testing.T,
	defaultConfigPath string) (*viper.Viper, *cobra.Command) {

	t.Helper()

	cmd := &cobra.Command{Use: "darepod"}
	cmd.Flags().String("configfile", defaultConfigPath, "")

	v := viper.New()
	if err := v.BindPFlag(
		"configfile", cmd.Flags().Lookup("configfile"),
	); err != nil {

		t.Fatalf("bind configfile flag: %v", err)
	}

	return v, cmd
}
