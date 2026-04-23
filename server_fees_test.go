package darepo

import (
	"context"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/fees"
	"github.com/lightninglabs/darepo/lndbackend"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestScheduleFromConfigCarriesMinRefreshDeltaBlocks verifies
// that the MinRefreshDeltaBlocks field added to FeesConfig for
// #263 is faithfully propagated into the fees.Schedule consumed
// by the calculator. A regression that dropped this field would
// silently reset the δ_min refresh-fee floor to zero on every
// boot.
func TestScheduleFromConfigCarriesMinRefreshDeltaBlocks(t *testing.T) {
	t.Parallel()

	cfg := &FeesConfig{
		AnnualRate:          0.05,
		BaseMarginSat:       100,
		MinViableVTXOPolicy: "reject",
		MinViableVTXOPct:    50,

		// The load-bearing field: a non-default floor to
		// prove the copy path actually wires through.
		MinRefreshDeltaBlocks: 77,
	}

	sched := scheduleFromConfig(cfg)
	require.Equal(t, uint32(77), sched.MinRefreshDeltaBlocks)
}

// TestScheduleFromConfigRefusesInvalidSchedule verifies that a
// malformed FeesConfig produces a schedule that Validate()
// rejects. The actual refusal on boot is enforced in
// setupFeesSubsystem, which calls Validate and returns the
// error; this test locks in that the conversion path does not
// silently normalize bad inputs.
func TestScheduleFromConfigRefusesInvalidSchedule(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cfg     *FeesConfig
		wantErr string
	}{
		{
			name: "negative annual rate",
			cfg: &FeesConfig{
				AnnualRate:          -0.01,
				MinViableVTXOPolicy: "reject",
			},
			wantErr: "annual rate must be >= 0",
		},
		{
			name: "negative margin",
			cfg: &FeesConfig{
				AnnualRate:          0.05,
				BaseMarginSat:       -100,
				MinViableVTXOPolicy: "reject",
			},
			wantErr: "base margin must be >= 0",
		},
		{
			name: "utilization over 100%",
			cfg: &FeesConfig{
				AnnualRate:              0.05,
				UtilizationThresholdBPS: 20_000,
				MinViableVTXOPolicy:     "reject",
			},
			wantErr: "utilization threshold must be <= 10000 bps",
		},
		{
			name: "min viable pct over 100",
			cfg: &FeesConfig{
				AnnualRate:          0.05,
				MinViableVTXOPolicy: "reject",
				MinViableVTXOPct:    150,
			},
			wantErr: "min viable VTXO pct must be <= 100",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sched := scheduleFromConfig(tc.cfg)
			err := sched.Validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestScheduleFromConfigNilConfigIsAllZero verifies that a nil
// FeesConfig yields an all-zero Schedule, which is the
// documented "fees disabled" configuration. This pins the
// back-compat contract for operators who haven't opted into
// the fees subsystem yet.
func TestScheduleFromConfigNilConfigIsAllZero(t *testing.T) {
	t.Parallel()

	sched := scheduleFromConfig(nil)
	require.NotNil(t, sched)
	require.Equal(t, float64(0), sched.AnnualRate)
	require.Equal(t, int64(0), sched.BaseMarginSat)
	require.Equal(t, uint32(0), sched.UtilizationThresholdBPS)
	require.Equal(t, uint32(0), sched.MinRefreshDeltaBlocks)

	// Zero schedule must validate so the daemon boots with
	// fees disabled.
	require.NoError(t, sched.Validate())

	// And a Calculator must build over the zero schedule.
	_, err := fees.NewCalculator(sched)
	require.NoError(t, err)
}

// fakeWalletKit is a minimal lndclient.WalletKitClient for the
// pickFeeEstimator selector unit test. Every method panics so an
// unexpected call in a new test surfaces loudly rather than silently
// hitting a zero-value path.
type fakeWalletKit struct {
	lndclient.WalletKitClient
}

// EstimateFeeRate is unused by pickFeeEstimator itself (the
// selector only checks for non-nil walletKit), but provided so the
// returned estimator is callable during any follow-up assertion.
func (f *fakeWalletKit) EstimateFeeRate(_ context.Context,
	_ int32) (chainfee.SatPerKWeight, error) {

	return chainfee.FeePerKwFloor, nil
}

// TestPickFeeEstimatorSelector verifies pickFeeEstimator's three
// branches: (1) static override from config, (2) WalletKit when LND
// is wired, (3) static FeePerKwFloor fallback. This locks in the
// config-driven switch that replaced the pre-#267 hardcoded static
// estimator so test harnesses can pin the rate via StaticFeeRateSatKW
// and production gets the chain-backed path without a code change.
func TestPickFeeEstimatorSelector(t *testing.T) {
	t.Parallel()

	const customStatic = chainfee.SatPerKWeight(4_321)

	cases := []struct {
		name      string
		cfg       *FeesConfig
		walletKit lndclient.WalletKitClient
		wantRate  chainfee.SatPerKWeight
		wantType  string
	}{
		{
			name:      "nil cfg falls back to floor",
			cfg:       nil,
			walletKit: nil,
			wantRate:  chainfee.FeePerKwFloor,
			wantType:  "static",
		},
		{
			name: "static override beats walletKit",
			cfg: &FeesConfig{
				StaticFeeRateSatKW: int64(customStatic),
			},
			walletKit: &fakeWalletKit{},
			wantRate:  customStatic,
			wantType:  "static",
		},
		{
			name:      "walletKit wins when no static override",
			cfg:       &FeesConfig{},
			walletKit: &fakeWalletKit{},
			wantType:  "walletkit",
		},
		{
			name:      "fallback floor when neither configured",
			cfg:       &FeesConfig{},
			walletKit: nil,
			wantRate:  chainfee.FeePerKwFloor,
			wantType:  "static",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			est := pickFeeEstimator(
				tc.cfg, tc.walletKit, btclog.Disabled,
			)
			require.NotNil(t, est)

			switch tc.wantType {
			case "static":
				// The static estimator's EstimateFeePerKW
				// returns its configured rate regardless of
				// target; assert that matches.
				got, err := est.EstimateFeePerKW(6)
				require.NoError(t, err)
				require.Equal(t, tc.wantRate, got)

			case "walletkit":
				// Must be a *WalletKitEstimator, not a
				// static estimator. Type-assert to lock in
				// the selector's choice.
				_, ok := est.(*lndbackend.WalletKitEstimator)
				require.True(t, ok,
					"expected *WalletKitEstimator, "+
						"got %T", est)
			}
		})
	}
}
