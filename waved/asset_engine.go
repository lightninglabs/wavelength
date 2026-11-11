package waved

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/tap-sdk/embedded"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/tapassets"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// assetConfCallerID names the engine's confirmation watches at the
	// chain source.
	assetConfCallerID = "asset-engine"

	// assetConfQueueSize is how many events of one confirmation watch
	// can wait for the engine.
	assetConfQueueSize = 16

	// assetConfUnregisterTimeout bounds the request that removes a
	// confirmation watch from the chain source.
	assetConfUnregisterTimeout = 5 * time.Second
)

// errAssetWalletNotReady reports a call to the embedded asset backend that
// arrived before the daemon wallet was unlocked.
var errAssetWalletNotReady = errors.New("wallet is not ready")

// startEmbeddedAssets starts the embedded asset backend on the daemon's
// database, wallet seed and chain backend, and installs its asset VTXO
// verifier. The wallet may still be locked, so the engine reaches the
// wallet and the chain backend on each call, and resumes its transfers once
// the wallet services are up. The returned cleanup stops the engine.
func (s *Server) startEmbeddedAssets(ctx context.Context, store *db.Store) (
	func(), error) {

	if s.cfg.Assets == nil ||
		s.cfg.Assets.Backend != AssetBackendEmbedded {
		return func() {}, nil
	}

	groupKeys, err := s.cfg.Assets.groupKeys()
	if err != nil {
		return nil, err
	}

	client, err := embedded.NewClient(&embedded.Config{
		Network:   tapsdk.Network(s.cfg.Network),
		Store:     db.NewAssetEngineStore(store),
		KeyRing:   &assetKeyRing{server: s},
		Chain:     &assetChain{server: s},
		GroupKeys: groupKeys,
		Logger:    &assetEngineLogger{log: s.log},
	})
	if err != nil {
		return nil, fmt.Errorf("start embedded asset backend: %w", err)
	}

	chain, err := tapassets.NewLocalProofChainVerifier(client)
	if err != nil {
		return nil, err
	}
	verifier, err := tapassets.NewVTXOVerifier(chain)
	if err != nil {
		return nil, err
	}

	s.assetClient = client
	s.embeddedAssets = client
	s.assetVTXOVerifier = &groupTrustingVerifier{
		verifier: verifier,
		engine:   client,
	}

	// The engine follows its transfers through the chain backend, which
	// exists only once the wallet is unlocked.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)

		if err := s.WaitForWalletServicesReady(runCtx); err != nil {
			return
		}
		if err := client.Start(runCtx); err != nil {
			s.log.ErrorS(runCtx, "Unable to resume asset "+
				"transfers", err)
		}
	}()

	return func() {
		cancel()
		<-done

		if err := client.Close(); err != nil {
			s.log.DebugS(ctx, "Unable to stop the asset engine",
				slog.String("error", err.Error()),
			)
		}
	}, nil
}

// groupTrustingVerifier verifies asset VTXOs with the embedded engine. The
// daemon asks for a VTXO of the asset it names, so that asset's group is
// one it accepts proofs of.
type groupTrustingVerifier struct {
	verifier round.AssetVTXOVerifier
	engine   *embedded.Client
}

// VerifyAssetVTXO trusts the group of the requested asset, then verifies the
// VTXO.
func (v *groupTrustingVerifier) VerifyAssetVTXO(ctx context.Context,
	assetRef string, assetAmount uint64, commitmentTx *wire.MsgTx,
	clientTree *tree.Tree, sealedPackage []byte) error {

	if err := trustAssetGroup(v.engine, assetRef); err != nil {
		return err
	}

	return v.verifier.VerifyAssetVTXO(
		ctx, assetRef, assetAmount, commitmentTx, clientTree,
		sealedPackage,
	)
}

// trustAssetGroup makes the engine verify proofs of the group an asset
// reference names. A reference to a single issuance names no group.
func trustAssetGroup(engine *embedded.Client, assetRef string) error {
	ref, err := tapsdk.ParseAssetRef(assetRef)
	if err != nil {
		return fmt.Errorf("asset reference: %w", err)
	}
	if groupKey, ok := ref.GroupKey(); ok {
		engine.TrustGroupKey(groupKey)
	}

	return nil
}

// assetKeyRing derives the embedded engine's keys from the daemon wallet's
// seed.
type assetKeyRing struct {
	server *Server
}

// ring returns the unlocked daemon wallet's key ring.
func (k *assetKeyRing) ring() (keychain.SecretKeyRing, error) {
	s := k.server
	if !s.isWalletReady() {
		return nil, errAssetWalletNotReady
	}

	switch s.cfg.Wallet.Type {
	case WalletTypeLwwallet:
		if s.lwWallet.IsSome() {
			return s.lwWallet.UnsafeFromSome().KeyRing(), nil
		}

	case WalletTypeBtcwallet:
		if s.btcwWallet.IsSome() {
			return s.btcwWallet.UnsafeFromSome().KeyRing(), nil
		}
	}

	return nil, errAssetWalletNotReady
}

// DeriveNextKey derives the next unused key of the family.
func (k *assetKeyRing) DeriveNextKey(family keychain.KeyFamily) (
	keychain.KeyDescriptor, error) {

	ring, err := k.ring()
	if err != nil {
		return keychain.KeyDescriptor{}, err
	}

	return ring.DeriveNextKey(family)
}

// DerivePrivKey derives the private key of a key descriptor.
func (k *assetKeyRing) DerivePrivKey(desc keychain.KeyDescriptor) (
	*btcec.PrivateKey, error) {

	ring, err := k.ring()
	if err != nil {
		return nil, err
	}

	return ring.DerivePrivKey(desc)
}

// assetChain is the embedded engine's view of the chain through the
// daemon's chain backend.
type assetChain struct {
	server *Server
}

// backend returns the chain backend of the unlocked daemon wallet.
func (c *assetChain) backend() (chainsource.ChainBackend, error) {
	s := c.server
	if !s.isWalletReady() || s.chainBackend == nil {
		return nil, errAssetWalletNotReady
	}

	return s.chainBackend, nil
}

// BestBlock returns the height and hash of the best chain tip.
func (c *assetChain) BestBlock(ctx context.Context) (uint32, chainhash.Hash,
	error) {

	backend, err := c.backend()
	if err != nil {
		return 0, chainhash.Hash{}, err
	}

	height, hash, err := backend.BestBlock(ctx)
	if err != nil {
		return 0, chainhash.Hash{}, err
	}
	if height < 0 {
		return 0, chainhash.Hash{}, fmt.Errorf("chain backend "+
			"reported height %d", height)
	}

	return uint32(height), hash, nil
}

// BlockHeader returns the header of the best-chain block at height.
func (c *assetChain) BlockHeader(ctx context.Context, height uint32) (
	*wire.BlockHeader, error) {

	backend, err := c.backend()
	if err != nil {
		return nil, err
	}

	headers, ok := backend.(chainsource.BlockHeaderSource)
	if !ok {
		return nil, fmt.Errorf("chain backend %T serves no "+
			"block headers", backend)
	}

	return headers.BlockHeaderByHeight(ctx, height)
}

// PublishTransaction broadcasts a transaction. A transaction the network
// already has is published.
func (c *assetChain) PublishTransaction(ctx context.Context, tx *wire.MsgTx,
	label string) error {

	backend, err := c.backend()
	if err != nil {
		return err
	}

	err = backend.BroadcastTx(ctx, tx, label)
	if err == nil || chainsource.IsIgnorableBroadcastError(err) ||
		strings.Contains(err.Error(), "already in block chain") {
		return nil
	}

	return err
}

// chainSource returns the chain source actor, which the daemon registers
// before the wallet services are up.
func (c *assetChain) chainSource() (actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
], error) {

	s := c.server
	if !s.isWalletReady() || s.actorSystem == nil {
		return nil, errAssetWalletNotReady
	}

	refs := actor.FindInReceptionist(
		s.actorSystem.Receptionist(), chainsource.ChainSourceKey,
	)
	if len(refs) == 0 {
		return nil, errAssetWalletNotReady
	}

	return refs[0], nil
}

// RegisterConfirmation follows the confirmation of a transaction through
// re-orgs until it is final. The watch goes through the chain source, which
// orders the events of every backend and reports a confirmation final once
// it is buried, also on a backend that reports no finality itself.
func (c *assetChain) RegisterConfirmation(ctx context.Context,
	txid chainhash.Hash, pkScript []byte, heightHint uint32) (
	*embedded.ConfirmationEvents, error) {

	chainSource, err := c.chainSource()
	if err != nil {
		return nil, err
	}

	watch := newAssetConfWatch()
	_, err = chainSource.Ask(ctx, &chainsource.RegisterConfRequest{
		CallerID:      assetConfCallerID,
		Txid:          &txid,
		PkScript:      pkScript,
		TargetConfs:   1,
		HeightHint:    heightHint,
		IncludeBlock:  true,
		NotifyActor:   fn.Some(watch.confirmedRef()),
		NotifyReorged: fn.Some(watch.reorgedRef()),
		NotifyDone:    fn.Some(watch.doneRef()),
	}).Await(ctx).Unpack()
	if err != nil {
		watch.stop()

		return nil, fmt.Errorf("register confirmation watch: %w", err)
	}

	go watch.forward()

	// The engine cancels a watch when its tracker stops, which can be
	// after the tracker's context ended.
	unregisterCtx := context.WithoutCancel(ctx)

	return &embedded.ConfirmationEvents{
		Confirmed: watch.confirmed,
		Reorged:   watch.reorged,
		Done:      watch.done,
		Cancel: func() {
			watch.stop()

			ctx, cancel := context.WithTimeout(
				unregisterCtx, assetConfUnregisterTimeout,
			)
			defer cancel()

			_ = chainSource.Tell(
				ctx, &chainsource.UnregisterConfRequest{
					CallerID:    assetConfCallerID,
					Txid:        &txid,
					PkScript:    pkScript,
					TargetConfs: 1,
				},
			)
		},
	}, nil
}

type (
	// assetConfirmedRef takes the confirmations of a watch.
	assetConfirmedRef = actor.TellOnlyRef[chainsource.ConfirmationEvent]

	// assetReorgedRef takes the re-orgs of a watch.
	assetReorgedRef = actor.TellOnlyRef[chainsource.ConfReorgedEvent]

	// assetDoneRef takes the finality of a watch.
	assetDoneRef = actor.TellOnlyRef[chainsource.ConfDoneEvent]
)

// assetConfEvent is one event of a confirmation watch.
type assetConfEvent struct {
	// confirmed is the confirming block of a confirmation event.
	confirmed *embedded.Confirmation

	// reorged is set for the event of a confirming block that left the
	// best chain.
	reorged bool

	// done is set for the event of a final confirmation.
	done bool
}

// assetConfirmedEvent converts a confirmation of the chain source. A
// confirmation without its block converts to an event without one.
func assetConfirmedEvent(event chainsource.ConfirmationEvent) assetConfEvent {
	if event.Block == nil || event.BlockHeight < 0 {
		return assetConfEvent{}
	}

	return assetConfEvent{
		confirmed: &embedded.Confirmation{
			Height: uint32(event.BlockHeight),
			Block:  event.Block,
		},
	}
}

// assetReorgedEvent converts a re-org event of the chain source.
func assetReorgedEvent(chainsource.ConfReorgedEvent) assetConfEvent {
	return assetConfEvent{reorged: true}
}

// assetDoneEvent converts a finality event of the chain source.
func assetDoneEvent(chainsource.ConfDoneEvent) assetConfEvent {
	return assetConfEvent{done: true}
}

// assetConfWatch hands the events of one confirmation watch to the engine in
// the order the chain source delivered them. The engine reads each kind of
// event from its own channel and picks among ready channels at random, so
// the channels are unbuffered and one event is offered at a time: the
// engine holds a confirmation before it can learn that it is final.
type assetConfWatch struct {
	// queue holds the events the chain source delivered and the engine
	// has not taken yet.
	queue chan assetConfEvent

	confirmed chan *embedded.Confirmation
	reorged   chan struct{}
	done      chan struct{}

	// quit is closed when the engine cancels the watch.
	quit     chan struct{}
	stopOnce sync.Once
}

// newAssetConfWatch returns a watch that forwards nothing until forward
// runs.
func newAssetConfWatch() *assetConfWatch {
	return &assetConfWatch{
		queue:     make(chan assetConfEvent, assetConfQueueSize),
		confirmed: make(chan *embedded.Confirmation),
		reorged:   make(chan struct{}),
		done:      make(chan struct{}),
		quit:      make(chan struct{}),
	}
}

// confirmedRef returns the reference that takes the watch's confirmations.
func (w *assetConfWatch) confirmedRef() assetConfirmedRef {
	return assetConfRef[chainsource.ConfirmationEvent]{
		watch: w,
		event: assetConfirmedEvent,
	}
}

// reorgedRef returns the reference that takes the watch's re-orgs.
func (w *assetConfWatch) reorgedRef() assetReorgedRef {
	return assetConfRef[chainsource.ConfReorgedEvent]{
		watch: w,
		event: assetReorgedEvent,
	}
}

// doneRef returns the reference that takes the watch's finality.
func (w *assetConfWatch) doneRef() assetDoneRef {
	return assetConfRef[chainsource.ConfDoneEvent]{
		watch: w,
		event: assetDoneEvent,
	}
}

// stop ends the watch. It is safe to call more than once.
func (w *assetConfWatch) stop() {
	w.stopOnce.Do(func() {
		close(w.quit)
	})
}

// forward hands the queued events to the engine one at a time until the
// confirmation is final or the watch stops. A confirmation whose block the
// backend could not fetch ends the events instead: the engine then
// registers again, and the backend looks the block up again.
func (w *assetConfWatch) forward() {
	for {
		var event assetConfEvent
		select {
		case event = <-w.queue:
		case <-w.quit:
			return
		}

		switch {
		case event.confirmed != nil:
			select {
			case w.confirmed <- event.confirmed:
			case <-w.quit:
				return
			}

		case event.reorged:
			select {
			case w.reorged <- struct{}{}:
			case <-w.quit:
				return
			}

		case event.done:
			select {
			case w.done <- struct{}{}:
			case <-w.quit:
			}

			return

		default:
			close(w.confirmed)

			return
		}
	}
}

// assetConfRef is the reference through which the chain source delivers one
// kind of event to a confirmation watch.
type assetConfRef[M actor.Message] struct {
	watch *assetConfWatch
	event func(M) assetConfEvent
}

// ID names the reference in the chain source's logs.
func (r assetConfRef[M]) ID() string {
	return "asset-conf-watch"
}

// Tell queues an event for the engine. An event for a stopped watch is
// dropped.
func (r assetConfRef[M]) Tell(ctx context.Context, msg M) error {
	select {
	case r.watch.queue <- r.event(msg):
		return nil

	case <-r.watch.quit:
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryTell queues an event for the engine if the queue has room.
func (r assetConfRef[M]) TryTell(ctx context.Context, msg M) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	select {
	case r.watch.queue <- r.event(msg):
		return nil

	case <-r.watch.quit:
		return nil

	default:
		return actor.ErrMailboxFull
	}
}

// assetJournal keeps the asset deposit journal in the daemon's database.
type assetJournal struct {
	store *db.AssetEngineStore
}

// Load returns the journal value stored under key.
func (j *assetJournal) Load(ctx context.Context, key string) ([]byte, error) {
	value, err := j.store.LoadJournalEntry(ctx, key)
	if errors.Is(err, db.ErrAssetJournalEntryNotFound) {
		return nil, tapassets.ErrStoreNotFound
	}

	return value, err
}

// Store stores a journal value under key.
func (j *assetJournal) Store(ctx context.Context, key string,
	value []byte) error {

	return j.store.StoreJournalEntry(ctx, key, value)
}

// assetEngineLogger writes the embedded engine's background events to the
// daemon log.
type assetEngineLogger struct {
	log btclog.Logger
}

// Infof logs a transfer's progress.
func (l *assetEngineLogger) Infof(format string, args ...any) {
	l.log.Infof("Asset engine: "+format, args...)
}

// Errorf logs a failure the engine retries. The engine keeps retrying on its
// own, so nobody has to act on it.
func (l *assetEngineLogger) Errorf(format string, args ...any) {
	l.log.Infof("Asset engine: "+format, args...)
}
