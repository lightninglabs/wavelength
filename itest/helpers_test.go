//go:build itest

package itest

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// longRegistrationTimeout returns an OperatorConfigMutator that pins the
// operator's round registration window to 30s. Multi-client shared-round
// tests need this because the harness default (500ms) is too tight for
// several clients to all join the same round in series under CI load: the
// first client triggers seal before the rest arrive, and the late clients
// land in a fresh round. 30s matches the production default (10s) with a
// 3x cushion for busy runners.
func longRegistrationTimeout() func(cfg *darepo.Config) {
	return func(cfg *darepo.Config) {
		cfg.Rounds.RegistrationTimeout = 30 * time.Second
	}
}

// longSignatureCollectionTimeout returns an OperatorConfigMutator that pins
// the operator's signature-collection window to 60s. Restart-resume tests can
// intentionally stop a client after the daemon durably checkpoints signatures
// but before the server-connection actor delivers them to the operator. The
// resumed client replays those signatures, but wallet start-up plus durable
// message retry can exceed the production 10s window on busy btcwallet CI
// runners.
func longSignatureCollectionTimeout() func(cfg *darepo.Config) {
	return func(cfg *darepo.Config) {
		cfg.Rounds.SignatureCollectionTimeout = 60 * time.Second
	}
}

const (
	defaultSmallTimeout = 5 * time.Second
	defaultTimeout      = 60 * time.Second

	// pollInterval is the cadence at which require.Eventually loops
	// re-evaluate their predicate. Most predicates here read a small
	// amount of in-memory state from a sibling goroutine (round
	// status, VTXO status, daemon balance), so the floor is set by
	// scheduler latency rather than RPC cost. The historical value
	// was 200ms, which is comfortably below human-noticeable but
	// adds avoidable wait at the tail of every successful poll:
	// e.g. a status that flipped 10ms after the previous poll waits
	// another ~190ms before the next check. Compressing this to 50ms
	// shaves on the order of 150ms per Eventually call site, and
	// each round confirms through several such call sites.
	pollInterval = 50 * time.Millisecond

	// confirmationGrace is a short pause between observing the
	// round transaction in bitcoind's mempool and mining the first
	// block. The pause exists to let every daemon's confirmation
	// watcher arm before the block lands; otherwise a watcher that
	// missed the mempool notification will re-poll on its next tick
	// (slower under multi-client tests). 200ms is enough for the
	// in-process bridge and is still well below the per-test
	// signature-collection budget.
	confirmationGrace = 200 * time.Millisecond
)

// Test convention (post-issue #263):
//
// The harness default is now a non-zero fee schedule (see
// harness.DefaultItestFeeSchedule). This means every boarding,
// refresh, sweep, and OOR flow in these itests runs with real
// operator fees deducted from VTXO amounts. When asserting a
// specific balance, callers MUST compute the expected net value
// via the fee-aware helpers in fees_helpers_test.go:
//
//   - feeQuoteForBoarding / expectedNetAfterBoarding
//   - feeQuoteForRefresh  / expectedNetAfterRefresh
//   - operatorEstimateFee (hits the live server for a quote
//     after utilization has moved)
//
// The convenience wrappers waitForVTXOBalance and
// waitForExactVTXOBalance take the amount verbatim; they do NOT
// subtract fees. A test that hardcodes a literal like
// int64(99_000) under the fees-on default will fail on the
// client's post-fee balance. The zero-fee code path is still
// available for regression tests via harness.WithZeroFeeSchedule
// (exercised by TestFeesDisabledGreenPath).

// getOperatorInfo fetches the public operator info over the real client RPC
// surface exposed by the in-process operator.
func getOperatorInfo(t *testing.T,
	h *harness.ArkHarness) *arkrpc.GetInfoResponse {

	t.Helper()

	conn, err := grpc.Dial(
		h.ArkRPCAddr,
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
	)
	require.NoError(t, err, "failed to connect to operator RPC")
	defer conn.Close()

	client := arkrpc.NewArkServiceClient(conn)
	resp, err := client.GetInfo(t.Context(), &arkrpc.GetInfoRequest{})
	require.NoError(t, err, "operator GetInfo RPC failed")
	require.Equal(t, "regtest", resp.Network)

	return resp
}

// waitForConfirmedBoardingBalance waits until the daemon reports at least the
// requested confirmed boarding balance.
func waitForConfirmedBoardingBalance(t *testing.T,
	client daemonrpc.DaemonServiceClient,
	minConfirmedSat int64) *daemonrpc.GetBalanceResponse {

	t.Helper()

	var lastResp *daemonrpc.GetBalanceResponse
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.GetBalance(
			ctx, &daemonrpc.GetBalanceRequest{},
		)
		if err != nil {
			return false
		}

		lastResp = resp

		return resp.BoardingConfirmedSat >= minConfirmedSat
	}, defaultTimeout, pollInterval,
		"boarding balance never reached %d sats", minConfirmedSat)

	return lastResp
}

// waitForBoardRegistered waits until a boarding request reports registered.
func waitForBoardRegistered(t *testing.T,
	client daemonrpc.DaemonServiceClient) *daemonrpc.BoardResponse {

	t.Helper()

	var lastResp *daemonrpc.BoardResponse
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.Board(ctx, &daemonrpc.BoardRequest{})
		if err != nil {
			return false
		}

		lastResp = resp

		return resp.Status == "registered"
	}, defaultTimeout, pollInterval,
		"Board RPC never reached registered status")

	return lastResp
}

// waitForRegisteredClients waits until the operator reports the requested
// number of registered clients.
func waitForRegisteredClients(t *testing.T, h *harness.ArkHarness,
	minClients int) *adminrpc.ListClientsResponse {

	t.Helper()

	var lastResp *adminrpc.ListClientsResponse
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := h.ArkAdminClient.ListClients(
			ctx, &adminrpc.ListClientsRequest{},
		)
		if err != nil {
			return false
		}

		lastResp = resp

		return len(resp.Clients) >= minClients
	}, defaultTimeout, pollInterval,
		"operator never observed %d registered clients", minClients)

	return lastResp
}

// waitForClientRegistration waits until at least one client registration is
// visible to the operator admin RPC.
func waitForClientRegistration(t *testing.T, h *harness.ArkHarness) {
	t.Helper()

	waitForRegisteredClients(t, h, 1)
}

// waitForClientRoundState waits until any round reported by the daemon
// satisfies the requested lifecycle state.
func waitForClientRoundState(t *testing.T, client daemonrpc.DaemonServiceClient,
	target daemonrpc.RoundState) *daemonrpc.RoundInfo {

	t.Helper()

	var matched *daemonrpc.RoundInfo
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.ListRounds(
			ctx, &daemonrpc.ListRoundsRequest{},
		)
		if err != nil {
			return false
		}

		for _, round := range resp.Rounds {
			if roundStateSatisfiesTarget(round.State, target) {
				matched = round

				return true
			}
		}

		return false
	}, defaultTimeout, pollInterval,
		"client never reached round state %s", target.String())

	return matched
}

// waitForNonTempClientRoundState waits until a server-assigned round reported
// by the daemon satisfies the requested lifecycle state.
func waitForNonTempClientRoundState(t *testing.T,
	client daemonrpc.DaemonServiceClient,
	target daemonrpc.RoundState) *daemonrpc.RoundInfo {

	t.Helper()

	var matched *daemonrpc.RoundInfo
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.ListRounds(
			ctx, &daemonrpc.ListRoundsRequest{},
		)
		if err != nil {
			return false
		}

		for _, round := range resp.Rounds {
			if round.IsTemp || round.RoundId == "" {
				continue
			}

			if roundStateSatisfiesTarget(round.State, target) {
				matched = round

				return true
			}
		}

		return false
	}, defaultTimeout, pollInterval,
		"client never reached non-temp round state %s",
		target.String())

	return matched
}

// roundStateSatisfiesTarget tolerates short-lived intermediate states that can
// be missed by polling on fast CI runners.
func roundStateSatisfiesTarget(state, target daemonrpc.RoundState) bool {
	if state == target {
		return true
	}

	// Integration polling can miss the short-lived INPUT_SIG_SENT state
	// on fast CI runners; confirmed means the round already passed it.
	if target == daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT &&
		state == daemonrpc.RoundState_ROUND_STATE_CONFIRMED {
		return true
	}

	// JOINED can also be transient on fast runners; once input signatures
	// are sent, the round has necessarily passed JOINED.
	return target == daemonrpc.RoundState_ROUND_STATE_JOINED &&
		(state == daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT ||
			state == daemonrpc.RoundState_ROUND_STATE_CONFIRMED)
}

// waitForNamedClientRoundState waits until a specific round reaches the
// requested state in either the live or persisted daemon round views.
func waitForNamedClientRoundState(t *testing.T,
	client daemonrpc.DaemonServiceClient, roundID string,
	target daemonrpc.RoundState) *daemonrpc.RoundInfo {

	t.Helper()

	var matched *daemonrpc.RoundInfo
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.ListRounds(
			ctx, &daemonrpc.ListRoundsRequest{},
		)
		if err == nil {
			for _, round := range resp.Rounds {
				if round.RoundId != roundID {
					continue
				}

				if roundStateSatisfiesTarget(
					round.State, target,
				) {

					matched = round

					return true
				}
			}
		}

		persistedResp, err := client.ListRounds(
			ctx, &daemonrpc.ListRoundsRequest{
				PersistedOnly: true,
			},
		)
		if err != nil {
			return false
		}

		for _, round := range persistedResp.Rounds {
			if round.RoundId != roundID {
				continue
			}

			if roundStateSatisfiesTarget(round.State, target) {
				matched = round

				return true
			}
		}

		return false
	}, defaultTimeout, pollInterval,
		"client round %s never reached state %s",
		roundID, target.String())

	return matched
}

// waitForPersistedClientRoundState waits until a specific persisted round
// reaches the requested state and minimum materialized VTXO count.
func waitForPersistedClientRoundState(t *testing.T,
	client daemonrpc.DaemonServiceClient, roundID string,
	target daemonrpc.RoundState, minVTXOs int) *daemonrpc.RoundInfo {

	t.Helper()

	var matched *daemonrpc.RoundInfo
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.ListRounds(
			ctx, &daemonrpc.ListRoundsRequest{
				PersistedOnly: true,
			},
		)
		if err != nil {
			return false
		}

		for _, round := range resp.Rounds {
			if round.RoundId != roundID {
				continue
			}

			if !roundStateSatisfiesTarget(round.State, target) {
				continue
			}

			if len(round.Vtxos) < minVTXOs {
				continue
			}

			matched = round

			return true
		}

		return false
	}, defaultTimeout, pollInterval,
		"persisted client round %s never reached "+
			"state %s with >= %d vtxos",
		roundID, target.String(), minVTXOs)

	return matched
}

// snapshotClientRoundIDs returns round IDs visible before a new action is
// triggered.
func snapshotClientRoundIDs(t *testing.T,
	client daemonrpc.DaemonServiceClient) map[string]struct{} {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), defaultSmallTimeout)
	defer cancel()

	resp, err := client.ListRounds(
		ctx, &daemonrpc.ListRoundsRequest{},
	)
	require.NoError(
		t, err, "ListRounds RPC failed while snapshotting rounds",
	)

	ids := make(map[string]struct{}, len(resp.Rounds))
	for _, round := range resp.Rounds {
		ids[round.RoundId] = struct{}{}
	}

	return ids
}

// waitForNewClientRoundState waits for a round that did not already satisfy
// the target state to reach that target after a new action is triggered.
func waitForNewClientRoundState(t *testing.T,
	client daemonrpc.DaemonServiceClient,
	existingRoundIDs map[string]struct{},
	target daemonrpc.RoundState) *daemonrpc.RoundInfo {

	t.Helper()

	var matched *daemonrpc.RoundInfo
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.ListRounds(
			ctx, &daemonrpc.ListRoundsRequest{},
		)
		if err != nil {
			return false
		}

		for _, round := range resp.Rounds {
			if _, known := existingRoundIDs[round.RoundId]; known {
				continue
			}

			if roundStateSatisfiesTarget(round.State, target) {
				matched = round

				return true
			}
		}

		return false
	}, defaultTimeout, pollInterval,
		"client never reached state %s on a post-trigger round",
		target.String())

	return matched
}

// waitForOperatorRoundStatus waits until the operator reports the requested
// round status for the given round ID.
func waitForOperatorRoundStatus(t *testing.T, h *harness.ArkHarness,
	roundID string, target adminrpc.RoundStatus) *adminrpc.RoundSummary {

	t.Helper()

	var matched *adminrpc.RoundSummary
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := h.ArkAdminClient.ListRounds(
			ctx, &adminrpc.ListRoundsRequest{},
		)
		if err != nil {
			return false
		}

		for _, round := range resp.Rounds {
			if round.Id != roundID {
				continue
			}

			if operatorRoundStatusSatisfiesTarget(
				round.Status, target,
			) {

				matched = round

				return true
			}
		}

		return false
	}, defaultTimeout, pollInterval,
		"operator round %s never reached status %s",
		roundID, target.String())

	return matched
}

// operatorRoundStatusSatisfiesTarget tolerates short-lived round status
// transitions that can be missed by polling on fast CI runners.
func operatorRoundStatusSatisfiesTarget(
	state, target adminrpc.RoundStatus) bool {

	if state == target {
		return true
	}

	// Broadcast can be brief; confirmed implies broadcast happened.
	return target == adminrpc.RoundStatus_ROUND_STATUS_BROADCAST &&
		state == adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED
}

// operatorRoundHasStatus reports whether the operator currently exposes the
// given round status for the specified round ID.
func operatorRoundHasStatus(t *testing.T, h *harness.ArkHarness, roundID string,
	target adminrpc.RoundStatus) bool {

	t.Helper()

	ctx, cancel := context.WithTimeout(
		t.Context(), defaultSmallTimeout,
	)
	defer cancel()

	resp, err := h.ArkAdminClient.ListRounds(
		ctx, &adminrpc.ListRoundsRequest{},
	)
	if err != nil {
		return false
	}

	for _, round := range resp.Rounds {
		if round.Id != roundID {
			continue
		}

		if operatorRoundStatusSatisfiesTarget(
			round.Status, target,
		) {
			return true
		}
	}

	return false
}

// mineUntilOperatorRoundConfirmed keeps mining regtest blocks until the
// operator marks the round confirmed.
func mineUntilOperatorRoundConfirmed(t *testing.T, h *harness.ArkHarness,
	roundID string, txID string) client_harness.Block {

	t.Helper()

	// Wait for the round transaction in bitcoind's mempool before mining.
	// This helps ensure confirmation watchers are armed before the first
	// block.
	// This is intentionally conservative for multi-client rounds where each
	// daemon registers its own watcher.
	h.WaitMempoolTx(txID)
	time.Sleep(confirmationGrace)

	var minedBlock client_harness.Block
	require.Eventually(t, func() bool {
		confirmed := operatorRoundHasStatus(
			t, h, roundID,
			adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
		)
		if confirmed {
			return true
		}

		blocks := h.GenerateAndWait(1)
		if len(blocks) == 0 {
			return false
		}

		minedBlock = blocks[0]

		return operatorRoundHasStatus(
			t, h, roundID,
			adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
		)
	}, defaultTimeout, pollInterval,
		"round %s was never confirmed after mining", roundID)

	return minedBlock
}

// waitForLiveVTXO waits until the daemon materializes a live VTXO for the
// requested round.
func waitForLiveVTXO(t *testing.T, client daemonrpc.DaemonServiceClient,
	roundID string) *daemonrpc.VTXO {

	t.Helper()

	var matched *daemonrpc.VTXO
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.ListVTXOs(ctx, &daemonrpc.ListVTXOsRequest{
			StatusFilter: daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
		})
		if err != nil {
			return false
		}

		for _, vtxo := range resp.Vtxos {
			if vtxo.RoundId == roundID {
				matched = vtxo

				return true
			}
		}

		return false
	}, defaultTimeout, pollInterval,
		"client never materialized a live VTXO for round %s", roundID)

	return matched
}

// listLiveVTXOs returns the daemon's current set of live VTXOs.
func listLiveVTXOs(t *testing.T,
	client daemonrpc.DaemonServiceClient) []*daemonrpc.VTXO {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), defaultSmallTimeout)
	defer cancel()

	resp, err := client.ListVTXOs(ctx, &daemonrpc.ListVTXOsRequest{
		StatusFilter: daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
	})
	require.NoError(t, err, "failed to list live VTXOs")

	return resp.Vtxos
}

// waitForVTXOStatusByOutpoint waits until the daemon reports the requested
// lifecycle status for the given VTXO outpoint.
func waitForVTXOStatusByOutpoint(t *testing.T,
	client daemonrpc.DaemonServiceClient, outpoint string,
	target daemonrpc.VTXOStatus) *daemonrpc.VTXO {

	t.Helper()

	var matched *daemonrpc.VTXO
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.ListVTXOs(
			ctx, &daemonrpc.ListVTXOsRequest{
				StatusFilter: target,
			},
		)
		if err != nil {
			return false
		}

		for _, vtxo := range resp.Vtxos {
			if vtxo.Outpoint != outpoint {
				continue
			}

			if vtxo.Status == target {
				matched = vtxo

				return true
			}
		}

		return false
	}, defaultTimeout, pollInterval,
		"never observed outpoint %s in status %s",
		outpoint, target.String())

	return matched
}

// outpointSet converts a VTXO slice into a membership set keyed by outpoint.
func outpointSet(vtxos []*daemonrpc.VTXO) map[string]struct{} {
	set := make(map[string]struct{}, len(vtxos))
	for _, vtxo := range vtxos {
		set[vtxo.Outpoint] = struct{}{}
	}

	return set
}

// waitForNewLiveVTXOWithAmount waits until a previously unseen live VTXO with
// the requested amount appears.
func waitForNewLiveVTXOWithAmount(t *testing.T,
	client daemonrpc.DaemonServiceClient,
	knownOutpoints map[string]struct{},
	amountSat int64) *daemonrpc.VTXO {

	t.Helper()

	var matched *daemonrpc.VTXO
	require.Eventually(t, func() bool {
		for _, vtxo := range listLiveVTXOs(t, client) {
			if _, known := knownOutpoints[vtxo.Outpoint]; known {
				continue
			}

			if vtxo.AmountSat == amountSat {
				matched = vtxo

				return true
			}
		}

		return false
	}, defaultTimeout, pollInterval,
		"never observed a new live VTXO with amount %d", amountSat)

	return matched
}

// waitForNewLiveVTXOsInRound polls the daemon's live VTXO set until at least
// expectedCount VTXOs tagged with roundID are visible, then returns the full
// set of matching VTXOs. Used by multi-input refresh tests where the output
// count is the assertion target rather than a per-output amount.
func waitForNewLiveVTXOsInRound(t *testing.T,
	client daemonrpc.DaemonServiceClient, roundID string,
	expectedCount int) []*daemonrpc.VTXO {

	t.Helper()

	var matched []*daemonrpc.VTXO
	require.Eventually(t, func() bool {
		matched = matched[:0]
		for _, vtxo := range listLiveVTXOs(t, client) {
			if vtxo.RoundId != roundID {
				continue
			}
			matched = append(matched, vtxo)
		}

		return len(matched) >= expectedCount
	}, defaultTimeout, pollInterval,
		"never observed %d live VTXOs in round %q",
		expectedCount, roundID)

	return matched
}

// waitForIndexedVTXOByPkScript waits until the daemon's authoritative indexer
// lookup returns one VTXO for the given pkScript and lifecycle status.
func waitForIndexedVTXOByPkScript(t *testing.T,
	client daemonrpc.DaemonServiceClient, pkScript []byte,
	target daemonrpc.VTXOStatus) *daemonrpc.VTXO {

	t.Helper()

	req := &daemonrpc.GetIndexedVTXOByPkScriptRequest{
		PkScript: append([]byte(nil), pkScript...),
		StatusFilter: []daemonrpc.VTXOStatus{
			target,
		},
	}

	var matched *daemonrpc.VTXO
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.GetIndexedVTXOByPkScript(ctx, req)
		if err != nil || resp.GetVtxo() == nil {
			return false
		}

		// Keep a defensive status check in case a backend ignores the
		// StatusFilter and returns a mismatched indexed entry.
		if resp.GetVtxo().Status != target {
			return false
		}

		matched = resp.GetVtxo()

		return true
	}, defaultTimeout, pollInterval,
		"never observed indexed pkScript in status %s",
		target.String())

	return matched
}

// waitForVTXOBalance waits until the daemon reports at least the requested
// VTXO balance.
func waitForVTXOBalance(t *testing.T, client daemonrpc.DaemonServiceClient,
	minVTXOBalanceSat int64) *daemonrpc.GetBalanceResponse {

	t.Helper()

	var lastResp *daemonrpc.GetBalanceResponse
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.GetBalance(
			ctx, &daemonrpc.GetBalanceRequest{},
		)
		if err != nil {
			return false
		}

		lastResp = resp

		return resp.VtxoBalanceSat >= minVTXOBalanceSat
	}, defaultTimeout, pollInterval,
		"VTXO balance never reached %d sats", minVTXOBalanceSat)

	return lastResp
}

// waitForExactVTXOBalance waits until the daemon reports exactly the requested
// VTXO balance.
func waitForExactVTXOBalance(t *testing.T, client daemonrpc.DaemonServiceClient,
	expectedVTXOBalanceSat int64) *daemonrpc.GetBalanceResponse {

	t.Helper()

	var lastResp *daemonrpc.GetBalanceResponse
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.GetBalance(
			ctx, &daemonrpc.GetBalanceRequest{},
		)
		if err != nil {
			return false
		}

		lastResp = resp

		return resp.VtxoBalanceSat == expectedVTXOBalanceSat
	}, defaultTimeout, pollInterval,
		"VTXO balance never reached exact value %d sats",
		expectedVTXOBalanceSat)

	return lastResp
}

// waitForVTXOBalanceBelow waits until the daemon reports a VTXO balance below
// the given exclusive upper bound.
func waitForVTXOBalanceBelow(t *testing.T, client daemonrpc.DaemonServiceClient,
	maxExclusiveVTXOBalanceSat int64) *daemonrpc.GetBalanceResponse {

	t.Helper()

	var lastResp *daemonrpc.GetBalanceResponse
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.GetBalance(
			ctx, &daemonrpc.GetBalanceRequest{},
		)
		if err != nil {
			return false
		}

		lastResp = resp

		return resp.VtxoBalanceSat < maxExclusiveVTXOBalanceSat
	}, defaultTimeout, pollInterval,
		"VTXO balance never dropped below %d sats",
		maxExclusiveVTXOBalanceSat)

	return lastResp
}

// confirmedWalletUTXOValues returns the current set of confirmed backing
// wallet UTXOs keyed by outpoint. Works for all wallet backends.
func confirmedWalletUTXOValues(t *testing.T,
	daemon *harness.ClientDaemonHarness) map[wire.OutPoint]btcutil.Amount {

	t.Helper()

	ctx, cancel := context.WithTimeout(
		t.Context(), defaultSmallTimeout,
	)
	defer cancel()

	utxos, err := daemon.ListWalletUnspent(ctx, 1, 9999999)
	require.NoError(t, err, "list confirmed wallet UTXOs")

	result := make(map[wire.OutPoint]btcutil.Amount, len(utxos))
	for _, utxo := range utxos {
		result[utxo.Outpoint] = utxo.Amount
	}

	return result
}

// waitForNewConfirmedWalletUTXOWithMaxValue waits until the client's wallet
// has a new confirmed UTXO not present in the baseline and whose value is at
// most maxValueSat. For unroll itests, this identifies the swept VTXO output
// rather than the much larger CPFP change output.
func waitForNewConfirmedWalletUTXOWithMaxValue(t *testing.T,
	daemon *harness.ClientDaemonHarness,
	baseline map[wire.OutPoint]btcutil.Amount,
	maxValueSat int64) wire.OutPoint {

	t.Helper()

	var found wire.OutPoint
	var foundValue btcutil.Amount

	require.Eventually(t, func() bool {
		current := confirmedWalletUTXOValues(t, daemon)
		for outpoint, value := range current {
			if _, ok := baseline[outpoint]; ok {
				continue
			}

			if int64(value) <= 0 || int64(value) > maxValueSat {
				continue
			}

			found = outpoint
			foundValue = value

			return true
		}

		return false
	}, defaultTimeout, pollInterval,
		"no new confirmed wallet UTXO at or below %d sats appeared",
		maxValueSat)

	t.Logf(
		"Detected swept wallet UTXO: outpoint=%s amount=%d",
		found.String(), foundValue,
	)

	return found
}

// waitForDaemonInfoReachable waits until the daemon's GetInfo RPC succeeds.
// This is useful after operator restarts to force mailbox reconnect/restore
// without assuming any specific connectivity flag semantics.
func waitForDaemonInfoReachable(t *testing.T,
	client daemonrpc.DaemonServiceClient) *daemonrpc.GetInfoResponse {

	t.Helper()

	var lastResp *daemonrpc.GetInfoResponse
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.GetInfo(
			ctx, &daemonrpc.GetInfoRequest{},
		)
		if err != nil {
			return false
		}

		lastResp = resp

		return true
	}, defaultTimeout, pollInterval,
		"daemon GetInfo never succeeded")

	return lastResp
}

// boardClientAndConfirmRound drives a real client daemon through boarding,
// round broadcast, block generation, confirmation, and live VTXO
// materialization.
func boardClientAndConfirmRound(t *testing.T, h *harness.ArkHarness,
	client daemonrpc.DaemonServiceClient, minConfirmations uint32,
	boardingAmount btcutil.Amount) (*daemonrpc.RoundInfo, *daemonrpc.VTXO,
	*daemonrpc.GetBalanceResponse) {

	t.Helper()

	existingRoundIDs := snapshotClientRoundIDs(t, client)

	newAddrResp, err := client.NewAddress(
		t.Context(), &daemonrpc.NewAddressRequest{},
	)
	require.NoError(t, err, "NewAddress RPC failed")
	require.NotEmpty(
		t, newAddrResp.Address, "boarding address should be set",
	)

	fundingTxID := h.Faucet(newAddrResp.Address, boardingAmount)
	t.Logf("Funded boarding address via txid=%s", fundingTxID)

	// Mine one extra block beyond the advertised minimum so both the
	// client wallet view and the operator's direct bitcoind validation
	// path observe the funding transaction before JoinRound runs.
	h.Generate(int(minConfirmations) + 1)

	balance := waitForConfirmedBoardingBalance(
		t, client, int64(boardingAmount),
	)
	t.Logf(
		"Client detected confirmed boarding balance=%d sats",
		balance.BoardingConfirmedSat,
	)

	boardResp := waitForBoardRegistered(t, client)
	require.Equal(t, "registered", boardResp.Status)

	joinedRound := waitForNewClientRoundState(
		t, client, existingRoundIDs,
		daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	require.NotEmpty(
		t, joinedRound.RoundId,
		"joined client round should have a concrete round id",
	)
	require.False(
		t, joinedRound.IsTemp,
		"joined client round should no longer be temporary",
	)

	waitForNamedClientRoundState(
		t, client, joinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)
	waitForPersistedClientRoundState(
		t, client, joinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
	)

	broadcastRound := waitForOperatorRoundStatus(
		t, h, joinedRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)
	t.Logf(
		"Round transaction broadcast: round_id=%q txid=%s",
		joinedRound.RoundId, broadcastRound.TxId,
	)

	mineUntilOperatorRoundConfirmed(
		t, h, joinedRound.RoundId, broadcastRound.TxId,
	)
	t.Logf(
		"Mined blocks until round confirmed: round_id=%q",
		joinedRound.RoundId,
	)

	confirmedRound := waitForNamedClientRoundState(
		t, client, joinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_CONFIRMED,
	)
	require.False(
		t, confirmedRound.IsTemp, "confirmed round should be persisted",
	)

	waitForOperatorRoundStatus(
		t, h, joinedRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
	)
	t.Logf(
		"Operator marked round confirmed: round_id=%q",
		joinedRound.RoundId,
	)

	liveVTXO := waitForLiveVTXO(t, client, joinedRound.RoundId)
	finalBalance := waitForVTXOBalance(t, client, liveVTXO.AmountSat)

	return joinedRound, liveVTXO, finalBalance
}
