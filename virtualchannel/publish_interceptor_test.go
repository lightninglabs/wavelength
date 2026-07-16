package virtualchannel

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

type fakeMaterializationStore struct {
	mu        sync.Mutex
	byPoint   map[wire.OutPoint]*Channel
	byTxID    map[chainhash.Hash][]*Channel
	byPending map[PendingChannelID]*PendingOpen
	marked    []ID
	copyReads bool
}

func (f *fakeMaterializationStore) FindVirtualChannelByChannelPoint(
	_ context.Context, channelPoint wire.OutPoint) (*Channel, bool, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	channel, ok := f.byPoint[channelPoint]
	if ok && f.copyReads {
		cloned := *channel
		channel = &cloned
	}

	return channel, ok, nil
}

func (f *fakeMaterializationStore) ListVirtualChannelsByFundingTxID(
	_ context.Context, txid chainhash.Hash) ([]*Channel, error) {

	return f.byTxID[txid], nil
}

func (f *fakeMaterializationStore) ListVirtualChannelsByStatus(
	_ context.Context, _ Status) ([]*Channel, error) {

	return nil, nil
}

func (f *fakeMaterializationStore) FindVirtualChannelPendingOpen(
	_ context.Context, pendingID PendingChannelID) (*PendingOpen, bool,
	error) {

	pending, ok := f.byPending[pendingID]

	return pending, ok, nil
}

func (f *fakeMaterializationStore) MarkVirtualChannelMaterializing(
	_ context.Context, id ID) (bool, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.marked = append(f.marked, id)

	return true, nil
}

func (f *fakeMaterializationStore) MarkVirtualChannelFundingVerified(
	_ context.Context, id ID) (bool, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	for _, channel := range f.byPoint {
		if channel.ID != id || channel.Status != StatusLNDNegotiating {
			continue
		}

		channel.Status = StatusFundingVerified

		return true, nil
	}

	return false, nil
}

func (f *fakeMaterializationStore) GetVirtualChannel(_ context.Context, id ID) (
	*Channel, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	for _, channel := range f.byPoint {
		if channel.ID == id {
			if f.copyReads {
				cloned := *channel

				return &cloned, nil
			}

			return channel, nil
		}
	}

	return nil, fmt.Errorf("channel %x not found", id)
}

type fakeBackingMaterializer struct {
	channels []*Channel
}

func (f *fakeBackingMaterializer) MaterializeVirtualChannelBacking(
	_ context.Context, channel *Channel) error {

	f.channels = append(f.channels, channel)

	return nil
}

// TestMaterializingPublishInterceptorCallsMaterializerFirst verifies that the
// lnd child publish callback waits for the configured ancestry materializer.
func TestMaterializingPublishInterceptorCallsMaterializerFirst(t *testing.T) {
	t.Parallel()

	channelPoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("virtual-materializer")),
		Index: 0,
	}
	channel := &Channel{
		Registration: Registration{
			ID:     fixedID(9),
			Status: StatusActive,
		},
	}
	store := &fakeMaterializationStore{
		byPoint: map[wire.OutPoint]*Channel{
			channelPoint: channel,
		},
		byTxID: make(map[chainhash.Hash][]*Channel),
	}
	materializer := &fakeBackingMaterializer{}
	interceptor, err := NewMaterializingPublishInterceptor(
		MaterializingPublishInterceptorConfig{
			Store:        store,
			Materializer: materializer,
		},
	)
	require.NoError(t, err)

	childTx := wire.NewMsgTx(2)
	childTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: channelPoint,
	})

	publishedChild := false
	err = interceptor.PublishTransaction(
		childTx, "lnd close",
		func() error {
			require.Equal(
				t, []*Channel{channel}, materializer.channels,
			)
			require.Equal(t, []ID{channel.ID}, store.marked)
			publishedChild = true

			return nil
		},
	)
	require.NoError(t, err)
	require.True(t, publishedChild)
}

// TestMaterializingPublishInterceptorIgnoresPlainTx verifies normal lnd
// wallet publication is unchanged when no input spends a virtual channel.
func TestMaterializingPublishInterceptorIgnoresPlainTx(t *testing.T) {
	t.Parallel()

	store := &fakeMaterializationStore{
		byPoint: make(map[wire.OutPoint]*Channel),
		byTxID:  make(map[chainhash.Hash][]*Channel),
	}
	materializer := &fakeBackingMaterializer{}
	interceptor, err := NewMaterializingPublishInterceptor(
		MaterializingPublishInterceptorConfig{
			Store:        store,
			Materializer: materializer,
		},
	)
	require.NoError(t, err)

	called := false
	err = interceptor.PublishTransaction(
		wire.NewMsgTx(2), "plain", func() error {
			called = true

			return nil
		},
	)
	require.NoError(t, err)
	require.True(t, called)
	require.Empty(t, materializer.channels)
	require.Empty(t, store.marked)
}

// TestMaterializingPublishInterceptorSuppressesVirtualFundingPublish verifies
// lnd funding rebroadcasts do not unroll active virtual channels.
func TestMaterializingPublishInterceptorSuppressesVirtualFundingPublish(
	t *testing.T) {

	t.Parallel()

	backingTx := wire.NewMsgTx(2)
	backingTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: []byte{0x51},
	})

	store := &fakeMaterializationStore{
		byPoint: make(map[wire.OutPoint]*Channel),
		byTxID: map[chainhash.Hash][]*Channel{
			backingTx.TxHash(): {
				{
					Registration: Registration{
						ID:        fixedID(4),
						Status:    StatusActive,
						BackingTx: backingTx,
					},
				},
			},
		},
	}
	materializer := &fakeBackingMaterializer{}
	interceptor, err := NewMaterializingPublishInterceptor(
		MaterializingPublishInterceptorConfig{
			Store:        store,
			Materializer: materializer,
		},
	)
	require.NoError(t, err)

	called := false
	err = interceptor.PublishTransaction(
		backingTx, "funding",
		func() error {
			called = true

			return nil
		},
	)
	require.NoError(t, err)
	require.False(t, called)
	require.Empty(t, materializer.channels)
	require.Empty(t, store.marked)
}

// TestMaterializingPublishInterceptorAllowsClaimedFundingPublish verifies the
// materializer can relay the parent through the same integrated lnd hook after
// the channel FSM has durably claimed publication.
func TestMaterializingPublishInterceptorAllowsClaimedFundingPublish(
	t *testing.T) {

	t.Parallel()

	backingTx := wire.NewMsgTx(2)
	backingTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: []byte{0x51},
	})

	store := &fakeMaterializationStore{
		byPoint: make(map[wire.OutPoint]*Channel),
		byTxID: map[chainhash.Hash][]*Channel{
			backingTx.TxHash(): {
				{
					Registration: Registration{
						ID:        fixedID(6),
						Status:    StatusMaterializing,
						BackingTx: backingTx,
					},
				},
			},
		},
	}
	interceptor, err := NewMaterializingPublishInterceptor(
		MaterializingPublishInterceptorConfig{
			Store:        store,
			Materializer: &fakeBackingMaterializer{},
		},
	)
	require.NoError(t, err)

	called := false
	err = interceptor.PublishTransaction(
		backingTx, "materialized funding", func() error {
			called = true

			return nil
		},
	)
	require.NoError(t, err)
	require.True(t, called)
}

// TestVirtualChannelPublishStatusGates pins the lifecycle states that may
// publish a virtual funding transaction or a child that spends it.
func TestVirtualChannelPublishStatusGates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		status          Status
		suppressFunding bool
		materialize     bool
	}{
		{
			name:   "requested",
			status: StatusRequested,
		},
		{
			name:   "funding bound",
			status: StatusFundingBound,
		},
		{
			name:            "lnd negotiating",
			status:          StatusLNDNegotiating,
			suppressFunding: true,
		},
		{
			name:            "funding verified",
			status:          StatusFundingVerified,
			suppressFunding: true,
		},
		{
			name:            "backing armed",
			status:          StatusBackingArmed,
			suppressFunding: true,
			materialize:     true,
		},
		{
			name:            "round confirmed",
			status:          StatusRoundConfirmed,
			suppressFunding: true,
			materialize:     true,
		},
		{
			name:            "active",
			status:          StatusActive,
			suppressFunding: true,
			materialize:     true,
		},
		{
			name:   "funding published",
			status: StatusFundingPublished,
		},
		{
			name:        "closing",
			status:      StatusClosing,
			materialize: true,
		},
		{
			name:        "materializing",
			status:      StatusMaterializing,
			materialize: true,
		},
		{
			name:   "closed",
			status: StatusClosed,
		},
		{
			name:   "failed",
			status: StatusFailed,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, test.suppressFunding,
				shouldSuppressFundingPublish(test.status),
			)
			require.Equal(
				t, test.materialize,
				shouldMaterialize(test.status),
			)
		})
	}
}

// TestBuildAuxComponentsInstallsPublishInterceptor verifies that integrated
// lnd startup can receive the virtual channel publish hook through
// AuxComponents.
func TestBuildAuxComponentsInstallsPublishInterceptor(t *testing.T) {
	t.Parallel()

	backingTx := wire.NewMsgTx(2)
	backingTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: []byte{0x51},
	})

	store := &fakeMaterializationStore{
		byPoint: make(map[wire.OutPoint]*Channel),
		byTxID: map[chainhash.Hash][]*Channel{
			backingTx.TxHash(): {
				{
					Registration: Registration{
						ID:        fixedID(5),
						Status:    StatusActive,
						BackingTx: backingTx,
					},
				},
			},
		},
	}
	components, err := BuildAuxComponents(
		MaterializingPublishInterceptorConfig{
			Store:        store,
			Materializer: &fakeBackingMaterializer{},
		},
	)
	require.NoError(t, err)
	require.True(t, components.PublishInterceptor.IsSome())
	require.True(t, components.ChannelActivationGate.IsSome())

	called := false
	interceptor := components.PublishInterceptor.UnsafeFromSome()
	err = interceptor.PublishTransaction(
		backingTx, "funding", func() error {
			called = true

			return nil
		},
	)
	require.NoError(t, err)
	require.False(t, called)
}

// fixedID returns a deterministic virtual channel id for tests.
func fixedID(fill byte) ID {
	var id ID
	for i := range id {
		id[i] = fill
	}

	return id
}
