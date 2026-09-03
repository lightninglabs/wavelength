//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
)

// TestDecodeReceiveRequestUsesBoundedDefault verifies older hosts that omit
// TimeoutSeconds still receive a finite deadline without changing the SDK DTO.
func TestDecodeReceiveRequestUsesBoundedDefault(t *testing.T) {
	req, timeout, err := decodeReceiveRequest([]byte(
		`{"AmountSat":21000,"Memo":"coffee"}`,
	))
	if err != nil {
		t.Fatalf("decode receive request: %v", err)
	}
	if req.AmountSat != 21_000 || req.Memo != "coffee" {
		t.Fatalf("unexpected receive request: %+v", req)
	}
	if timeout != defaultReceiveTimeout {
		t.Fatalf("timeout = %v, want %v", timeout,
			defaultReceiveTimeout)
	}
}

// TestDecodeReceiveRequestAcceptsExplicitDeadline verifies a mobile host can
// choose a shorter bounded foreground deadline for invoice creation.
func TestDecodeReceiveRequestAcceptsExplicitDeadline(t *testing.T) {
	_, timeout, err := decodeReceiveRequest([]byte(
		`{"AmountSat":1000,"TimeoutSeconds":12}`,
	))
	if err != nil {
		t.Fatalf("decode receive request: %v", err)
	}
	if timeout != 12*time.Second {
		t.Fatalf("timeout = %v, want 12s", timeout)
	}
}

// TestDecodeReceiveRequestRejectsInvalidDeadlines verifies malformed negative
// or excessive values cannot restore an unbounded receive call.
func TestDecodeReceiveRequestRejectsInvalidDeadlines(t *testing.T) {
	for name, body := range map[string]string{
		"negative":  `{"AmountSat":1000,"TimeoutSeconds":-1}`,
		"excessive": `{"AmountSat":1000,"TimeoutSeconds":301}`,
		"overflow":  `{"AmountSat":1000,"TimeoutSeconds":9223372036854775807}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := decodeReceiveRequest(
				[]byte(body),
			); err == nil {

				t.Fatal("expected invalid timeout error")
			}
		})
	}
}

// TestReadContextHasDeadline verifies repeatable mobile reads never inherit the
// full daemon lifetime when a foreign host cannot provide context.Context.
func TestReadContextHasDeadline(t *testing.T) {
	ctx, cancel := readContext(t.Context())
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("read context has no deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > defaultReadTimeout {
		t.Fatalf("read deadline remaining = %v", remaining)
	}
}

// TestReceiveErrorMarksUncertainTimeout verifies a binding-owned receive
// deadline has a stable host-visible prefix and preserves the original cause.
func TestReceiveErrorMarksUncertainTimeout(t *testing.T) {
	parentCtx := context.Background()
	callCtx, cancel := context.WithTimeout(parentCtx, 0)
	defer cancel()

	err := receiveError(callCtx, context.DeadlineExceeded)
	if !strings.HasPrefix(err.Error(), receiveUncertainErrorPrefix) {
		t.Fatalf("receive error = %q", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("receive error lost deadline cause: %v", err)
	}
}

// TestReceiveErrorMarksLifecycleCancellation verifies Stop cancellation has
// the same reconcile-before-retry marker as a binding-owned deadline.
func TestReceiveErrorMarksLifecycleCancellation(t *testing.T) {
	parentCtx, cancelParent := context.WithCancel(context.Background())
	callCtx, cancelCall := context.WithTimeout(parentCtx, time.Minute)
	defer cancelCall()
	cancelParent()
	<-callCtx.Done()

	want := errors.New("wallet stopped")
	got := receiveError(callCtx, want)
	if !strings.HasPrefix(got.Error(), receiveUncertainErrorPrefix) {
		t.Fatalf("receive error = %q", got)
	}
	if !errors.Is(got, want) {
		t.Fatalf("receive error lost lifecycle cause: %v", got)
	}
}

// TestParseConfigEmptyUsesDefaults verifies that an empty config string yields
// the wavewalletdk defaults rather than a zero config.
func TestParseConfigEmptyUsesDefaults(t *testing.T) {
	got, err := parseConfig("")
	if err != nil {
		t.Fatalf("parseConfig(\"\"): %v", err)
	}

	want := wavewalletdk.DefaultConfig()
	if got.Network != want.Network {
		t.Fatalf("network = %q, want default %q", got.Network,
			want.Network)
	}
	if got.WalletType != want.WalletType {
		t.Fatalf("wallet type = %q, want default %q", got.WalletType,
			want.WalletType)
	}
	if opts := mobileStartOptions(got); len(opts) != 0 {
		t.Fatalf("default config produced %d start options", len(opts))
	}
}

// TestParseConfigOverlaysSetFields verifies that only the fields the host set
// are overlaid onto the defaults, and that the seconds/scalar mappings land on
// the right wavewalletdk fields.
func TestParseConfigOverlaysSetFields(t *testing.T) {
	const cfgJSON = `{
		"data_dir": "/tmp/wavewalletdk",
		"network": "regtest",
		"server_address": "127.0.0.1:9000",
		"server_insecure": true,
		"wallet_poll_interval_seconds": 5,
		"wallet_recovery_window": 250,
		"max_operator_fee_sat": 1000,
		"max_payment_cltv": 400,
		"auto_refresh_fee_floor_sat": 750,
		"auto_refresh_fee_rate_ppm": 25000,
		"signing_workers": 1,
		"buffer_size": 4096
	}`

	got, err := parseConfig(cfgJSON)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if got.DataDir != "/tmp/wavewalletdk" {
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
	if got.MaxPaymentCLTV != 400 {
		t.Fatalf("max payment CLTV = %d", got.MaxPaymentCLTV)
	}
	if got.AutoRefreshFeeFloorSat != 750 {
		t.Fatalf("auto refresh fee floor = %d",
			got.AutoRefreshFeeFloorSat)
	}
	if got.AutoRefreshFeeRatePPM != 25_000 {
		t.Fatalf("auto refresh fee rate = %d",
			got.AutoRefreshFeeRatePPM)
	}
	if got.SigningWorkers != 1 {
		t.Fatalf("signing workers = %d", got.SigningWorkers)
	}
	if got.BufferSize != 4096 {
		t.Fatalf("buffer size = %d", got.BufferSize)
	}
}

// TestParseConfigDisablesMaxPaymentCLTV verifies an explicit JSON zero wins
// over the swap-enabled default instead of being mistaken for an omitted
// convenience field.
func TestParseConfigDisablesMaxPaymentCLTV(t *testing.T) {
	got, err := parseConfig(`{"max_payment_cltv": 0}`)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if got.MaxPaymentCLTV != 0 {
		t.Fatalf("max payment CLTV = %d, want disabled",
			got.MaxPaymentCLTV)
	}
	if opts := mobileStartOptions(got); len(opts) != 1 {
		t.Fatalf("explicit zero produced %d start options, want 1",
			len(opts))
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
		"max payment CLTV": `{"max_payment_cltv": -1}`,
		"auto fee floor":   `{"auto_refresh_fee_floor_sat": -1}`,
		"auto fee rate":    `{"auto_refresh_fee_rate_ppm": -1}`,
		"signing workers":  `{"signing_workers": -1}`,
		"buffer size":      `{"buffer_size": -1}`,
	}
	for name, cfgJSON := range cases {
		if _, err := parseConfig(cfgJSON); err == nil {
			t.Fatalf("%s: expected error for negative value", name)
		}
	}
}

// TestParseConfigRejectsOverflowMaxPaymentCLTV verifies the mobile boundary
// rejects a value that would wrap when narrowed to the daemon's int32 policy.
func TestParseConfigRejectsOverflowMaxPaymentCLTV(t *testing.T) {
	if _, err := parseConfig(
		`{"max_payment_cltv": 2147483648}`,
	); err == nil {

		t.Fatal("expected error for max payment CLTV above int32 max")
	}
}

// TestParseConfigRejectsExcessiveAutoRefreshRate verifies the mobile boundary
// rejects rates above 100%, matching waved's configuration validation.
func TestParseConfigRejectsExcessiveAutoRefreshRate(t *testing.T) {
	if _, err := parseConfig(
		`{"auto_refresh_fee_rate_ppm": 1000001}`,
	); err == nil {

		t.Fatal("expected error for auto-refresh fee rate above 100%")
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
		wavewalletdk.MaxSigningWorkers+1)
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

// TestEntryRoundTripsAsJSON guards the bytes-out contract: a wavewalletdk.Entry
// must marshal to JSON the host can decode, including the optional nested
// Progress / Request unions.
func TestEntryRoundTripsAsJSON(t *testing.T) {
	entry := wavewalletdk.Entry{
		ID:        "abc",
		Kind:      wavewalletdk.EntryKindReceive,
		Status:    wavewalletdk.EntryStatusPending,
		AmountSat: 1234,
		Request: &wavewalletdk.EntryRequest{
			Type:             wavewalletdk.EntryRequestTypeLightning,
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

	var back wavewalletdk.Entry
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}
	if back.ID != entry.ID || back.AmountSat != entry.AmountSat {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
	if back.Request == nil ||
		back.Request.Type != wavewalletdk.EntryRequestTypeLightning {

		t.Fatalf("request union lost in round-trip: %+v", back.Request)
	}
}
