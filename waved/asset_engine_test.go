package waved

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/tap-sdk/embedded"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/tapassets"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// fakeAssetChainBackend is a chain backend that serves headers and scripted
// confirmation events.
type fakeAssetChainBackend struct {
	chainsource.ChainBackend

	height       int32
	header       *wire.BlockHeader
	broadcastErr error
	broadcast    []*wire.MsgTx

	confirmed chan *chainsource.TxConfirmation
	reorged   chan uint64
	done      chan struct{}
	canceled  chan struct{}
}

// BestBlock returns the scripted best height.
func (b *fakeAssetChainBackend) BestBlock(context.Context) (int32,
	chainhash.Hash, error) {

	return b.height, chainhash.Hash{1}, nil
}

// BlockHeaderByHeight returns the scripted header.
func (b *fakeAssetChainBackend) BlockHeaderByHeight(context.Context, uint32) (
	*wire.BlockHeader, error) {

	return b.header, nil
}

// BroadcastTx records the transaction and returns the scripted error.
func (b *fakeAssetChainBackend) BroadcastTx(_ context.Context, tx *wire.MsgTx,
	_ string) error {

	b.broadcast = append(b.broadcast, tx)

	return b.broadcastErr
}

// RegisterConf returns a registration over the scripted event channels.
func (b *fakeAssetChainBackend) RegisterConf(context.Context, *chainhash.Hash,
	[]byte, uint32, uint32, bool) (*chainsource.ConfRegistration, error) {

	return &chainsource.ConfRegistration{
		Confirmed: b.confirmed,
		Reorged:   b.reorged,
		Done:      b.done,
		Cancel: func() {
			close(b.canceled)
		},
	}, nil
}

// RegisterBlocks returns a registration that delivers no block.
func (b *fakeAssetChainBackend) RegisterBlocks(context.Context) (
	*chainsource.BlockRegistration, error) {

	return &chainsource.BlockRegistration{
		Epochs: make(chan *chainsource.BlockEpoch),
		Cancel: func() {},
	}, nil
}

// headerlessChainBackend is a chain backend that serves no block headers.
type headerlessChainBackend struct {
	chainsource.ChainBackend
}

// newAssetChainTestServer returns a server whose wallet is ready on the
// given chain backend.
func newAssetChainTestServer(backend chainsource.ChainBackend) *Server {
	ready := make(chan struct{})
	close(ready)

	return &Server{
		cfg:          DefaultConfig(),
		log:          btclog.Disabled,
		walletReady:  ready,
		chainBackend: backend,
	}
}

// TestAssetChainNotReady refuses chain and key access before the wallet is
// unlocked.
func TestAssetChainNotReady(t *testing.T) {
	t.Parallel()

	server := &Server{
		cfg:         DefaultConfig(),
		walletReady: make(chan struct{}),
	}
	chain := &assetChain{server: server}

	_, _, err := chain.BestBlock(t.Context())
	require.ErrorIs(t, err, errAssetWalletNotReady)
	_, err = chain.BlockHeader(t.Context(), 1)
	require.ErrorIs(t, err, errAssetWalletNotReady)

	keys := &assetKeyRing{server: server}
	_, err = keys.DeriveNextKey(keychain.KeyFamily(212))
	require.ErrorIs(t, err, errAssetWalletNotReady)
	_, err = keys.DerivePrivKey(keychain.KeyDescriptor{})
	require.ErrorIs(t, err, errAssetWalletNotReady)
}

// TestAssetChainHeadersAndBroadcast reads the tip and headers through the
// chain backend and treats a transaction the network has as published.
func TestAssetChainHeadersAndBroadcast(t *testing.T) {
	t.Parallel()

	backend := &fakeAssetChainBackend{
		height: 120,
		header: &wire.BlockHeader{
			Nonce: 7,
		},
	}
	chain := &assetChain{server: newAssetChainTestServer(backend)}
	ctx := t.Context()

	height, hash, err := chain.BestBlock(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 120, height)
	require.Equal(t, chainhash.Hash{1}, hash)

	header, err := chain.BlockHeader(ctx, 100)
	require.NoError(t, err)
	require.EqualValues(t, 7, header.Nonce)

	tx := wire.NewMsgTx(2)
	require.NoError(t, chain.PublishTransaction(ctx, tx, "label"))

	backend.broadcastErr = errors.New("txn-already-in-mempool")
	require.NoError(t, chain.PublishTransaction(ctx, tx, "label"))

	backend.broadcastErr = errors.New("Transaction already in block chain")
	require.NoError(t, chain.PublishTransaction(ctx, tx, "label"))

	backend.broadcastErr = errors.New("bad-txns-inputs-missingorspent")
	require.Error(t, chain.PublishTransaction(ctx, tx, "label"))
	require.Len(t, backend.broadcast, 4)

	// A backend without a header source cannot verify proofs.
	headerless := &assetChain{
		server: newAssetChainTestServer(&headerlessChainBackend{}),
	}
	_, err = headerless.BlockHeader(ctx, 100)
	require.ErrorContains(t, err, "serves no block headers")
}

// newAssetConfBackend returns a chain backend with scripted confirmation
// events and its best block at height.
func newAssetConfBackend(height int32) *fakeAssetChainBackend {
	return &fakeAssetChainBackend{
		height:    height,
		confirmed: make(chan *chainsource.TxConfirmation, 1),
		reorged:   make(chan uint64, 1),
		done:      make(chan struct{}, 1),
		canceled:  make(chan struct{}),
	}
}

// newAssetConfTestChain returns the engine's chain view of a backend, with
// the chain source its confirmation watches go through.
func newAssetConfTestChain(t *testing.T,
	backend chainsource.ChainBackend) *assetChain {

	t.Helper()

	server := newAssetChainTestServer(backend)
	server.actorSystem = actor.NewActorSystem()
	t.Cleanup(func() {
		_ = server.actorSystem.Shutdown(context.Background())
	})
	server.registerChainSourceActor(t.Context())

	return &assetChain{server: server}
}

// assetTestConfirmation returns a backend confirmation at height, with its
// block when one is given.
func assetTestConfirmation(height uint32,
	block *wire.MsgBlock) *chainsource.TxConfirmation {

	return &chainsource.TxConfirmation{
		BlockHash: &chainhash.Hash{
			byte(height),
		},
		BlockHeight: height,
		Block:       block,
	}
}

// requireAssetConfSignal waits for a signal of a confirmation watch.
func requireAssetConfSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("event never arrived")
	}
}

// TestAssetChainConfirmationEvents forwards a confirmation with its block,
// a re-org and the final signal, and cancels the backend registration.
func TestAssetChainConfirmationEvents(t *testing.T) {
	t.Parallel()

	backend := newAssetConfBackend(100)
	chain := newAssetConfTestChain(t, backend)

	events, err := chain.RegisterConfirmation(
		t.Context(), chainhash.Hash{9}, []byte{0x51}, 100,
	)
	require.NoError(t, err)

	confirm := func(height uint32, seq uint64) {
		t.Helper()

		block := &wire.MsgBlock{Header: wire.BlockHeader{Nonce: 3}}
		conf := assetTestConfirmation(height, block)
		conf.Seq = seq
		backend.confirmed <- conf

		select {
		case conf := <-events.Confirmed:
			require.EqualValues(t, height, conf.Height)
			require.Same(t, block, conf.Block)

		case <-time.After(5 * time.Second):
			t.Fatal("confirmation never arrived")
		}
	}

	confirm(101, 1)

	backend.reorged <- 2
	requireAssetConfSignal(t, events.Reorged)

	confirm(102, 3)

	backend.done <- struct{}{}
	requireAssetConfSignal(t, events.Done)

	events.Cancel()
	requireAssetConfSignal(t, backend.canceled)
}

// TestAssetChainConfirmationBeforeFinal checks that the engine holds the
// confirmation of a transaction before it can learn that it is final, when
// the backend has both ready at once, as for a transfer resumed long after
// it confirmed.
func TestAssetChainConfirmationBeforeFinal(t *testing.T) {
	t.Parallel()

	for range 20 {
		backend := newAssetConfBackend(100)
		block := &wire.MsgBlock{}
		backend.confirmed <- assetTestConfirmation(101, block)
		backend.done <- struct{}{}

		chain := newAssetConfTestChain(t, backend)
		events, err := chain.RegisterConfirmation(
			t.Context(), chainhash.Hash{9}, []byte{0x51}, 100,
		)
		require.NoError(t, err)

		select {
		case conf := <-events.Confirmed:
			require.Same(t, block, conf.Block)

		case <-events.Done:
			t.Fatal("final before the confirmation")

		case <-time.After(5 * time.Second):
			t.Fatal("confirmation never arrived")
		}
		requireAssetConfSignal(t, events.Done)

		events.Cancel()
	}
}

// TestAssetChainConfirmationWithoutBlock checks that a confirmation whose
// block the backend could not fetch ends the events, so that the engine
// registers again.
func TestAssetChainConfirmationWithoutBlock(t *testing.T) {
	t.Parallel()

	backend := newAssetConfBackend(100)
	chain := newAssetConfTestChain(t, backend)

	events, err := chain.RegisterConfirmation(
		t.Context(), chainhash.Hash{9}, []byte{0x51}, 100,
	)
	require.NoError(t, err)

	backend.confirmed <- assetTestConfirmation(101, nil)

	select {
	case conf, ok := <-events.Confirmed:
		require.False(t, ok)
		require.Nil(t, conf)

	case <-time.After(5 * time.Second):
		t.Fatal("events never ended")
	}

	events.Cancel()
	requireAssetConfSignal(t, backend.canceled)
}

// TestAssetChainConfirmationFinality checks that a buried confirmation is
// final on a backend that reports no finality itself.
func TestAssetChainConfirmationFinality(t *testing.T) {
	t.Parallel()

	const confHeight = 101

	tip := int32(confHeight + chainsource.DefaultFinalityDepth - 1)
	backend := newAssetConfBackend(tip)
	chain := newAssetConfTestChain(t, backend)

	events, err := chain.RegisterConfirmation(
		t.Context(), chainhash.Hash{9}, []byte{0x51}, 100,
	)
	require.NoError(t, err)

	backend.confirmed <- assetTestConfirmation(confHeight, &wire.MsgBlock{})

	select {
	case conf := <-events.Confirmed:
		require.EqualValues(t, confHeight, conf.Height)

	case <-time.After(5 * time.Second):
		t.Fatal("confirmation never arrived")
	}
	requireAssetConfSignal(t, events.Done)

	events.Cancel()
}

// TestAssetChainConfirmationNotReady refuses a confirmation watch before the
// chain source is registered.
func TestAssetChainConfirmationNotReady(t *testing.T) {
	t.Parallel()

	chain := &assetChain{
		server: newAssetChainTestServer(newAssetConfBackend(100)),
	}

	_, err := chain.RegisterConfirmation(
		t.Context(), chainhash.Hash{9}, []byte{0x51}, 100,
	)
	require.ErrorIs(t, err, errAssetWalletNotReady)
}

// TestTrustAssetGroup trusts the group of a group-key reference and nothing
// for an asset-ID reference.
func TestTrustAssetGroup(t *testing.T) {
	t.Parallel()

	engine, err := embedded.NewClient(&embedded.Config{
		Network: tapsdk.NetworkRegtest,
		Store:   embedded.NewMemoryStore(),
		KeyRing: &assetKeyRing{server: &Server{
			cfg:         DefaultConfig(),
			walletReady: make(chan struct{}),
		}},
		Chain: &assetChain{},
	})
	require.NoError(t, err)

	groupKey, err := (&AssetsConfig{
		GroupKeys: []string{
			testGroupKeyHex,
		},
	}).groupKeys()
	require.NoError(t, err)
	groupRef := tapsdk.AssetRefFromGroupKey(groupKey[0])
	assetRef := tapsdk.AssetRefFromAssetID(tapsdk.AssetID{1})

	require.NoError(t, trustAssetGroup(engine, groupRef.String()))
	require.NoError(t, trustAssetGroup(engine, assetRef.String()))
	require.Error(t, trustAssetGroup(engine, "not-an-asset"))
}

// TestAssetJournal maps an absent journal entry to the journal's not-found
// error.
func TestAssetJournal(t *testing.T) {
	t.Parallel()

	sqlDB := db.NewTestDB(t)
	store := db.NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), btclog.Disabled,
	)
	journal := &assetJournal{store: db.NewAssetEngineStore(store)}
	ctx := t.Context()

	_, err := journal.Load(ctx, "deposit")
	require.ErrorIs(t, err, tapassets.ErrStoreNotFound)

	require.NoError(t, journal.Store(ctx, "deposit", []byte{1, 2}))
	value, err := journal.Load(ctx, "deposit")
	require.NoError(t, err)
	require.Equal(t, []byte{1, 2}, value)
}
