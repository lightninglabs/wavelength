package waved

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/tap-sdk/embedded"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lndbackend"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/tapassets"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// assetConfCallerID prefixes the caller ID of each confirmation watch
	// of the engine at the chain source.
	assetConfCallerID = "asset-engine"

	// assetConfQueueSize is how many events of one confirmation watch
	// can wait for the engine.
	assetConfQueueSize = 16

	// assetConfUnregisterTimeout bounds the request that removes a
	// confirmation watch from the chain source.
	assetConfUnregisterTimeout = 5 * time.Second
)

// errAssetWalletNotReady reports a call to the asset engine that
// arrived before the daemon wallet was unlocked.
var errAssetWalletNotReady = errors.New("wallet is not ready")

// initEmbeddedAssets creates the asset engine on the daemon's
// database, wallet seed and chain backend, and installs its asset VTXO
// verifier and its claimer. The wallet may still be locked, so the engine
// reaches the wallet and the chain backend on each call. Once the wallet
// services are up it follows the transfers an earlier run left unconfirmed
// and holds the mailbox of every Taproot Asset address open. The returned
// cleanup stops both.
func (s *Server) initEmbeddedAssets(ctx context.Context, store *db.Store) (
	func(), error) {

	if s.cfg.Assets == nil || !s.cfg.Assets.Enabled {
		return func() {}, nil
	}

	groupKeys, err := s.cfg.Assets.groupKeys()
	if err != nil {
		return nil, err
	}

	engineStore := db.NewAssetEngineStore(store)
	keys := &assetKeyRing{server: s}
	receive, err := s.assetReceiveConfig(engineStore, keys)
	if err != nil {
		return nil, err
	}
	client, err := embedded.NewClient(&embedded.Config{
		Network:   tapsdk.Network(s.cfg.Network),
		Store:     engineStore,
		KeyRing:   keys,
		Signer:    keys,
		Chain:     &assetChain{server: s},
		GroupKeys: groupKeys,
		Logger:    &assetEngineLogger{log: s.log},
		Receive:   receive,
	})
	if err != nil {
		return nil, fmt.Errorf("create asset engine: %w", err)
	}

	chain, err := tapassets.NewLocalProofChainVerifier(client)
	if err != nil {
		return nil, err
	}
	verifier, err := tapassets.NewVTXOVerifier(chain)
	if err != nil {
		return nil, err
	}

	// The claim journal lives in the database beside the deposit journal,
	// since a browser has no files.
	claimer, err := s.newAssetClaimer(
		client, &assetJournal{
			store: engineStore,
		},
	)
	if err != nil {
		return nil, err
	}

	s.assetClient = client
	s.embeddedAssets = client
	s.assetAddresses = receive != nil
	s.assetVTXOVerifier = &groupTrustingVerifier{
		verifier: verifier,
		engine:   client,
	}
	s.assetClaimer = &groupTrustingClaimer{
		claimer: claimer,
		engine:  client,
	}

	// The engine follows its transfers through the chain backend and
	// answers a mailbox challenge with the wallet's keys, and both exist
	// only once the wallet is unlocked.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)

		if err := s.WaitForWalletServicesReady(runCtx); err != nil {
			return
		}
		if err := client.ResumeTransfers(runCtx); err != nil {
			s.log.ErrorS(runCtx, "Unable to resume asset "+
				"transfers", err)
		}
		if receive == nil {
			return
		}
		if err := client.ResumeReceives(runCtx); err != nil {
			s.log.ErrorS(runCtx, "Unable to resume asset "+
				"receives", err)
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

// groupTrustingClaimer claims composed outputs with the embedded engine. The
// daemon claims an output it deposited or accepted in a round, so the group
// of that output's asset is one it accepts proofs of. The engine forgets a
// group it trusted for a round at a restart, and the exit of an asset VTXO
// can follow one.
type groupTrustingClaimer struct {
	claimer assetClaimer
	engine  *embedded.Client
}

// Prepare trusts the group of the claimed asset, then signs the claim.
func (c *groupTrustingClaimer) Prepare(ctx context.Context,
	request *tapassets.ClaimRequest) (*tapassets.ClaimResult, error) {

	if err := c.trustClaimedGroup(request); err != nil {
		return nil, err
	}

	return c.claimer.Prepare(ctx, request)
}

// Claim trusts the group of the claimed asset, then records the claim.
func (c *groupTrustingClaimer) Claim(ctx context.Context,
	request *tapassets.ClaimRequest) (*tapassets.ClaimResult, error) {

	if err := c.trustClaimedGroup(request); err != nil {
		return nil, err
	}

	return c.claimer.Claim(ctx, request)
}

// trustClaimedGroup trusts the group of the asset a claim names. The claimer
// refuses a missing request.
func (c *groupTrustingClaimer) trustClaimedGroup(
	request *tapassets.ClaimRequest) error {

	if request == nil {
		return nil
	}

	return trustAssetGroup(c.engine, string(request.AssetRef))
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

// assetKeyRing derives, signs and decrypts with the embedded engine's keys
// through the daemon wallet. An lnd wallet does all three through lnd, so
// its keys never leave lnd. A wallet holding the seed uses its key ring.
type assetKeyRing struct {
	server *Server
}

// ring returns the key ring of the unlocked daemon wallet that holds the
// seed.
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

// lndWallet returns the wallet of the connected lnd, or None when the
// daemon wallet is not lnd.
func (k *assetKeyRing) lndWallet() (fn.Option[*lndbackend.ClientWallet],
	error) {

	s := k.server
	if s.cfg.Wallet.Type != WalletTypeLnd {
		return fn.None[*lndbackend.ClientWallet](), nil
	}
	if !s.isWalletReady() || s.lnd.IsNone() {
		return fn.None[*lndbackend.ClientWallet](),
			errAssetWalletNotReady
	}

	lndSvc := s.lnd.UnsafeFromSome()
	wallet := lndbackend.NewClientWallet(lndSvc.Signer, lndSvc.WalletKit)
	wallet.Log = fn.Some(s.subLogger(lndbackend.Subsystem))

	return fn.Some(wallet), nil
}

// DeriveNextKey derives the next unused key of the family.
func (k *assetKeyRing) DeriveNextKey(ctx context.Context,
	family keychain.KeyFamily) (keychain.KeyDescriptor, error) {

	lndWallet, err := k.lndWallet()
	if err != nil {
		return keychain.KeyDescriptor{}, err
	}
	if lndWallet.IsSome() {
		keyDesc, err := lndWallet.UnsafeFromSome().DeriveNextKey(
			ctx, family,
		)
		if err != nil {
			return keychain.KeyDescriptor{}, err
		}

		return *keyDesc, nil
	}

	ring, err := k.ring()
	if err != nil {
		return keychain.KeyDescriptor{}, err
	}

	return embedded.NewLocalKeyRing(ring).DeriveNextKey(ctx, family)
}

// SignOutputRaw signs the input of tx that signDesc describes with a key
// DeriveNextKey derived.
func (k *assetKeyRing) SignOutputRaw(ctx context.Context, tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	lndWallet, err := k.lndWallet()
	if err != nil {
		return nil, err
	}
	if lndWallet.IsSome() {
		return lndWallet.UnsafeFromSome().SignOutputRawContext(
			ctx, tx, signDesc,
		)
	}

	ring, err := k.ring()
	if err != nil {
		return nil, err
	}

	return embedded.NewKeyRingSigner(ring).SignOutputRaw(
		ctx, tx, signDesc,
	)
}

// lndSigner returns the signer of the connected lnd, or nil when the daemon
// wallet is not lnd.
func (k *assetKeyRing) lndSigner() (lndclient.SignerClient, error) {
	s := k.server
	if s.cfg.Wallet.Type != WalletTypeLnd {
		return nil, nil
	}
	if !s.isWalletReady() || s.lnd.IsNone() {
		return nil, errAssetWalletNotReady
	}

	return s.lnd.UnsafeFromSome().Signer, nil
}

// ECDH returns the SHA-256 of the compressed shared point of the key at
// locator and pubKey, with which the engine decrypts a mailbox message sent
// to an address.
func (k *assetKeyRing) ECDH(ctx context.Context, locator keychain.KeyLocator,
	pubKey *btcec.PublicKey) ([32]byte, error) {

	signer, err := k.lndSigner()
	if err != nil {
		return [32]byte{}, err
	}
	if signer != nil {
		return signer.DeriveSharedKey(ctx, pubKey, &locator)
	}

	ring, err := k.ring()
	if err != nil {
		return [32]byte{}, err
	}

	return embedded.NewKeyRingReceiveKeys(ring).ECDH(ctx, locator, pubKey)
}

// SignMessageSchnorr returns the BIP-340 signature of the key at locator
// over the SHA-256 of msg, with which the engine answers the challenge of a
// mailbox. lnd signs that digest for a Schnorr signature without a tag.
func (k *assetKeyRing) SignMessageSchnorr(ctx context.Context,
	locator keychain.KeyLocator, msg []byte) (*schnorr.Signature, error) {

	signer, err := k.lndSigner()
	if err != nil {
		return nil, err
	}
	if signer != nil {
		sig, err := signer.SignMessage(
			ctx, msg, locator, lndclient.SignSchnorr(nil),
		)
		if err != nil {
			return nil, err
		}

		return schnorr.ParseSignature(sig)
	}

	ring, err := k.ring()
	if err != nil {
		return nil, err
	}

	return embedded.NewKeyRingReceiveKeys(ring).SignMessageSchnorr(
		ctx, locator, msg,
	)
}

// assetChain is the embedded engine's view of the chain through the
// daemon's chain backend.
type assetChain struct {
	server *Server

	// confWatches counts the confirmation watches registered so far.
	confWatches atomic.Uint64
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

// BlockHeader returns the header of the best-chain block at height, which
// must not be above the best block.
func (c *assetChain) BlockHeader(ctx context.Context, height uint32) (
	*wire.BlockHeader, error) {

	backend, err := c.backend()
	if err != nil {
		return nil, err
	}

	// A backend can serve the header of a block its tip has not reached:
	// the lwwallet backend polls its tip and reads headers from Esplora
	// directly. The engine takes a block above the tip for one that left
	// the chain, so it gets no header the tip does not cover and waits
	// for the tip instead.
	tip, _, err := c.BestBlock(ctx)
	if err != nil {
		return nil, err
	}
	if height > tip {
		return nil, fmt.Errorf("block at height %d is above the chain "+
			"tip at height %d", height, tip)
	}

	if headers, ok := backend.(chainsource.BlockHeaderSource); ok {
		return headers.BlockHeaderByHeight(ctx, height)
	}

	// The lnd chain backend serves no headers, but lnd's chain kit does.
	s := c.server
	if s.cfg.Wallet.Type == WalletTypeLnd && s.lnd.IsSome() {
		chainKit := s.lnd.UnsafeFromSome().ChainKit
		hash, err := chainKit.GetBlockHash(ctx, int64(height))
		if err != nil {
			return nil, fmt.Errorf("block hash at height %d: %w",
				height, err)
		}

		return chainKit.GetBlockHeader(ctx, hash)
	}

	return nil, fmt.Errorf("chain backend %T serves no block headers",
		backend)
}

// BlockHash returns the hash of the best-chain block at height.
func (c *assetChain) BlockHash(ctx context.Context, height uint32) (
	chainhash.Hash, error) {

	header, err := c.BlockHeader(ctx, height)
	if err != nil {
		return chainhash.Hash{}, err
	}

	return header.BlockHash(), nil
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

	// The chain source keeps one watch per caller ID, transaction, script
	// and confirmation target: a second registration replaces the first,
	// and unregistering either ends both. Each watch therefore gets a
	// caller ID of its own.
	callerID := fmt.Sprintf("%s-%d", assetConfCallerID,
		c.confWatches.Add(1))

	watch := newAssetConfWatch()
	_, err = chainSource.Ask(ctx, &chainsource.RegisterConfRequest{
		CallerID:      callerID,
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
					CallerID:    callerID,
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

// assetJournal keeps the asset deposit and claim journals in the daemon's
// database. Each journal prefixes its own keys, so the two share one table.
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

// Errorf logs a failure the engine retries, such as a transfer it cannot
// confirm yet or a mailbox stream that dropped. The engine keeps retrying on
// its own, so nobody has to act on it.
func (l *assetEngineLogger) Errorf(format string, args ...any) {
	l.log.Infof("Asset engine: "+format, args...)
}
