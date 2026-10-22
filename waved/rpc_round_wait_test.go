package waved

import (
	"testing"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestQueryRoundStatesBatchWait exposes an immutable selected window through
// the real actor-query projection, including its temporary identity and inputs.
func TestQueryRoundStatesBatchWait(t *testing.T) {
	t.Parallel()

	cutoff := time.Unix(1800000000, 0).UTC()
	input := wire.OutPoint{Index: 3}
	address, err := btcaddr.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	pending := &round.PendingRoundAssembly{
		BatchWait: round.ScheduledBatchWait{
			Opens:       cutoff.Add(-time.Minute),
			Cutoff:      cutoff,
			PlannedJoin: cutoff.Add(-45 * time.Second),
		},
		Boarding: []round.BoardingIntent{{
			BoardingIntent: wallet.BoardingIntent{
				Address: wallet.BoardingAddress{
					Address: address,
				},
				Outpoint: wire.OutPoint{
					Index: 2,
				},
			},
		}},
		Forfeits: []types.ForfeitRequest{
			{
				VTXOOutpoint: &input,
			},
		},
	}
	system := newRoundActorWithStates(t, map[string]round.FSMStateInfo{
		"pending-a": {State: pending, IsTemp: true},
	})
	defer func() {
		require.NoError(t, system.Shutdown(t.Context()))
	}()
	srv := &RPCServer{server: &Server{actorSystem: system}}
	rounds, err := srv.queryRoundStates(t.Context())
	require.NoError(t, err)
	require.Len(t, rounds, 1)
	got := rounds[0]
	require.Equal(t, "pending-a", got.RoundKey)
	require.Equal(
		t, "Waiting for scheduled batch registration",
		got.WaitingReason,
	)
	require.Equal(t, cutoff.Unix(), got.BatchWindow.CutoffUnix)
	require.Equal(
		t, cutoff.Add(-time.Minute).Unix(),
		got.BatchWindow.RegistrationOpensUnix,
	)
	require.Equal(
		t, cutoff.Add(-45*time.Second).UnixMilli(),
		got.BatchWindow.PlannedJoinUnixMs,
	)
	require.Equal(t, []string{
		pending.Boarding[0].Outpoint.String(), input.String(),
	}, got.InputOutpoints)
	require.Equal(
		t, []string{address.EncodeAddress()}, got.BoardingAddresses,
	)

	// The snapshot owns its protobuf, so callers cannot mutate the FSM.
	got.BatchWindow.CutoffUnix = 1
	require.Equal(t, cutoff, pending.BatchWait.Cutoff)
	assembling := &waverpc.RoundInfo{}
	populateRoundWait(assembling, &round.PendingRoundAssembly{})
	require.Equal(t, "Assembling round inputs", assembling.WaitingReason)
	require.Nil(t, assembling.BatchWindow)
	sent := &waverpc.RoundInfo{}
	populateRoundWait(sent, &round.IntentSentState{})
	require.Empty(t, sent.WaitingReason)
	require.Nil(t, sent.BatchWindow)
}

// TestWatchRoundWaitChanges covers same-state metadata updates, suppression of
// identical polls, and simultaneous temporary rounds that have no server ID.
func TestWatchRoundWaitChanges(t *testing.T) {
	t.Parallel()

	previous := make(map[string]*waverpc.RoundInfo)
	a := &waverpc.RoundInfo{RoundKey: "a", IsTemp: true,
		State: waverpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY}
	b := &waverpc.RoundInfo{}
	proto.Merge(b, a)
	b.RoundKey = "b"
	current := []*waverpc.RoundInfo{a, b}
	require.Len(t, changedRoundStatuses(previous, current), 2)
	require.Empty(t, changedRoundStatuses(previous, current))
	a.WaitingReason = "Waiting for scheduled batch registration"
	a.BatchWindow = &waverpc.ScheduledBatchWindow{CutoffUnix: 1800000000}
	require.Equal(
		t, []*waverpc.RoundInfo{a},
		changedRoundStatuses(previous, current),
	)
	require.Empty(t, changedRoundStatuses(previous, current))
	a.WaitingReason = ""
	a.BatchWindow = nil
	a.State = waverpc.RoundState_ROUND_STATE_REGISTRATION_SENT
	require.Equal(
		t, []*waverpc.RoundInfo{a},
		changedRoundStatuses(previous, current),
	)
	changedRoundStatuses(previous, []*waverpc.RoundInfo{a})
	require.NotContains(t, previous, "b")
}

// TestListRoundsPendingWaitsIgnorePageSize pins the live-round contract: SQL
// pagination must never hide a simultaneous pending registration.
func TestListRoundsPendingWaitsIgnorePageSize(t *testing.T) {
	t.Parallel()
	system := newRoundActorWithStates(
		t, map[string]round.FSMStateInfo{
			"pending-a": {
				State:  &round.PendingRoundAssembly{},
				IsTemp: true,
			},
			"pending-b": {
				State:  &round.PendingRoundAssembly{},
				IsTemp: true,
			},
		},
	)
	defer func() {
		require.NoError(t, system.Shutdown(t.Context()))
	}()
	srv := &RPCServer{server: &Server{actorSystem: system}}
	resp, err := srv.ListRounds(t.Context(), &waverpc.ListRoundsRequest{
		StateFilter: waverpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY,
		PageSize:    1,
	})
	require.NoError(t, err)
	require.Len(t, resp.GetRounds(), 2)
	keys := []string{resp.Rounds[0].RoundKey, resp.Rounds[1].RoundKey}
	require.ElementsMatch(t, []string{"pending-a", "pending-b"}, keys)
}
