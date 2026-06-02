package virtualchannel

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

type fakeMaterializationStore struct {
	byPoint   map[wire.OutPoint]*Channel
	byTxID    map[chainhash.Hash][]*Channel
	byPending map[PendingChannelID]*PendingOpen
	marked    []ID
}

func (f *fakeMaterializationStore) FindVirtualChannelByChannelPoint(
	_ context.Context, channelPoint wire.OutPoint) (*Channel, bool, error) {

	channel, ok := f.byPoint[channelPoint]

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

	f.marked = append(f.marked, id)

	return true, nil
}

type fakeBroadcaster struct {
	txs    []*wire.MsgTx
	labels []string
}

func (f *fakeBroadcaster) PublishTransaction(_ context.Context, tx *wire.MsgTx,
	label string) error {

	f.txs = append(f.txs, tx)
	f.labels = append(f.labels, label)

	return nil
}

type fakeBackingMaterializer struct {
	channels []*Channel
}

func (f *fakeBackingMaterializer) MaterializeVirtualChannelBacking(
	_ context.Context, channel *Channel) error {

	f.channels = append(f.channels, channel)

	return nil
}

type fakeCloseSettler struct {
	handled  bool
	channels []*Channel
	txs      []*wire.MsgTx
	labels   []string
}

func (f *fakeCloseSettler) SettleCooperativeClose(_ context.Context,
	channel *Channel, closeTx *wire.MsgTx, label string) (bool, error) {

	f.channels = append(f.channels, channel)
	f.txs = append(f.txs, closeTx)
	f.labels = append(f.labels, label)

	return f.handled, nil
}

// TestMaterializingPublishInterceptorPublishesParentFirst verifies that lnd's
// child publish callback only runs after the backing parent is published.
func TestMaterializingPublishInterceptorPublishesParentFirst(t *testing.T) {
	t.Parallel()

	channelPoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("virtual-funding")),
		Index: 0,
	}
	backingTx := wire.NewMsgTx(2)
	backingTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: []byte{0x51},
	})

	channelID := fixedID(1)
	store := &fakeMaterializationStore{
		byPoint: map[wire.OutPoint]*Channel{
			channelPoint: {
				Registration: Registration{
					ID:        channelID,
					Status:    StatusActive,
					BackingTx: backingTx,
				},
			},
		},
		byTxID: make(map[chainhash.Hash][]*Channel),
	}
	broadcaster := &fakeBroadcaster{}

	interceptor, err := NewMaterializingPublishInterceptor(
		MaterializingPublishInterceptorConfig{
			Store:       store,
			Broadcaster: broadcaster,
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
			require.Len(t, broadcaster.txs, 1)
			require.Equal(t, backingTx, broadcaster.txs[0])
			require.Equal(t, []ID{channelID}, store.marked)
			publishedChild = true

			return nil
		},
	)
	require.NoError(t, err)
	require.True(t, publishedChild)
	require.Equal(
		t, []string{MaterializedBackingLabel}, broadcaster.labels,
	)
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
	broadcaster := &fakeBroadcaster{}
	interceptor, err := NewMaterializingPublishInterceptor(
		MaterializingPublishInterceptorConfig{
			Store:       store,
			Broadcaster: broadcaster,
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
	require.Empty(t, broadcaster.txs)
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
	broadcaster := &fakeBroadcaster{}
	interceptor, err := NewMaterializingPublishInterceptor(
		MaterializingPublishInterceptorConfig{
			Store:       store,
			Broadcaster: broadcaster,
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
	require.Empty(t, broadcaster.txs)
	require.Empty(t, store.marked)
}

// TestMaterializingPublishInterceptorSettlesCooperativeClose verifies that a
// handled virtual cooperative close suppresses lnd's on-chain publish path.
func TestMaterializingPublishInterceptorSettlesCooperativeClose(t *testing.T) {
	t.Parallel()

	channelPoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("virtual-funding-close")),
		Index: 0,
	}
	backingTx := wire.NewMsgTx(2)
	backingTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: []byte{0x51},
	})

	channel := &Channel{
		Registration: Registration{
			ID:        fixedID(6),
			Status:    StatusActive,
			BackingTx: backingTx,
		},
	}
	store := &fakeMaterializationStore{
		byPoint: map[wire.OutPoint]*Channel{
			channelPoint: channel,
		},
		byTxID: make(map[chainhash.Hash][]*Channel),
	}
	broadcaster := &fakeBroadcaster{}
	settler := &fakeCloseSettler{
		handled: true,
	}
	interceptor, err := NewMaterializingPublishInterceptor(
		MaterializingPublishInterceptorConfig{
			Store:        store,
			Broadcaster:  broadcaster,
			CloseSettler: settler,
		},
	)
	require.NoError(t, err)

	closeTx := wire.NewMsgTx(2)
	closeTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: channelPoint,
	})

	called := false
	err = interceptor.PublishTransaction(
		closeTx, "close-channel",
		func() error {
			called = true

			return nil
		},
	)
	require.NoError(t, err)
	require.False(t, called)
	require.Empty(t, broadcaster.txs)
	require.Empty(t, store.marked)
	require.Equal(t, []*Channel{channel}, settler.channels)
	require.Equal(t, []*wire.MsgTx{closeTx}, settler.txs)
	require.Equal(t, []string{"close-channel"}, settler.labels)
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
			Store:       store,
			Broadcaster: &fakeBroadcaster{},
		},
	)
	require.NoError(t, err)
	require.True(t, components.PublishInterceptor.IsSome())

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
