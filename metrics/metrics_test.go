package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestRoundChangeRequiredForBoardingTotal_Increments asserts the
// operator-alert counter for the no-change-with-boarding round
// failure increments by exactly 1 per Inc() call. This is the
// load-bearing observability primitive operators need to alert on
// when their hot LND wallet runs short of liquidity such that
// FundPsbt produces no change output.
func TestRoundChangeRequiredForBoardingTotal_Increments(t *testing.T) {
	// No t.Parallel — package-level Prometheus counter is global
	// state shared across tests.

	before := testutil.ToFloat64(RoundChangeRequiredForBoardingTotal)
	RoundChangeRequiredForBoardingTotal.Inc()
	RoundChangeRequiredForBoardingTotal.Inc()
	RoundChangeRequiredForBoardingTotal.Inc()
	after := testutil.ToFloat64(RoundChangeRequiredForBoardingTotal)

	require.Equal(
		t, before+3, after,
		"counter must increment by exactly 1 per Inc() call",
	)
}

// TestRoundChangeRequiredForBoardingTotal_Registered asserts the
// operator-alert counter is registered with RegisterAll so it surfaces
// on the /metrics scrape endpoint. Without registration, the counter
// would silently never reach Prometheus regardless of Inc() calls.
func TestRoundChangeRequiredForBoardingTotal_Registered(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterAll(reg)

	// Trigger an increment so the counter has a non-zero sample
	// in the registry's gather output.
	RoundChangeRequiredForBoardingTotal.Inc()

	mfs, err := reg.Gather()
	require.NoError(t, err)

	const wantName = "arkd_round_change_required_for_boarding_total"
	var found bool
	for _, mf := range mfs {
		if mf.GetName() != wantName {
			continue
		}
		found = true

		// Help text must mention the operator action so dashboards
		// surface a useful tooltip.
		require.True(
			t,
			strings.Contains(
				mf.GetHelp(),
				"Operator",
			),
			"help text must mention operator action: got %q",
			mf.GetHelp(),
		)

		break
	}
	require.True(
		t, found, "metric %s must be registered via RegisterAll",
		wantName,
	)
}
