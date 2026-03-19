//go:build itest

package itest

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultSmallTimeout = 5 * time.Second
	defaultTimeout      = 60 * time.Second
	pollInterval        = 200 * time.Millisecond
	confirmationGrace   = 2 * time.Second
)

// getOperatorInfo fetches the public operator info over the real client RPC
// surface exposed by the in-process operator.
func getOperatorInfo(t *testing.T,
	h *harness.ArkHarness) *arkrpc.GetInfoResponse {

	t.Helper()

	conn, err := grpc.Dial(
		h.ArkRPCAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
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
func waitForClientRoundState(t *testing.T,
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

// roundStateSatisfiesTarget tolerates short-lived intermediate states that can
// be missed by polling on fast CI runners.
func roundStateSatisfiesTarget(state,
	target daemonrpc.RoundState) bool {

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
			if round.Id == roundID && round.Status == target {
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

// operatorRoundHasStatus reports whether the operator currently exposes the
// given round status for the specified round ID.
func operatorRoundHasStatus(t *testing.T, h *harness.ArkHarness,
	roundID string, target adminrpc.RoundStatus) bool {

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
		if round.Id == roundID && round.Status == target {
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
			ctx, &daemonrpc.ListVTXOsRequest{},
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
func waitForExactVTXOBalance(t *testing.T,
	client daemonrpc.DaemonServiceClient,
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
func waitForVTXOBalanceBelow(t *testing.T,
	client daemonrpc.DaemonServiceClient,
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
	boardingAmount btcutil.Amount) (
	*daemonrpc.RoundInfo, *daemonrpc.VTXO, *daemonrpc.GetBalanceResponse) {

	t.Helper()

	existingRoundIDs := snapshotClientRoundIDs(t, client)

	newAddrResp, err := client.NewAddress(
		t.Context(), &daemonrpc.NewAddressRequest{},
	)
	require.NoError(t, err, "NewAddress RPC failed")
	require.NotEmpty(t, newAddrResp.Address,
		"boarding address should be set")

	fundingTxID := h.Faucet(newAddrResp.Address, boardingAmount)
	t.Logf("Funded boarding address via txid=%s", fundingTxID)

	// Mine one extra block beyond the advertised minimum so both the
	// client wallet view and the operator's direct bitcoind validation
	// path observe the funding transaction before JoinRound runs.
	h.Generate(int(minConfirmations) + 1)

	balance := waitForConfirmedBoardingBalance(
		t, client, int64(boardingAmount),
	)
	t.Logf("Client detected confirmed boarding balance=%d sats",
		balance.BoardingConfirmedSat)

	boardResp := waitForBoardRegistered(t, client)
	require.Equal(t, "registered", boardResp.Status)

	joinedRound := waitForNewClientRoundState(
		t, client, existingRoundIDs,
		daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	require.NotEmpty(t, joinedRound.RoundId,
		"joined client round should have a concrete round id")
	require.False(t, joinedRound.IsTemp,
		"joined client round should no longer be temporary")

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
	t.Logf("Round transaction broadcast: round_id=%q txid=%s",
		joinedRound.RoundId, broadcastRound.TxId)

	mineUntilOperatorRoundConfirmed(
		t, h, joinedRound.RoundId, broadcastRound.TxId,
	)
	t.Logf("Mined blocks until round confirmed: round_id=%q",
		joinedRound.RoundId)

	confirmedRound := waitForNamedClientRoundState(
		t, client, joinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_CONFIRMED,
	)
	require.False(t, confirmedRound.IsTemp,
		"confirmed round should be persisted")

	waitForOperatorRoundStatus(
		t, h, joinedRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
	)
	t.Logf("Operator marked round confirmed: round_id=%q",
		joinedRound.RoundId)

	liveVTXO := waitForLiveVTXO(t, client, joinedRound.RoundId)
	finalBalance := waitForVTXOBalance(t, client, liveVTXO.AmountSat)

	return joinedRound, liveVTXO, finalBalance
}
