//go:build mobile && walletdkrpc && swapruntime

package mobile

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/lightninglabs/wavelength/sdk/walletdk"
)

// TestParseConfigEmptyUsesDefaults verifies that an empty config string yields
// the walletdk defaults rather than a zero config.
func TestParseConfigEmptyUsesDefaults(t *testing.T) {
	got, err := parseConfig("")
	if err != nil {
		t.Fatalf("parseConfig(\"\"): %v", err)
	}

	want := walletdk.DefaultConfig()
	if got.Network != want.Network {
		t.Fatalf("network = %q, want default %q", got.Network,
			want.Network)
	}
	if got.WalletType != want.WalletType {
		t.Fatalf("wallet type = %q, want default %q", got.WalletType,
			want.WalletType)
	}
}

// TestParseConfigOverlaysSetFields verifies that only the fields the host set
// are overlaid onto the defaults, and that the seconds/scalar mappings land on
// the right walletdk fields.
func TestParseConfigOverlaysSetFields(t *testing.T) {
	const cfgJSON = `{
		"data_dir": "/tmp/walletdk",
		"network": "regtest",
		"server_address": "127.0.0.1:9000",
		"server_insecure": true,
		"wallet_poll_interval_seconds": 5,
		"wallet_recovery_window": 250,
		"max_operator_fee_sat": 1000,
		"signing_workers": 1,
		"buffer_size": 4096
	}`

	got, err := parseConfig(cfgJSON)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if got.DataDir != "/tmp/walletdk" {
		t.Fatalf("data dir = %q", got.DataDir)
	}
	if got.Network != "regtest" {
		t.Fatalf("network = %q", got.Network)
	}
	if got.ServerAddress != "127.0.0.1:9000" {
		t.Fatalf("server address = %q", got.ServerAddress)
	}
	if !got.ServerInsecure {
		t.Fatal("server insecure not applied")
	}
	if got.WalletPollInterval.Seconds() != 5 {
		t.Fatalf("poll interval = %v, want 5s", got.WalletPollInterval)
	}
	if got.WalletRecoveryWindow != 250 {
		t.Fatalf("recovery window = %d", got.WalletRecoveryWindow)
	}
	if got.MaxOperatorFeeSat != 1000 {
		t.Fatalf("max operator fee = %d", got.MaxOperatorFeeSat)
	}
	if got.SigningWorkers != 1 {
		t.Fatalf("signing workers = %d", got.SigningWorkers)
	}
	if got.BufferSize != 4096 {
		t.Fatalf("buffer size = %d", got.BufferSize)
	}
}

// TestParseConfigRejectsBadJSON verifies malformed JSON is reported as an
// error rather than silently ignored.
func TestParseConfigRejectsBadJSON(t *testing.T) {
	if _, err := parseConfig("{not json"); err == nil {
		t.Fatal("expected error for malformed config JSON")
	}
}

// TestParseConfigRejectsNegativeScalars verifies the signed gomobile config
// scalars are rejected when negative, so malformed JSON returns a startup error
// instead of, e.g., a negative poll interval panicking the tip poller.
func TestParseConfigRejectsNegativeScalars(t *testing.T) {
	cases := map[string]string{
		"poll interval":    `{"wallet_poll_interval_seconds": -1}`,
		"recovery window":  `{"wallet_recovery_window": -5}`,
		"max operator fee": `{"max_operator_fee_sat": -1000}`,
		"signing workers":  `{"signing_workers": -1}`,
		"buffer size":      `{"buffer_size": -1}`,
	}
	for name, cfgJSON := range cases {
		if _, err := parseConfig(cfgJSON); err == nil {
			t.Fatalf("%s: expected error for negative value", name)
		}
	}
}

// TestParseConfigRejectsOverflowRecoveryWindow verifies a recovery window above
// the uint32 max is rejected rather than silently wrapping in the conversion.
func TestParseConfigRejectsOverflowRecoveryWindow(t *testing.T) {
	// 4294967296 == math.MaxUint32 + 1.
	if _, err := parseConfig(
		`{"wallet_recovery_window": 4294967296}`,
	); err == nil {

		t.Fatal("expected error for recovery window above uint32 max")
	}
}

// TestParseConfigRejectsExcessiveSigningWorkers verifies the mobile boundary
// applies the same bounded concurrency cap as waved.
func TestParseConfigRejectsExcessiveSigningWorkers(t *testing.T) {
	cfgJSON := fmt.Sprintf(`{"signing_workers": %d}`,
		walletdk.MaxSigningWorkers+1)
	if _, err := parseConfig(cfgJSON); err == nil {
		t.Fatal("expected error for excessive signing worker count")
	}
}

// TestVerbsFailWhenNotStarted verifies that every accessor reports a clear
// not-started error before Start, instead of panicking on a nil client.
func TestVerbsFailWhenNotStarted(t *testing.T) {
	if _, _, err := activeClient(); err == nil {
		t.Fatal("activeClient should fail before Start")
	}
	if _, err := GetInfo(); err == nil {
		t.Fatal("GetInfo should fail before Start")
	}
	if _, err := Balance(); err == nil {
		t.Fatal("Balance should fail before Start")
	}
	if _, err := ConfirmedBalanceSat(); err == nil {
		t.Fatal("ConfirmedBalanceSat should fail before Start")
	}
	if IsRunning() {
		t.Fatal("IsRunning should be false before Start")
	}
}

// TestStopIdempotentWhenNotStarted verifies Stop is a no-op when nothing is
// running.
func TestStopIdempotentWhenNotStarted(t *testing.T) {
	if err := Stop(); err != nil {
		t.Fatalf("Stop on stopped client = %v, want nil", err)
	}
}

// TestStartRejectsBadConfigAndResets verifies that a Start whose config fails
// to parse returns the error synchronously and releases the singleton so a
// later Start can run.
func TestStartRejectsBadConfigAndResets(t *testing.T) {
	if err := Start("{bad json"); err == nil {
		t.Fatal("expected error for bad config")
	}

	// The singleton must have reset; activeClient still reports not
	// started and a subsequent Stop is clean.
	if IsRunning() {
		t.Fatal("IsRunning true after failed Start")
	}
	if err := Stop(); err != nil {
		t.Fatalf("Stop after failed Start = %v", err)
	}
}

// TestSubscribeFailsWhenNotStarted verifies Subscribe returns the not-started
// error rather than panicking on a nil client.
func TestSubscribeFailsWhenNotStarted(t *testing.T) {
	if _, err := Subscribe(nil); err == nil {
		t.Fatal("Subscribe should fail before Start")
	}
}

// TestEntryRoundTripsAsJSON guards the bytes-out contract: a walletdk.Entry
// must marshal to JSON the host can decode, including the optional nested
// Progress / Request unions.
func TestEntryRoundTripsAsJSON(t *testing.T) {
	entry := walletdk.Entry{
		ID:        "abc",
		Kind:      walletdk.EntryKindReceive,
		Status:    walletdk.EntryStatusPending,
		AmountSat: 1234,
		Request: &walletdk.EntryRequest{
			Type:             walletdk.EntryRequestTypeLightning,
			LightningInvoice: "lnbc1...",
		},
	}

	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}

	// The DTOs carry no json tags, so the wire keys are the Go field names
	// (PascalCase). That is the documented public contract a foreign
	// decoder relies on, so pin the literal keys here rather than only
	// proving a Go-to-Go round trip.
	for _, key := range []string{`"ID"`, `"Kind"`, `"AmountSat"`} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("entry JSON missing wire key %s: %s", key, b)
		}
	}

	var back walletdk.Entry
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}
	if back.ID != entry.ID || back.AmountSat != entry.AmountSat {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
	if back.Request == nil ||
		back.Request.Type != walletdk.EntryRequestTypeLightning {

		t.Fatalf("request union lost in round-trip: %+v", back.Request)
	}
}
