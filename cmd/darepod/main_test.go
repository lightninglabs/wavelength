package main

import (
	"os"
	"path/filepath"
	"strings"
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

// TestResolveBitcoindAuth covers the three branches of cookie-vs-
// user/pass credential resolution: passthrough, cookie-file parsing,
// and the mutual-exclusion guard.
func TestResolveBitcoindAuth(t *testing.T) {
	t.Parallel()

	writeCookie := func(t *testing.T, contents string) string {
		t.Helper()

		dir := t.TempDir()
		path := filepath.Join(dir, "bitcoind.cookie")
		err := os.WriteFile(path, []byte(contents), 0o600)
		if err != nil {
			t.Fatalf("write cookie: %v", err)
		}

		return path
	}

	tests := []struct {
		name       string
		user       string
		pass       string
		cookie     func(t *testing.T) string
		wantUser   string
		wantPass   string
		wantErrSub string
	}{
		{
			name: "no inputs is passthrough",
		},
		{
			name:     "explicit user and password are passthrough",
			user:     "alice",
			pass:     "secret",
			wantUser: "alice",
			wantPass: "secret",
		},
		{
			name: "cookie only is parsed",
			cookie: func(t *testing.T) string {
				return writeCookie(t, "__cookie__:abc123")
			},
			wantUser: "__cookie__",
			wantPass: "abc123",
		},
		{
			name: "cookie tolerates surrounding whitespace",
			cookie: func(t *testing.T) string {
				return writeCookie(t, "  __cookie__:abc123\n")
			},
			wantUser: "__cookie__",
			wantPass: "abc123",
		},
		{
			name: "cookie preserves colons inside the password",
			cookie: func(t *testing.T) string {
				return writeCookie(t, "__cookie__:abc:def:ghi")
			},
			wantUser: "__cookie__",
			wantPass: "abc:def:ghi",
		},
		{
			name: "cookie plus user is rejected",
			user: "alice",
			cookie: func(t *testing.T) string {
				return writeCookie(t, "__cookie__:abc123")
			},
			wantErrSub: "mutually exclusive",
		},
		{
			name: "cookie plus pass is rejected",
			pass: "secret",
			cookie: func(t *testing.T) string {
				return writeCookie(t, "__cookie__:abc123")
			},
			wantErrSub: "mutually exclusive",
		},
		{
			name: "malformed cookie is reported",
			cookie: func(t *testing.T) string {
				return writeCookie(t, "no-colon-here")
			},
			wantErrSub: "unexpected format",
		},
		{
			name: "missing cookie file is reported",
			cookie: func(_ *testing.T) string {
				return "/nonexistent/path/to/cookie"
			},
			wantErrSub: "read bitcoind cookie",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var cookiePath string
			if tc.cookie != nil {
				cookiePath = tc.cookie(t)
			}

			user, pass, err := resolveBitcoindAuth(
				tc.user, tc.pass, cookiePath,
			)
			if tc.wantErrSub != "" {
				if err == nil ||
					!strings.Contains(
						err.Error(), tc.wantErrSub,
					) {

					t.Fatalf("want error containing "+
						"%q, got %v", tc.wantErrSub,
						err)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if user != tc.wantUser {
				t.Fatalf("user: want %q, got %q", tc.wantUser,
					user)
			}
			if pass != tc.wantPass {
				t.Fatalf("pass: want %q, got %q", tc.wantPass,
					pass)
			}
		})
	}
}
