package vtxo

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestJoinedRefreshExpiryRejectsForfeitCollection advances a reserved input
// into critical expiry after its round joined. The real child and manager
// must deliver a refusal that stops collection without a timeout, checkpoint
// or server signature submission. Rollback must leave the exit intact.
func TestJoinedRefreshExpiryRejectsForfeitCollection(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()
	desc.BatchExpiry = 1000
	h.withExpiryConfig(&ExpiryConfig{
		RefreshThresholdBlocks:  200,
		CriticalThresholdBlocks: 50,
		TreeDepthMultiplier:     1,
	})
	managerRef := newMockManagerRef(t)
	resolver := &cancelSensitiveChainResolverRef{
		seen: make(chan error, 1),
	}
	child := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO: desc, Store: h.store, Manager: managerRef,
			ChainResolver: resolver,
		},
		state: &PendingForfeitState{
			VTXO: desc, RequestedAtHeight: 800,
		},
		env: h.env,
	}
	id := round.RoundID(uuid.New())
	peer := h.newTestDescriptor()
	peerChild := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:  peer,
			Store: h.store,
		},
		state: &ForfeitingState{
			VTXO: peer, NewRoundID: id.String(),
			LastCheckedHeight: 100,
			ForfeitTxID: chainhash.HashH(
				[]byte("local-forfeit"),
			),
			ForfeitTx: wire.NewMsgTx(2),
		},
		env: h.env,
	}
	requested := &round.IntentSentState{
		Intents: round.Intents{
			Forfeits: []types.ForfeitRequest{{
				VTXOOutpoint: &desc.Outpoint,
			}, {
				VTXOOutpoint: &peer.Outpoint,
			}},
		},
	}

	clientEnv := &round.ClientEnvironment{Log: btclog.Disabled}
	admission, err := requested.ProcessEvent(
		h.ctx, &round.RoundJoined{
			RoundID: id,
			AcceptedVTXOOutpoints: []wire.OutPoint{
				desc.Outpoint, peer.Outpoint,
			},
		},
		clientEnv,
	)
	require.NoError(t, err)
	joined, ok := admission.NextState.(*round.IntentSentState)
	require.True(t, ok)
	require.Equal(t, id, joined.AdmittedRoundID)

	// The join has already happened. Critical expiry now preempts the
	// reservation, before the round requests input signatures.
	h.store.On(
		"UpdateVTXOStatus", h.ctx, desc.Outpoint,
		VTXOStatusUnilateralExit,
	).Return(nil).Once()
	_, err = child.Receive(h.ctx, &BlockEpochEvent{Height: 970}).Unpack()
	require.NoError(t, err)
	require.IsType(t, &UnilateralExitState{}, child.state)
	select {
	case err := <-resolver.seen:
		require.NoError(t, err)

	case <-time.After(2 * time.Second):
		t.Fatal("critical expiry did not start an exit")
	}

	_, err = child.Receive(h.ctx, &round.ForfeitRequestEvent{
		RoundID: joined.AdmittedRoundID.String(),
	}).Unpack()
	require.ErrorIs(t, err, ErrExitInFlight)
	require.IsType(t, &UnilateralExitState{}, child.state)

	// Drive the actual manager relay. No signature, signing-wallet call
	// or state write may be substituted for the rejection.
	messages := managerRef.getMessages()
	require.Len(t, messages, 1)
	roundRef := newMockRoundActorRef(t)
	manager := &Manager{
		cfg: &ManagerConfig{
			RoundActor: roundRef,
		},
		actors: map[wire.OutPoint]VTXOActorRef{
			desc.Outpoint: &inlineVTXOActorRef{
				actor: child,
			},
			peer.Outpoint: &inlineVTXOActorRef{
				actor: peerChild,
			},
		},
	}
	_, err = manager.Receive(h.ctx, messages[0]).Unpack()
	require.NoError(t, err)
	forwarded := roundRef.getMessages()
	require.Len(t, forwarded, 1)
	rejection, ok := forwarded[0].(*round.ForfeitSignatureRejected)
	require.True(t, ok)
	require.Equal(t, id.String(), rejection.RoundID)
	require.Equal(t, desc.Outpoint, rejection.VTXOOutpoint)

	// Skip the unrelated tree-signing ceremony, then deliver the same
	// request-scoped result to its real collecting-state transition.
	collected := map[wire.OutPoint]*round.ForfeitSignatureResponse{
		peer.Outpoint: {
			RoundID:      id.String(),
			VTXOOutpoint: peer.Outpoint,
		},
	}
	collecting := &round.ForfeitSignaturesCollectingState{
		RoundID: id, Intents: joined.Intents,
		ExpectedForfeits: map[wire.OutPoint]*round.ConnectorLeafInfo{
			desc.Outpoint: {}, peer.Outpoint: {},
		},
		CollectedForfeits: collected,
	}
	transition, err := collecting.ProcessEvent(
		h.ctx, rejection, &round.ClientEnvironment{
			Log: btclog.Disabled,
		},
	)
	require.NoError(t, err)
	require.IsType(t, &round.ClientFailedState{}, transition.NextState)
	outbox := transition.NewEvents.UnwrapOr(
		round.ClientEmittedEvent{},
	).Outbox
	require.Len(t, outbox, 3)
	release, ok := outbox[0].(*round.ReleaseForfeitReservation)
	require.True(t, ok)
	require.Equal(t, id.String(), release.RoundID)
	require.ElementsMatch(
		t, []wire.OutPoint{desc.Outpoint, peer.Outpoint},
		release.Outpoints,
	)
	require.IsType(t, &round.RoundFailedNotification{}, outbox[1])
	require.IsType(t, &round.CancelTimeoutReq{}, outbox[2])

	h.store.On(
		"UpdateVTXOStatus", mock.Anything, peer.Outpoint,
		VTXOStatusLive,
	).Return(nil).Once()
	_, err = manager.Receive(h.ctx, &actormsg.ReleaseForfeitRequest{
		RoundID: release.RoundID, Outpoints: release.Outpoints,
	}).Unpack()
	require.NoError(t, err)
	require.IsType(t, &UnilateralExitState{}, child.state)
	require.IsType(t, &LiveState{}, peerChild.state)
	h.store.AssertExpectations(t)
}
