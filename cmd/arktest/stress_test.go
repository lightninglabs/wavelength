//go:build itest

package main

import (
	"encoding/json"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	darepoharness "github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
)

// TestStressClientNamesAreZeroPadded verifies generated stress names stay
// stable and sort naturally for larger client sets.
func TestStressClientNamesAreZeroPadded(t *testing.T) {
	require.Equal(t, []string{
		"client01", "client02", "client03",
	}, stressClientNames(3))

	names := stressClientNames(101)
	require.Equal(t, "client001", names[0])
	require.Equal(t, "client101", names[len(names)-1])
}

// TestStressBudgetHonorsDisabledRestarts verifies a disabled restart class
// does not keep the workload alive after payment and round budgets are spent.
func TestStressBudgetHonorsDisabledRestarts(t *testing.T) {
	runner := &stressRunner{
		cfg: stressConfig{
			maxPayments:      1,
			maxRounds:        1,
			maxRestarts:      10,
			clientRestarts:   false,
			operatorRestarts: false,
		},
		summary: stressSummary{
			PaymentsAttempted: 1,
			RoundsTriggered:   1,
		},
	}

	require.False(t, runner.hasBudget())
}

// TestRoundStateAtLeastUsesLifecycleOrder verifies client round waiters do not
// depend on protobuf enum numeric order.
func TestRoundStateAtLeastUsesLifecycleOrder(t *testing.T) {
	require.True(t, roundStateAtLeast(
		daemonrpc.RoundState_ROUND_STATE_CONFIRMED,
		daemonrpc.RoundState_ROUND_STATE_REGISTRATION_SENT,
	))
	require.True(t, roundStateAtLeast(
		daemonrpc.RoundState_ROUND_STATE_IDLE,
		daemonrpc.RoundState_ROUND_STATE_IDLE,
	))
	require.False(t, roundStateAtLeast(
		daemonrpc.RoundState_ROUND_STATE_IDLE,
		daemonrpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY,
	))
	require.False(t, roundStateAtLeast(
		daemonrpc.RoundState_ROUND_STATE_QUOTE_RECEIVED,
		daemonrpc.RoundState_ROUND_STATE_JOINED,
	))
	require.True(t, roundStateAtLeast(
		daemonrpc.RoundState_ROUND_STATE_QUOTE_RECEIVED,
		daemonrpc.RoundState_ROUND_STATE_REGISTRATION_SENT,
	))
	require.False(t, roundStateAtLeast(
		daemonrpc.RoundState_ROUND_STATE_FAILED,
		daemonrpc.RoundState_ROUND_STATE_IDLE,
	))
}

// TestStressBudgetIncludesClientCrashes verifies crash events share the
// restart budget and keep the workload eligible while crash budget remains.
func TestStressBudgetIncludesClientCrashes(t *testing.T) {
	runner := &stressRunner{
		cfg: stressConfig{
			maxRestarts:      1,
			clientCrashes:    true,
			clientRestarts:   false,
			operatorRestarts: false,
		},
	}

	require.True(t, runner.hasBudget())

	runner.summary.ClientCrashes = 1
	require.False(t, runner.hasBudget())
}

// TestStressClientRPCRejectsUnavailableHandles verifies concurrent workload
// paths return errors instead of dereferencing nil handles during a
// restart/crash window.
func TestStressClientRPCRejectsUnavailableHandles(t *testing.T) {
	runner := &stressRunner{
		clients: map[string]*darepoharness.ClientDaemonHarness{
			"client01": nil,
			"client02": {},
		},
	}

	_, err := runner.clientRPC("client01")
	require.ErrorContains(t, err, "client client01 daemon unavailable")

	_, err = runner.clientRPC("client02")
	require.ErrorContains(t, err, "client client02 daemon unavailable")

	_, err = runner.clientRPC("client03")
	require.ErrorContains(t, err, "client client03 daemon unavailable")
}

// TestStressFailureExpectationPolicy verifies workload failures are classified
// against the active stress shape.
func TestStressFailureExpectationPolicy(t *testing.T) {
	runner := &stressRunner{
		cfg: stressConfig{
			maxRestarts:    1,
			clientCrashes:  true,
			clientRestarts: false,
		},
	}

	class := runner.classifyFailure(errors.New(
		"rpc error: code = Canceled desc = grpc: " +
			"the client connection is closing",
	))
	require.Equal(t, failureClassConnectionClosing, class)
	require.True(t, runner.failureExpected(class))

	class = runner.classifyFailure(errors.New(
		"rpc error: code = InvalidArgument desc = OOR change output " +
			"429 sat is below dust limit 1000 sat",
	))
	require.Equal(t, failureClassDustChange, class)
	require.True(t, runner.failureExpected(class))

	class = runner.classifyFailure(errors.New(
		"client06 has 0 sats, need at least 1000",
	))
	require.Equal(t, failureClassInsufficientFunds, class)
	require.True(t, runner.failureExpected(class))

	class = runner.classifyFailure(errors.New("boom"))
	require.Equal(t, failureClassUnexpected, class)
	require.False(t, runner.failureExpected(class))

	require.True(t, runner.failureExpected(failureClassDustChange))
	require.True(t, runner.failureExpected(failureClassInsufficientFunds))
	require.True(t, runner.failureExpected(failureClassNoFundedSender))
	require.True(t, runner.failureExpected(failureClassNoLiveVTXOs))
	require.False(t, runner.failureExpected(failureClassRoundTimeout))
	require.False(t, runner.failureExpected(failureClassFailedRound))

	runner.cfg.operatorRestarts = true
	require.True(t, runner.failureExpected(failureClassRoundTimeout))
	require.True(t, runner.failureExpected(failureClassFailedRound))

	runner.cfg.operatorRestarts = false
	runner.cfg.maxRestarts = 0
	require.False(t, runner.failureExpected(failureClassConnectionClosing))
	require.False(t, runner.failureExpected(failureClassRoundTimeout))
}

// TestStressUnexpectedProbeFailuresAffectInvariants verifies unexpected
// sender-selection probe failures are visible in the workload summary even when
// they do not directly increment failed payment counters.
func TestStressUnexpectedProbeFailuresAffectInvariants(t *testing.T) {
	runner := &stressRunner{}

	runner.recordUnexpectedProbeFailure(failureClassUnexpected, false)
	runner.recordUnexpectedProbeFailure(
		failureClassConnectionClosing, true,
	)

	require.Equal(t, 1, runner.summary.UnexpectedFailures)
	require.Equal(t, 0, runner.summary.ExpectedFailures)
	require.Equal(t, map[string]int{
		string(failureClassUnexpected): 1,
	}, runner.summary.FailureClasses)
}

// TestSenderSelectionStatsFields verifies no-funded-sender diagnostics keep the
// aggregate and per-client details needed to explain skipped payments.
func TestSenderSelectionStatsFields(t *testing.T) {
	stats := senderSelectionStats{
		ClientsChecked:   3,
		RPCFailed:        1,
		BelowMin:         2,
		Candidates:       0,
		MaxLiveBalance:   900,
		TotalLiveBalance: 1200,
		MaxAvailable:     700,
		TotalAvailable:   700,
		TotalReserved:    500,
		MinPayment:       1000,
		Clients: []senderSelectionClient{
			{
				Name:     "client01",
				Status:   "rpc_failed",
				Class:    failureClassConnectionClosing,
				Expected: true,
				Error:    "connection is closing",
			},
			{
				Name:        "client02",
				Status:      "below_min",
				LiveBalance: 900,
				LiveVTXOs:   1,
				Reserved:    200,
				Available:   700,
				Expected:    true,
			},
		},
	}

	fields := stats.fields()

	require.Equal(t, 3, fields["clients_checked"])
	require.Equal(t, 1, fields["rpc_failed"])
	require.Equal(t, 2, fields["below_min"])
	require.Equal(t, int64(900), fields["max_live_balance_sat"])
	require.Equal(t, int64(1200), fields["total_live_balance_sat"])
	require.Equal(t, int64(700), fields["max_available_sat"])
	require.Equal(t, int64(700), fields["total_available_sat"])
	require.Equal(t, int64(500), fields["total_reserved_sat"])
	require.Equal(t, int64(1000), fields["min_payment_sat"])
	require.Equal(
		t, "client01:rpc_failed/connection_closing,"+
			"client02:below_min/live=900/reserved=200/"+
			"available=700/vtxos=1",
		fields["client_scan"],
	)
	require.Equal(t, stats.Clients, fields["clients"])

	encoded, err := json.Marshal(fields["clients"])
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"expected":true`)
	require.NotContains(t, string(encoded), `"expected":false`)
	require.Contains(t, string(encoded), `"live_balance_sat":0`)
	require.Contains(t, string(encoded), `"available_sat":0`)

	require.Equal(
		t, "\t\tclient01 status=rpc_failed "+
			"class=connection_closing expected=true\n"+
			"\t\t+1 more (see events.jsonl)",
		stats.scanBlock(1),
	)
}

// TestStressPaymentReservationsAvoidOverbooking verifies concurrent payment
// selection reserves whole VTXOs, not partial balances.
func TestStressPaymentReservationsAvoidOverbooking(t *testing.T) {
	runner := &stressRunner{
		cfg: stressConfig{
			minPayment: 1_000,
			maxPayment: 1_000,
		},
		rng:             rand.New(rand.NewSource(1)),
		paymentReserved: make(map[string]map[string]int64),
	}
	vtxos := []*daemonrpc.VTXO{
		{
			Outpoint:  "txid:0",
			AmountSat: 1_500,
		},
	}

	reservation, ok := runner.reservePaymentVTXOs("client01", vtxos)
	require.True(t, ok)
	require.Equal(t, int64(1_000), reservation.Amount)
	require.Equal(t, int64(1_500), reservation.Available)
	require.Equal(t, []string{"txid:0"}, reservation.Outpoints)
	require.Equal(t, int64(1_500), sumReservedVTXOs(
		runner.paymentReserved["client01"],
	))

	reservation, ok = runner.reservePaymentVTXOs("client01", vtxos)
	require.False(t, ok)
	require.Equal(t, int64(0), reservation.Available)

	runner.releasePaymentReservation("client01", []string{"txid:0"})
	require.Empty(t, runner.paymentReserved)
}

// TestStressFinalSummaryMetrics verifies derived latency, success-rate, and
// throughput fields are stable snapshots of the runner counters.
func TestStressFinalSummaryMetrics(t *testing.T) {
	started := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	completed := started.Add(4 * time.Second)
	runner := &stressRunner{
		cfg: stressConfig{
			seed:        42,
			concurrency: 3,
		},
		started: started,
		state: &harnessState{
			RunDir: "/tmp/arktest",
		},
		names: []string{"client01", "client02"},
		summary: stressSummary{
			PaymentsAttempted: 5,
			PaymentsSettled:   4,
			PaymentsFailed:    1,
			RoundsTriggered:   2,
			RoundsConfirmed:   1,
			RoundsFailed:      1,
			ExpectedFailures:  1,
			FailureClasses: map[string]int{
				string(failureClassDustChange): 1,
			},
		},
		paymentLatencies: []time.Duration{
			100 * time.Millisecond,
			200 * time.Millisecond,
			300 * time.Millisecond,
			1_000 * time.Millisecond,
		},
	}

	summary := runner.finalSummary(completed)

	require.Equal(t, int64(42), summary.Seed)
	require.Equal(t, int64(4_000), summary.DurationMS)
	require.Equal(t, 2, summary.Clients)
	require.Equal(t, 3, summary.Concurrency)
	require.Equal(t, 80.0, summary.PaymentSuccessPct)
	require.Equal(t, int64(400), summary.PaymentAvgMS)
	require.Equal(t, int64(200), summary.PaymentP50MS)
	require.Equal(t, int64(1_000), summary.PaymentP95MS)
	require.Equal(t, int64(1_000), summary.PaymentMaxMS)
	require.InDelta(t, 1.0, summary.PaymentThroughput, 0.001)
	require.Equal(t, 2, summary.RoundsTriggered)
	require.Equal(t, 1, summary.RoundsConfirmed)
	require.Equal(t, 1, summary.RoundsFailed)
	require.Equal(t, stressResultPass, summary.HarnessResult)
	require.Equal(t, stressResultExpectedFailures, summary.WorkloadResult)
	require.Equal(t, stressResultPass, summary.InvariantsResult)
	require.Equal(t, stressResultPass, summary.RecoveryResult)
	require.Equal(t, 1, summary.ExpectedFailures)
	require.Equal(t, 0, summary.UnexpectedFailures)
	require.Equal(t, map[string]int{
		string(failureClassDustChange): 1,
	}, summary.FailureClasses)
}

// TestPercentileDurationUsesNearestRank verifies the percentile helper's
// nearest-rank behavior at the edges and middle of a sorted sample.
func TestPercentileDurationUsesNearestRank(t *testing.T) {
	sorted := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		30 * time.Millisecond,
		40 * time.Millisecond,
	}

	require.Equal(t, 10*time.Millisecond, percentileDuration(sorted, 0))
	require.Equal(t, 20*time.Millisecond, percentileDuration(sorted, 50))
	require.Equal(t, 40*time.Millisecond, percentileDuration(sorted, 95))
	require.Equal(t, time.Duration(0), percentileDuration(nil, 95))
}
