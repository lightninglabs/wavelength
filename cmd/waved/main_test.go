package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lightninglabs/wavelength/waved"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// TestReadConfigFileLoadsProperties checks that an explicit config file is
// merged into Viper and unmarshaled into the daemon config.
func TestReadConfigFileLoadsProperties(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "waved.conf")
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

	cfg := waved.DefaultConfig()
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
// WAVED_CONFIGFILE is reported as an error.
func TestReadConfigFileRejectsMissingEnvPath(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "missing.conf")
	t.Setenv("WAVED_CONFIGFILE", configPath)

	v, cmd := newTestConfigCommand(t, "")
	v.SetEnvPrefix("WAVED")
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

	cmd := &cobra.Command{Use: "waved"}
	cmd.Flags().String("configfile", defaultConfigPath, "")

	v := viper.New()
	if err := v.BindPFlag(
		"configfile", cmd.Flags().Lookup("configfile"),
	); err != nil {

		t.Fatalf("bind configfile flag: %v", err)
	}

	return v, cmd
}

// TestIsLoopbackHost pins the URL parsing that decides whether the
// cleartext warning should fire. Edge cases that previously slipped
// through net.SplitHostPort: bracketed IPv6, hosts that carry a path,
// and the full URL forms with explicit http:// / https:// schemes.
func TestIsLoopbackHost(t *testing.T) {
	t.Parallel()

	cases := []struct {
		host string
		want bool
	}{
		// Loopback shapes.
		{
			"127.0.0.1:8332",
			true,
		},
		{
			"127.0.0.1",
			true,
		},
		{
			"localhost:8332",
			true,
		},
		{
			"localhost",
			true,
		},
		{
			"[::1]:8332",
			true,
		},
		{
			"::1",
			true,
		},
		{
			"http://127.0.0.1:8332",
			true,
		},
		{
			"https://127.0.0.1:8332",
			true,
		},
		{
			"http://localhost",
			true,
		},
		{
			"http://[::1]:8332",
			true,
		},
		{
			"https://[::1]:8332",
			true,
		},
		{
			"127.0.0.1:8332/wallet/foo",
			true,
		},
		{
			"http://127.0.0.1:8332/wallet/foo",
			true,
		},
		// every 127/8 address is loopback
		{
			"127.0.0.2:8332",
			true,
		},

		// Non-loopback.
		{
			"10.0.0.5:8332",
			false,
		},
		{
			"bitcoind.example.com:8332",
			false,
		},
		{
			"https://bitcoind.example.com:8332",
			false,
		},
		{
			"[fd00::1]:8332",
			false,
		},

		// Degenerate.
		{
			"",
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			t.Parallel()

			if got := isLoopbackHost(tc.host); got != tc.want {
				t.Fatalf("isLoopbackHost(%q) = %v, want %v",
					tc.host, got, tc.want)
			}
		})
	}
}

// TestIsBitcoindHTTPSEndpoint verifies the warning gate used for
// non-loopback bitcoind RPC connections.
func TestIsBitcoindHTTPSEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		host        string
		tlsCertPath string
		want        bool
	}{
		{
			name: "explicit https",
			host: "https://bitcoind.example:8332",
			want: true,
		},
		{
			name:        "bare host with cert",
			host:        "bitcoind.example:8332",
			tlsCertPath: "/path/to/ca.pem",
			want:        true,
		},
		{
			name:        "explicit http cert remains plaintext",
			host:        "http://bitcoind.example:8332",
			tlsCertPath: "/path/to/ca.pem",
			want:        false,
		},
		{
			name: "bare host without cert",
			host: "bitcoind.example:8332",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := isBitcoindHTTPSEndpoint(tc.host, tc.tlsCertPath)
			if got != tc.want {
				t.Fatalf("isBitcoindHTTPSEndpoint() = "+
					"%v, want %v", got, tc.want)
			}
		})
	}
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
