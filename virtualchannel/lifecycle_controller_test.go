package virtualchannel

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/stretchr/testify/require"
)

type flakyActivationLookup struct {
	point wire.OutPoint
	calls atomic.Int32
}

func (f *flakyActivationLookup) FindVirtualChannelByChannelPoint(
	context.Context, wire.OutPoint) (*Channel, bool, error) {

	if f.calls.Add(1) == 1 {
		return nil, false, errors.New("temporary read failure")
	}

	return &Channel{Registration: Registration{
		ID: fixedID(50), Status: StatusActive, ChannelPoint: f.point,
	}}, true, nil
}

func (f *flakyActivationLookup) ListVirtualChannelsByFundingTxID(
	context.Context, chainhash.Hash) ([]*Channel, error) {

	return nil, nil
}

func (f *flakyActivationLookup) FindVirtualChannelPendingOpen(context.Context,
	PendingChannelID) (*PendingOpen, bool, error) {

	return nil, false, nil
}

func (f *flakyActivationLookup) MarkVirtualChannelFundingVerified(
	context.Context, ID) (bool, error) {

	return false, nil
}

func TestLifecycleActivationGate(t *testing.T) {
	t.Parallel()

	armedPoint := wire.OutPoint{
		Hash: chainhash.HashH([]byte("armed-channel")), Index: 0,
	}
	activePoint := wire.OutPoint{
		Hash: chainhash.HashH([]byte("active-channel")), Index: 0,
	}
	negotiatingPoint := wire.OutPoint{
		Hash: chainhash.HashH([]byte("negotiating-channel")), Index: 0,
	}
	pendingID := fixedPendingID(42)
	store := &fakeMaterializationStore{
		copyReads: true,
		byPending: map[PendingChannelID]*PendingOpen{
			pendingID: {
				PendingChannelID: pendingID,
				Status:           StatusLNDNegotiating,
			},
		},
		byPoint: map[wire.OutPoint]*Channel{
			armedPoint: {
				Registration: Registration{
					ID:     fixedID(40),
					Status: StatusBackingArmed,
				},
			},
			activePoint: {
				Registration: Registration{
					ID: fixedID(41), Status: StatusActive,
				},
			},
			negotiatingPoint: {
				Registration: Registration{
					ID:     fixedID(43),
					Status: StatusLNDNegotiating,
				},
			},
		},
	}
	gate, err := NewLifecycleActivationGate(store)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	err = gate.WaitForActivation(ctx, funding.ChannelActivationRequest{
		FundingOutpoint: armedPoint,
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	pendingCtx, pendingCancel := context.WithTimeout(
		t.Context(), 25*time.Millisecond,
	)
	defer pendingCancel()
	err = gate.WaitForActivation(pendingCtx,
		funding.ChannelActivationRequest{
			PendingChanID: funding.PendingChanID(pendingID),
		})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(
		t,
		gate.WaitForActivation(
			t.Context(), funding.ChannelActivationRequest{
				FundingOutpoint: activePoint,
			},
		),
	)
	require.NoError(
		t,
		gate.WaitForActivation(
			t.Context(), funding.ChannelActivationRequest{
				FundingOutpoint: wire.OutPoint{
					Hash: chainhash.HashH(
						[]byte("plain-channel"),
					),
					Index: 0,
				},
			},
		),
	)

	activationErr := make(chan error, 1)
	go func() {
		activationErr <- gate.WaitForActivation(
			t.Context(), funding.ChannelActivationRequest{
				FundingOutpoint: negotiatingPoint,
			},
		)
	}()
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()

		return store.byPoint[negotiatingPoint].Status ==
			StatusFundingVerified
	}, time.Second, 10*time.Millisecond)
	store.mu.Lock()
	store.byPoint[negotiatingPoint].Status = StatusActive
	store.mu.Unlock()
	require.NoError(t, <-activationErr)
}

func TestLifecycleActivationGateRetriesStoreFailure(t *testing.T) {
	t.Parallel()

	point := wire.OutPoint{Hash: chainhash.HashH([]byte("flaky-channel"))}
	store := &flakyActivationLookup{point: point}
	gate, err := NewLifecycleActivationGate(store)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(
		t,
		gate.WaitForActivation(
			ctx, funding.ChannelActivationRequest{
				FundingOutpoint: point,
			},
		),
	)
	require.GreaterOrEqual(t, store.calls.Load(), int32(2))
}
