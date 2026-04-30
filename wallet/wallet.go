package wallet

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/taproot-assets/proof"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// BoardingKeyFamily is the BIP32 key family used for deriving boarding
	// address keys.
	BoardingKeyFamily = 42

	// MinBoardingConfs is the minimum number of confirmations required
	// before notifying about a boarding UTXO.
	MinBoardingConfs = 1

	// MaxConfsForListUnspent is the maximum confirmations parameter for
	// ListUnspent queries.
	MaxConfsForListUnspent = 9999999

	// listUnspentMaxRetries is the maximum number of times we'll retry a
	// ListUnspent query within a single block epoch if we didn't detect any
	// new boarding UTXOs. This mitigates a race where we receive a block
	// epoch notification before the wallet's UTXO set is fully updated.
	// For neutrino backends, btcwallet's internal block processing
	// (fetching the full block from P2P, running AddCredit) can take
	// over a second after the epoch arrives.
	listUnspentMaxRetries = 10

	// listUnspentRetryDelay is the delay between ListUnspent retries.
	// We keep this small so confirmed boarding UTXOs are detected
	// promptly without waiting for another block.
	listUnspentRetryDelay = 200 * time.Millisecond
)

// notifierInfo holds the configuration for a registered confirmation notifier.
type notifierInfo struct {
	// actor is the reference to send BoardingUtxoConfirmedEvent messages
	// to.
	actor actor.TellOnlyRef[BoardingUtxoConfirmedEvent]

	// minConf is the minimum number of confirmations required before
	// notifying this actor about a boarding UTXO.
	minConf uint32
}

// Ark manages boarding addresses and monitors for on-chain boarding UTXOs. It
// provides the primary interface for creating boarding addresses, tracking
// confirmations, and notifying registered actors (like the round actor) when
// new boarding opportunities are detected.
type Ark struct {
	// backend provides LND integration for key derivation, address import,
	// and UTXO enumeration.
	backend BoardingBackend

	// store persists boarding addresses and intents to the database.
	store BoardingStore

	// vtxoReader provides read-only access to VTXO descriptors. The wallet
	// uses this to load VTXO data when building intent packages for round
	// registration (refresh and leave flows).
	vtxoReader VTXOReader

	// actorSystem is the actor system context for looking up actors by
	// service key. Used to find the round actor when forwarding intent
	// registration messages.
	actorSystem actor.SystemContext

	// chainSource provides block epoch notifications for polling.
	chainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]

	// notifiers is the map of registered actors to notify of new confirmed
	// UTXOs. Each entry contains the actor reference and configuration.
	notifiers map[string]notifierInfo

	// seenUtxos tracks all UTXOs we've already processed to detect new
	// confirmations from ListUnspent.
	seenUtxos fn.Set[UtxoKey]

	// wg tracks background goroutines spawned by the wallet actor.
	wg sync.WaitGroup

	// ctx is the wallet's internal context, cancelled on shutdown. Used for
	// background goroutines that should respect wallet lifecycle.
	//
	//nolint:containedctx
	ctx context.Context

	// cancel cancels the internal context on shutdown.
	cancel context.CancelFunc

	// actorLog is an optional logger for this actor instance. When set, it
	// takes precedence over the context-based logger from
	// build.LoggerFromContext. When None, the actor falls back to the
	// context logger (or btclog.Disabled if none is found).
	actorLog fn.Option[btclog.Logger]

	// ledgerSink is an optional reference to the client-side ledger
	// accounting actor. When set, the wallet emits UTXOCreatedMsg /
	// UTXOSpentMsg events as on-chain wallet UTXOs come and go so
	// the local ledger sees a complete on-chain audit log alongside
	// the off-chain double-entry ledger. When None, audit emission
	// is silently skipped (tests, lightweight harnesses).
	ledgerSink fn.Option[ledger.Sink]
}

// NewArk creates a new Ark wallet actor. The logger is optional and falls back
// to the global package logger when nil is passed.
//
// The vtxoReader provides read-only VTXO descriptor access so the wallet can
// compose intent packages for round registration. The actorSystem enables
// round actor lookup via service key. The ledgerSink is required (use fn.None
// to opt out); it is plumbed as a mandatory argument so every call site must
// make an explicit choice about accounting emission rather than silently
// skipping it.
func NewArk(backend BoardingBackend, store BoardingStore,
	vtxoReader VTXOReader,
	chainSource actor.ActorRef[chainsource.ChainSourceMsg, chainsource.ChainSourceResp],
	actorSystem actor.SystemContext,
	ledgerSink fn.Option[ledger.Sink],
	actorLog btclog.Logger) *Ark {

	// Wrap the provided logger in an Option. A nil logger becomes None,
	// causing the actor to fall back to build.LoggerFromContext at call
	// sites via the logger() helper.
	optLog := fn.None[btclog.Logger]()
	if actorLog != nil {
		optLog = fn.Some(actorLog)
	}

	return &Ark{
		backend:     backend,
		store:       store,
		vtxoReader:  vtxoReader,
		chainSource: chainSource,
		actorSystem: actorSystem,
		ledgerSink:  ledgerSink,
		notifiers:   make(map[string]notifierInfo),
		seenUtxos:   fn.NewSet[UtxoKey](),
		actorLog:    optLog,
	}
}

// logger returns the configured logger or falls back to extracting from
// context. If no logger is found in either location, returns btclog.Disabled.
func (a *Ark) logger(ctx context.Context) btclog.Logger {
	return a.actorLog.UnwrapOr(build.LoggerFromContext(ctx))
}

// emitUTXOCreated posts a UTXOCreatedMsg to the client ledger
// actor when the wallet observes a new on-chain UTXO. The ledger
// handler persists the row in the wallet_utxo_log audit table;
// this is purely observational (no double-entry debit/credit is
// written for wallet UTXO events).
//
// Classification is supplied by the caller because the wallet
// actor alone cannot always tell whether a UTXO is a deposit, a
// change output from a round, or a sweep return -- that context
// lives with whichever subsystem triggered the underlying tx.
// Emission is guarded by the nil-safe fn.Option[ledger.Sink] and
// Tell failures are logged but not propagated so a momentary
// ledger outage never blocks the confirmation path.
func (a *Ark) emitUTXOCreated(ctx context.Context, utxo *Utxo,
	blockHeight int32, classification string) {

	a.ledgerSink.WhenSome(func(sink ledger.Sink) {
		if utxo == nil {
			return
		}

		var height uint32
		if blockHeight > 0 {
			height = uint32(blockHeight)
		}

		msg := &ledger.UTXOCreatedMsg{
			OutpointHash:   utxo.Outpoint.Hash,
			OutpointIndex:  utxo.Outpoint.Index,
			AmountSat:      int64(utxo.Amount),
			BlockHeight:    height,
			Classification: classification,
		}

		if err := sink.Tell(ctx, msg); err != nil {
			a.logger(ctx).WarnS(ctx,
				"Failed to emit UTXOCreatedMsg to ledger", err,
				btclog.Fmt("outpoint", "%v", utxo.Outpoint),
				slog.Int64("amount_sat", int64(utxo.Amount)),
				slog.String("classification", classification))
		}
	})
}

// allTargetErrors builds a per-outpoint error map for operations that fail
// before the wallet can inspect individual VTXOs.
func allTargetErrors(targets []wire.OutPoint,
	err error) map[wire.OutPoint]error {

	errors := make(map[wire.OutPoint]error, len(targets))
	for _, outpoint := range targets {
		errors[outpoint] = err
	}

	return errors
}

// Start initializes the actor and subscribes to block epochs. The selfRef
// parameter is the actor's own reference, used to receive block epoch
// notifications from the chainsource actor.
func (a *Ark) Start(ctx context.Context,
	selfRef actor.TellOnlyRef[WalletMsg]) error {

	// Create an internal context for background goroutines that outlive
	// request contexts but should respect wallet shutdown.
	a.ctx, a.cancel = context.WithCancel(context.Background())

	// Load existing addresses from database to populate seenUtxos for
	// restart recovery.
	addresses, err := a.store.ListAllBoardingAddresses(ctx)
	if err != nil {
		return fmt.Errorf("load existing addresses: %w", err)
	}

	a.logger(ctx).InfoS(ctx, "Loaded boarding addresses from database",
		slog.Int("count", len(addresses)))

	// Re-import each persisted boarding address into the boarding
	// backend. For in-memory backends (lwwallet), this restores
	// tracked scripts lost on restart. For persistent backends
	// (LND), the script may already exist and the import will
	// return a benign "already exists" error which we log and
	// skip since the backend is already tracking the address.
	for _, addr := range addresses {
		_, err := a.backend.ImportTaprootScript(
			ctx, addr.Tapscript,
		)
		if err != nil {
			// The import may fail if the backend already
			// tracks this script (e.g., LND persists
			// imports internally and returns "already
			// exists"). This is expected during restart
			// recovery so we log and continue.
			a.logger(ctx).DebugS(ctx,
				"Boarding address re-import skipped",
				slog.String("address",
					addr.Address.String()),
				slog.String("reason",
					err.Error()))
		}
	}

	// Load just the outpoints of existing intents to populate seenUtxos.
	// This is more efficient than loading full intents since we only need
	// the outpoints to avoid duplicate notifications.
	outpoints, err := a.store.FetchBoardingIntentOutpoints(ctx)
	if err != nil {
		return fmt.Errorf("load existing intent outpoints: %w", err)
	}

	// Add each outpoint to our seen UTXO map to ensure we don't make
	// duplicate notifications.
	for _, outpoint := range outpoints {
		key := NewUtxoKey(outpoint)
		a.seenUtxos.Add(key)
	}

	a.logger(ctx).InfoS(ctx, "Loaded existing boarding intents",
		slog.Int("count", len(outpoints)))

	// Subscribe to block epochs from chainsource using notify pattern. Map
	// BlockEpoch messages to BlockEpochNotification for our actor.
	epochRef := chainsource.MapBlockEpoch(selfRef,
		func(epoch chainsource.BlockEpoch) WalletMsg {
			return BlockEpochNotification{BlockEpoch: epoch}
		},
	)
	req := &chainsource.SubscribeBlocksRequest{
		CallerID:    "boarding-wallet",
		NotifyActor: fn.Some(epochRef),
	}

	future := a.chainSource.Ask(ctx, req)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("subscribe to block epochs: %w",
			result.Err())
	}

	a.logger(ctx).InfoS(ctx, "Boarding wallet actor started")

	return nil
}

// Stop gracefully shuts down the wallet actor by unsubscribing from block
// notifications and waiting for any in-flight backlog deliveries to complete.
func (a *Ark) Stop(ctx context.Context) {
	a.logger(ctx).InfoS(ctx, "Stopping boarding wallet actor")

	// Cancel the internal context to signal background goroutines to stop.
	if a.cancel != nil {
		a.cancel()
	}

	err := a.chainSource.Tell(ctx, &chainsource.UnsubscribeBlocksRequest{
		CallerID: "boarding-wallet",
	})
	if err != nil {
		a.logger(ctx).WarnS(ctx, "Failed to unsubscribe blocks", err)
	}

	a.wg.Wait()

	a.logger(ctx).InfoS(ctx, "Boarding wallet actor stopped")
}

// Receive processes incoming messages using the actor pattern.
func (a *Ark) Receive(ctx context.Context,
	msg WalletMsg) fn.Result[WalletResp] {

	switch m := msg.(type) {
	case *CreateBoardingAddressRequest:
		return a.handleCreateBoardingAddress(ctx, m)

	case *GetActiveBoardingAddressesRequest:
		return a.handleGetActiveBoardingAddresses(ctx, m)

	case *GetBoardingBalanceRequest:
		return a.handleGetBoardingBalance(ctx, m)

	case *RegisterConfirmationNotifierRequest:
		return a.handleRegisterNotifier(ctx, m)

	case *GetConfirmedBoardingIntentsRequest:
		return a.handleGetConfirmedBoardingIntents(ctx, m)

	case *UnregisterConfirmationNotifierRequest:
		return a.handleUnregisterNotifier(ctx, m)

	case BlockEpochNotification:
		return a.handleBlockEpoch(ctx, m.BlockEpoch)

	case *RefreshVTXOsRequest:
		return a.handleRefreshVTXOs(ctx, m)

	case *LeaveVTXOsRequest:
		return a.handleLeaveVTXOs(ctx, m)

	case *BoardRequest:
		return a.handleBoard(ctx, m)

	case *SelectAndLockVTXOsRequest:
		return a.handleSelectAndLockVTXOs(ctx, m)

	case *UnlockVTXOsRequest:
		return a.handleUnlockVTXOs(ctx, m)

	case *CompleteSpendVTXOsRequest:
		return a.handleCompleteSpendVTXOs(ctx, m)

	case *SendVTXOsRequest:
		return a.handleSendVTXOs(ctx, m)

	default:
		return fn.Err[WalletResp](
			fmt.Errorf("unknown message type: %T", msg))
	}
}

// handleCreateBoardingAddress derives a new key, constructs a boarding
// tapscript, imports it into LND, and persists the address.
func (a *Ark) handleCreateBoardingAddress(ctx context.Context,
	req *CreateBoardingAddressRequest) fn.Result[WalletResp] {

	// Grab a fresh key from lnd, then create the boarding tapscript given
	// the current operator information.
	keyDesc, err := a.backend.DeriveNextKey(ctx, BoardingKeyFamily)
	if err != nil {
		return fn.Err[WalletResp](fmt.Errorf("derive key: %w", err))
	}
	tapscript, err := buildBoardingTapscript(
		keyDesc.PubKey, req.OperatorKey, req.ExitDelay,
	)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("build tapscript: %w", err))
	}

	// We'll now import the address into lnd which will enable us to view
	// the credits to the address using list unspent, etc.
	address, err := a.backend.ImportTaprootScript(ctx, tapscript)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("import taproot script: %w", err),
		)
	}

	// With the address created, we'll now write the new boarding address
	// to disk.
	boardingAddr := &BoardingAddress{
		Address:     address,
		Tapscript:   tapscript,
		KeyDesc:     *keyDesc,
		OperatorKey: req.OperatorKey,
		ExitDelay:   req.ExitDelay,
	}
	err = a.store.InsertBoardingAddress(ctx, boardingAddr)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("persist address: %w", err),
		)
	}

	a.logger(ctx).InfoS(ctx, "Created new boarding address",
		slog.String("address", address.String()),
		slog.Int("exit_delay", int(req.ExitDelay)))

	resp := &CreateBoardingAddressResponse{
		Address:   address,
		ClientKey: keyDesc.PubKey,
	}

	return fn.Ok[WalletResp](resp)
}

// handleGetActiveBoardingAddresses queries all boarding addresses from the
// database.
func (a *Ark) handleGetActiveBoardingAddresses(
	ctx context.Context,
	_ *GetActiveBoardingAddressesRequest) fn.Result[WalletResp] {

	addresses, err := a.store.ListAllBoardingAddresses(ctx)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("list addresses: %w", err))
	}

	resp := &GetActiveBoardingAddressesResponse{
		Addresses: addresses,
	}

	return fn.Ok[WalletResp](resp)
}

// handleGetBoardingBalance queries all boarding intents and sums their amounts.
func (a *Ark) handleGetBoardingBalance(ctx context.Context,
	_ *GetBoardingBalanceRequest) fn.Result[WalletResp] {

	status := BoardingStatusConfirmed
	intents, err := a.store.FetchBoardingIntentsByStatus(ctx, status)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("fetch intents: %w", err),
		)
	}

	var totalBalance btcutil.Amount
	for _, intent := range intents {
		totalBalance += intent.ChainInfo.Amount
	}

	resp := &GetBoardingBalanceResponse{
		TotalBalance: totalBalance,
		UtxoCount:    len(intents),
	}

	return fn.Ok[WalletResp](resp)
}

// handleRegisterNotifier adds an actor to the notification list and optionally
// sends backlog events.
func (a *Ark) handleRegisterNotifier(ctx context.Context,
	req *RegisterConfirmationNotifierRequest) fn.Result[WalletResp] {

	// Reject duplicate registrations. Callers must unregister first before
	// re-registering with the same ID.
	if _, exists := a.notifiers[req.NotifierID]; exists {
		return fn.Err[WalletResp](fmt.Errorf(
			"notifier already registered: %s", req.NotifierID,
		))
	}

	// Use the caller's minConf if specified, otherwise use the default.
	minConf := req.MinConf.UnwrapOr(MinBoardingConfs)

	a.notifiers[req.NotifierID] = notifierInfo{
		actor:   req.NotifyActor,
		minConf: minConf,
	}

	a.logger(ctx).InfoS(ctx, "Registered confirmation notifier",
		slog.String("notifier_id", req.NotifierID),
		slog.Int("min_conf", int(minConf)),
	)

	// If a backlog is needed, send it asynchronously so we don't block
	// the registration response. We use the wallet's internal context since
	// the request context will be cancelled after Ask returns, but we still
	// want the backlog goroutine to respect wallet shutdown.
	req.BacklogHeight.WhenSome(func(height int32) {
		a.wg.Go(func() {
			a.sendBacklog(a.ctx, req.NotifyActor, height)
		})
	})

	resp := &RegisterConfirmationNotifierResponse{
		Success: true,
	}

	return fn.Ok[WalletResp](resp)
}

// handleGetConfirmedBoardingIntents returns the wallet's currently confirmed
// boarding intents. This gives the round actor a restart-safe way to rebuild
// pending boarding input packages from the wallet's persisted state.
func (a *Ark) handleGetConfirmedBoardingIntents(ctx context.Context,
	_ *GetConfirmedBoardingIntentsRequest) fn.Result[WalletResp] {

	intents, err := a.store.FetchBoardingIntentsByStatus(
		ctx, BoardingStatusConfirmed,
	)
	if err != nil {
		return fn.Err[WalletResp](fmt.Errorf(
			"fetch confirmed boarding intents: %w", err,
		))
	}

	return fn.Ok[WalletResp](&GetConfirmedBoardingIntentsResponse{
		Intents: intents,
	})
}

// handleUnregisterNotifier removes an actor from the notification list.
func (a *Ark) handleUnregisterNotifier(ctx context.Context,
	req *UnregisterConfirmationNotifierRequest) fn.Result[WalletResp] {

	_, existed := a.notifiers[req.NotifierID]
	delete(a.notifiers, req.NotifierID)

	a.logger(ctx).InfoS(ctx, "Unregistered confirmation notifier",
		slog.String("notifier_id", req.NotifierID),
		slog.Bool("existed", existed))

	resp := &UnregisterConfirmationNotifierResponse{
		Success: existed,
	}

	return fn.Ok[WalletResp](resp)
}

// handleBlockEpoch processes new block notifications by polling ListUnspent
// for new boarding UTXOs.
func (a *Ark) handleBlockEpoch(ctx context.Context,
	epoch chainsource.BlockEpoch) fn.Result[WalletResp] {

	a.logger(ctx).InfoS(ctx, "Processing new block epoch",
		slog.Int("height", int(epoch.Height)))

	// A new block just arrived, so poll ListUnspent for new UTXOs.
	// Retry a few times because there can be a short lag between
	// receiving the block epoch and the wallet reporting the UTXO with
	// the expected confirmation count.
	var (
		lastUtxos []*Utxo
		foundNew  bool
	)
	for attempt := 0; attempt < listUnspentMaxRetries; attempt++ {
		utxos, err := a.backend.ListUnspent(
			ctx, MinBoardingConfs, MaxConfsForListUnspent,
		)
		if err != nil {
			a.logger(ctx).WarnS(
				ctx, "Failed listing UTXOs", err,
				slog.Int("height", int(epoch.Height)),
			)

			// Return success to avoid disrupting the actor.
			// We'll try again on the next block.
			return fn.Ok[WalletResp](nil)
		}

		lastUtxos = utxos

		// For each UTXO, we'll check if it's new and belongs to a fresh
		// boarding intent, dispatching notifications if needed.
		for _, utxo := range utxos {
			if a.processUtxo(ctx, epoch, utxo) {
				foundNew = true
			}
		}

		if foundNew || attempt == listUnspentMaxRetries-1 {
			break
		}

		timer := time.NewTimer(listUnspentRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fn.Ok[WalletResp](nil)
		case <-timer.C:
		}
	}

	a.logger(ctx).InfoS(ctx, "ListUnspent returned UTXOs",
		slog.Int("height", int(epoch.Height)),
		slog.Int("utxo_count", len(lastUtxos)))

	// Block epoch handling doesn't require a response.
	return fn.Ok[WalletResp](nil)
}

// processUtxo checks if a UTXO is new and belongs to a boarding address.
func (a *Ark) processUtxo(ctx context.Context,
	epoch chainsource.BlockEpoch, utxo *Utxo) bool {

	// Make sure we haven't already seen this UTXO.
	key := NewUtxoKey(utxo.Outpoint)
	if a.seenUtxos.Contains(key) {
		return false
	}

	// Check if this UTXO pays to a boarding address.
	addr, err := a.store.LookupBoardingAddress(ctx, utxo.PkScript)
	if err != nil {
		// Not a boarding address, ignore.
		return false
	}

	// New boarding UTXO detected!
	a.logger(ctx).InfoS(ctx, "Detected new boarding UTXO",
		btclog.Fmt("outpoint", "%v", utxo.Outpoint),
		slog.Int("amount", int(utxo.Amount)),
		slog.Int("height", int(epoch.Height)),
	)

	// Fetch the full transaction and its confirmation metadata. The
	// TxInfo block hash and height reflect the UTXO's actual
	// confirmation block, which may differ from the epoch during
	// catch-up after downtime.
	txInfo, err := a.backend.GetTransaction(
		ctx, utxo.Outpoint.Hash,
	)
	if err != nil {
		a.logger(ctx).WarnS(
			ctx, "Failed fetching boarding tx", err,
			btclog.Fmt("txid", "%v", utxo.Outpoint.Hash),
		)

		return false
	}

	// Use the confirmation block hash and height from the
	// transaction if available. Fall back to epoch values for
	// backends that don't provide confirmation metadata (e.g.,
	// unconfirmed or missing details).
	blockHash := epoch.Hash
	blockHeight := epoch.Height
	if txInfo.BlockHash != nil {
		blockHash = *txInfo.BlockHash
		blockHeight = txInfo.BlockHeight
	}

	// Build the SPV TxProof so the server can verify the boarding
	// UTXO without querying its own chain source.
	txProof := a.buildBoardingTxProof(
		ctx, blockHash, blockHeight, txInfo.Tx,
		utxo.Outpoint, addr,
	)

	intent := BoardingIntent{
		Address:  *addr,
		Outpoint: utxo.Outpoint,
		ChainInfo: BoardingChainInfo{
			ConfHeight: blockHeight,
			ConfHash:   blockHash,
			ConfTx:     txInfo.Tx,
			OutPoint:   utxo.Outpoint,
			Amount:     utxo.Amount,
			TxProof:    txProof,
		},
		Status: BoardingStatusConfirmed,
	}

	// Persist first - only mark seen and notify if DB succeeds. On failure
	// we'll retry on the next block when ListUnspent returns this UTXO
	// again.
	err = a.store.InsertBoardingIntents(ctx, intent)
	if err != nil {
		a.logger(ctx).WarnS(
			ctx, "Failed persisting boarding intent", err,
			btclog.Fmt("outpoint", "%v", utxo.Outpoint),
		)

		return false
	}

	a.seenUtxos.Add(key)

	// Mirror the confirmation into the client ledger so the UTXO
	// audit log has a deposit row alongside the double-entry
	// bookkeeping. Classification is ClassificationDeposit because
	// the detection path above filtered for UTXOs paying to a
	// known boarding address -- other classifications (change,
	// sweep_return) belong to different emission sites and are
	// not applicable here.
	a.emitUTXOCreated(ctx, utxo, blockHeight,
		ledger.ClassificationDeposit)

	// Notify registered actors that meet the confirmation threshold.
	event := BoardingUtxoConfirmedEvent{
		BoardingIntent: &intent,
	}
	for _, notifier := range a.notifiers {
		if uint32(utxo.Confirmations) >= notifier.minConf {
			if err := notifier.actor.Tell(ctx, event); err != nil {
				a.logger(ctx).WarnS(
					ctx, "Notify confirmation failed",
					err,
				)
			}
		}
	}

	return true
}

// sendBacklog sends recent confirmations to a newly registered notifier. It
// queries confirmed boarding intents and delivers events for those confirmed
// at or after the specified height, allowing newly registered actors to catch
// up on missed confirmations.
func (a *Ark) sendBacklog(ctx context.Context,
	notifier actor.TellOnlyRef[BoardingUtxoConfirmedEvent],
	fromHeight int32) {

	// Query confirmed intents with height filter applied at the database
	// level for efficiency.
	intents, err := a.store.FetchBoardingIntentsByStatusAndMinHeight(
		ctx, BoardingStatusConfirmed, fromHeight,
	)
	if err != nil {
		a.logger(ctx).WarnS(
			ctx, "Failed fetching confirmed intents",
			err,
		)

		return
	}

	for i := range intents {
		intent := &intents[i]

		event := BoardingUtxoConfirmedEvent{
			BoardingIntent: intent,
		}

		if err := notifier.Tell(ctx, event); err != nil {
			a.logger(ctx).WarnS(
				ctx, "Backlog delivery failed", err,
			)
		}
	}

	a.logger(ctx).InfoS(ctx, "Backlog delivery completed",
		slog.Int("from_height", int(fromHeight)),
		slog.Int("events_sent", len(intents)))
}

// handleRefreshVTXOs processes a request to refresh VTXOs. The wallet loads
// each VTXO descriptor, builds a forfeit + VTXO request pair, and sends the
// composed intent package to the round actor via RegisterIntentMsg. The round
// actor validates, registers with the FSM, and notifies VTXO actors.
//
// If some VTXOs fail to load but at least one succeeds, the successful
// forfeits are still submitted to the round (partial participation). The
// caller should check Errors in the response to detect partial failures.
func (a *Ark) handleRefreshVTXOs(ctx context.Context,
	req *RefreshVTXOsRequest) fn.Result[WalletResp] {

	a.logger(ctx).InfoS(ctx, "Received VTXO refresh request",
		slog.Int("target_count", len(req.TargetOutpoints)),
		slog.Bool("force_refresh", req.ForceRefresh))

	if a.actorSystem == nil {
		a.logger(ctx).WarnS(
			ctx, "No actor system for refresh", nil,
		)

		return fn.Ok[WalletResp](&RefreshVTXOsResponse{
			Errors: make(map[wire.OutPoint]error),
		})
	}

	if a.vtxoReader == nil {
		err := fmt.Errorf("VTXO reader not configured")
		a.logger(ctx).WarnS(ctx, "No VTXO reader for refresh", err)

		return fn.Ok[WalletResp](&RefreshVTXOsResponse{
			Errors: allTargetErrors(req.TargetOutpoints, err),
		})
	}

	// Build intent package by loading each VTXO descriptor and
	// constructing the corresponding forfeit + VTXO request pair.
	var (
		forfeits []types.ForfeitRequest
		vtxos    []types.VTXORequest
		errors   = make(map[wire.OutPoint]error)
	)

	for _, outpoint := range req.TargetOutpoints {
		vtxo, err := a.vtxoReader.GetVTXO(ctx, outpoint)
		if err != nil {
			a.logger(ctx).WarnS(ctx,
				"Failed to load VTXO for refresh",
				err,
				slog.String("outpoint",
					outpoint.String()))

			errors[outpoint] = err

			continue
		}

		// Validate the per-input operator fee up front so a
		// rejected outpoint never lands in the forfeits slice
		// without a matching VTXO request. A mixed-outcome
		// request with one valid and one invalid outpoint would
		// otherwise ship a Forfeits/VTXOs mismatch to
		// RegisterIntentMsg and reserve a VTXO into
		// PendingForfeitState with no replacement.
		policyTemplate, err := vtxo.EffectivePolicyTemplate()
		if err != nil {
			a.logger(ctx).WarnS(ctx,
				"Failed to load refresh policy",
				err,
				slog.String("outpoint",
					outpoint.String()))

			errors[outpoint] = err

			continue
		}

		// Under the #270 seal-time fee handshake the client no
		// longer subtracts an operator fee from the new VTXO at
		// intent-compose time: the target amount carries the
		// pre-fee value, and exactly one output across the FULL
		// composed intent (boarding + refresh + leave + directed
		// send) is marked IsChange=true to absorb the residual
		// (Σin − Σ(fixed) − fee) at seal time. The marker is
		// stamped centrally by the FSM's IntentRequested handler
		// (designateChangeMarker) over the fully-accumulated
		// intent — stamping here using `len(vtxos) == 0` would
		// only see this RPC's batch and produce two markers when
		// two RefreshVTXOs RPCs land back-to-back during the
		// same PendingRoundAssembly window.
		op := vtxo.Outpoint
		forfeits = append(forfeits, types.ForfeitRequest{
			VTXOOutpoint: &op,
			Amount:       vtxo.Amount,
		})
		vtxos = append(vtxos, types.VTXORequest{
			PolicyTemplate: policyTemplate,
			Amount:         vtxo.Amount,
			OwnerKey:       vtxo.ClientKey,
			SigningKey:     vtxo.ClientKey,
			// Refresh output: the new VTXO is funded by the
			// client's own forfeited VTXO (not by wallet or
			// an external party). Origin drives the ledger
			// emission to SourceRoundRefresh so the
			// VTXOReceived credit cancels the paired VTXOSent
			// debit on transfers_out, leaving only the
			// operator fee as the net vtxo_balance change.
			Origin: types.VTXOOriginRoundRefresh,
		})
	}

	// Reserve the forfeit inputs through the VTXO manager before
	// sending the intent to the round actor. This ensures the VTXO
	// actors are in PendingForfeitState before round registration,
	// preventing split-brain where the round has an intent for a
	// VTXO that is still Live or claimed for OOR spend.
	if len(forfeits) > 0 {
		reserveOutpoints := make(
			[]wire.OutPoint, 0, len(forfeits),
		)
		for _, f := range forfeits {
			if f.VTXOOutpoint != nil {
				reserveOutpoints = append(
					reserveOutpoints,
					*f.VTXOOutpoint,
				)
			}
		}

		_, err := a.askManager(
			ctx, &actormsg.ReserveForfeitRequest{
				Outpoints: reserveOutpoints,
			},
		)
		if err != nil {
			a.logger(ctx).WarnS(ctx,
				"Manager rejected refresh reservation",
				err)

			return fn.Err[WalletResp](fmt.Errorf(
				"reserve refresh inputs: %w", err,
			))
		}

		// Send the intent to the round actor. If registration
		// fails, release the forfeit reservation so VTXOs
		// return to LiveState.
		serviceKey := actormsg.RoundActorServiceKey()
		roundRef := serviceKey.Ref(a.actorSystem)

		future := roundRef.Ask(ctx, &actormsg.RegisterIntentMsg{
			Forfeits: forfeits,
			VTXOs:    vtxos,
		})
		result := future.Await(ctx)
		if result.IsErr() {
			a.logger(ctx).WarnS(ctx,
				"Round rejected refresh intent",
				result.Err())

			a.releaseManagerForfeit(
				ctx, reserveOutpoints,
			)

			return fn.Err[WalletResp](fmt.Errorf(
				"round rejected refresh intent: %w",
				result.Err(),
			))
		}
	}

	a.logger(ctx).InfoS(ctx, "Registered refresh intent package",
		slog.Int("forfeits", len(forfeits)),
		slog.Int("vtxos", len(vtxos)),
		slog.Int("errors", len(errors)))

	resp := &RefreshVTXOsResponse{
		RefreshingCount: len(forfeits),
		Errors:          errors,
	}

	return fn.Ok[WalletResp](resp)
}

// handleLeaveVTXOs processes a leave (offboard) request. The wallet loads each
// VTXO descriptor, builds a forfeit + leave request pair, and sends the
// composed intent package to the round actor via RegisterIntentMsg. The round
// actor validates, registers with the FSM, and notifies VTXO actors.
//
// If some VTXOs fail to load but at least one succeeds, the successful
// forfeits are still submitted (partial participation). The caller should
// check Errors in the response to detect partial failures.
//
// Each target produces its own LeaveRequest whose pkScript is taken
// from req.DestOutputs[outpoint] (per-outpoint override) when the
// entry is set, falling back to the singular req.DestOutput when it
// is not. A target with no destination on either side surfaces a
// per-outpoint error instead of panicking — the RPC layer is
// responsible for guaranteeing coverage before dispatch. The server
// creates a separate on-chain output for each leave; it does not
// aggregate them.
func (a *Ark) handleLeaveVTXOs(ctx context.Context,
	req *LeaveVTXOsRequest) fn.Result[WalletResp] {

	a.logger(ctx).InfoS(ctx, "Received VTXO leave request",
		slog.Int("target_count", len(req.TargetOutpoints)))

	if a.actorSystem == nil {
		a.logger(ctx).WarnS(
			ctx, "No actor system for leave", nil,
		)

		return fn.Ok[WalletResp](&LeaveVTXOsResponse{
			Errors: make(map[wire.OutPoint]error),
		})
	}

	if a.vtxoReader == nil {
		err := fmt.Errorf("VTXO reader not configured")
		a.logger(ctx).WarnS(ctx, "No VTXO reader for leave", err)

		return fn.Ok[WalletResp](&LeaveVTXOsResponse{
			Errors: allTargetErrors(req.TargetOutpoints, err),
		})
	}

	// Build intent package by loading each VTXO descriptor and
	// constructing the corresponding forfeit + leave request pair.
	var (
		forfeits []types.ForfeitRequest
		leaves   []*types.LeaveRequest
		errors   = make(map[wire.OutPoint]error)
	)

	for _, outpoint := range req.TargetOutpoints {
		vtxo, err := a.vtxoReader.GetVTXO(ctx, outpoint)
		if err != nil {
			a.logger(ctx).WarnS(ctx,
				"Failed to load VTXO for leave",
				err,
				slog.String("outpoint",
					outpoint.String()))

			errors[outpoint] = err

			continue
		}

		// Under the #270 seal-time fee handshake the leave
		// output carries the forfeited VTXO's full target
		// amount; exactly one output across the FULL composed
		// intent (boarding + refresh + leave + directed send)
		// is marked IsChange=true to absorb the residual
		// (Σin − Σ(fixed) − fee) at seal time. The marker is
		// stamped centrally by the FSM's IntentRequested handler
		// (designateChangeMarker) over the fully-accumulated
		// intent — stamping here using `len(leaves) == 0` would
		// only see this RPC's batch and produce two markers when
		// two LeaveVTXOs RPCs land back-to-back during the same
		// PendingRoundAssembly window.
		op := outpoint

		// Pick the destination: per-outpoint override from
		// DestOutputs takes precedence so a single batch can
		// offboard to distinct on-chain targets; otherwise the
		// caller's singular DestOutput applies. A missing entry
		// on both sides is a misuse by the RPC layer (which is
		// responsible for guaranteeing every target has a
		// destination before dispatch), so surface a clean
		// per-outpoint error rather than panicking.
		leaveOutput := req.DestOutputs[op]
		if leaveOutput == nil {
			leaveOutput = req.DestOutput
		}
		if leaveOutput == nil {
			errors[outpoint] = fmt.Errorf(
				"no destination for outpoint %s",
				outpoint,
			)

			continue
		}

		forfeits = append(forfeits, types.ForfeitRequest{
			VTXOOutpoint: &op,
			Amount:       vtxo.Amount,
		})
		leaves = append(leaves, &types.LeaveRequest{
			Output: &wire.TxOut{
				PkScript: leaveOutput.PkScript,
				Value:    int64(vtxo.Amount),
			},
		})
	}

	// Reserve the forfeit inputs through the VTXO manager before
	// sending the intent to the round actor. This ensures the VTXO
	// actors are in PendingForfeitState before round registration.
	if len(forfeits) > 0 {
		reserveOutpoints := make(
			[]wire.OutPoint, 0, len(forfeits),
		)
		for _, f := range forfeits {
			if f.VTXOOutpoint != nil {
				reserveOutpoints = append(
					reserveOutpoints,
					*f.VTXOOutpoint,
				)
			}
		}

		_, err := a.askManager(
			ctx, &actormsg.ReserveForfeitRequest{
				Outpoints: reserveOutpoints,
			},
		)
		if err != nil {
			a.logger(ctx).WarnS(ctx,
				"Manager rejected leave reservation",
				err)

			return fn.Err[WalletResp](fmt.Errorf(
				"reserve leave inputs: %w", err,
			))
		}

		// Send the intent to the round actor. If registration
		// fails, release the forfeit reservation so VTXOs
		// return to LiveState.
		serviceKey := actormsg.RoundActorServiceKey()
		roundRef := serviceKey.Ref(a.actorSystem)

		future := roundRef.Ask(ctx, &actormsg.RegisterIntentMsg{
			Forfeits: forfeits,
			Leaves:   leaves,
		})
		result := future.Await(ctx)
		if result.IsErr() {
			a.logger(ctx).WarnS(ctx,
				"Round rejected leave intent",
				result.Err())

			a.releaseManagerForfeit(
				ctx, reserveOutpoints,
			)

			return fn.Err[WalletResp](fmt.Errorf(
				"round rejected leave intent: %w",
				result.Err(),
			))
		}
	}

	a.logger(ctx).InfoS(ctx, "Registered leave intent package",
		slog.Int("forfeits", len(forfeits)),
		slog.Int("leaves", len(leaves)),
		slog.Int("errors", len(errors)))

	resp := &LeaveVTXOsResponse{
		LeavingCount: len(forfeits),
		Errors:       errors,
	}

	return fn.Ok[WalletResp](resp)
}

// handleBoard processes a boarding request by checking the confirmed balance,
// computing the requested VTXO output target amounts, and forwarding a
// TriggerBoardMsg to the round actor. The round actor handles the actual
// registration and FSM transitions asynchronously.
func (a *Ark) handleBoard(ctx context.Context,
	req *BoardRequest) fn.Result[WalletResp] {

	// Fetch confirmed boarding balance from the store.
	status := BoardingStatusConfirmed
	intents, err := a.store.FetchBoardingIntentsByStatus(ctx, status)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("fetch boarding intents: %w", err),
		)
	}

	var totalBalance btcutil.Amount
	for _, intent := range intents {
		totalBalance += intent.ChainInfo.Amount
	}

	if totalBalance == 0 {
		return fn.Err[WalletResp](
			fmt.Errorf("no confirmed boarding balance"),
		)
	}

	// Under the #270 seal-time fee handshake the server decides
	// the operator fee when the round seals, not at submit time.
	// The wallet therefore ships the full confirmed balance as
	// one or more VTXO intent targets. For multi-output boarding,
	// the common change-marker logic marks one output as the
	// residual output the server can stamp at seal time. We skip
	// the pre-#270 `vtxoAmount <= DustLimit` gate because it was
	// driven by an advisory submit-time fee estimate and would
	// spuriously reject boards that the seal-time quote would
	// have accepted.
	vtxoAmounts, err := splitBoardingAmount(
		totalBalance, req.TargetVTXOCount,
	)
	if err != nil {
		return fn.Err[WalletResp](err)
	}
	vtxoAmount := sumBoardingAmounts(vtxoAmounts)

	a.logger(ctx).InfoS(ctx, "Boarding request accepted",
		slog.Int64("boarding_balance",
			int64(totalBalance)),
		slog.Int64("vtxo_amount", int64(vtxoAmount)),
		slog.Int("vtxo_count", len(vtxoAmounts)))

	// Forward to round actor via service key lookup. The round actor
	// registers the VTXO output requests and triggers the round join.
	if a.actorSystem == nil {
		return fn.Err[WalletResp](
			fmt.Errorf("no actor system available for board"),
		)
	}

	serviceKey := actormsg.RoundActorServiceKey()
	roundRef := serviceKey.Ref(a.actorSystem)

	if err := roundRef.Tell(
		ctx, &actormsg.TriggerBoardMsg{
			Amounts: vtxoAmounts,
		},
	); err != nil {
		return fn.Err[WalletResp](fmt.Errorf(
			"forward board to round actor: %w", err,
		))
	}

	resp := &BoardResponse{
		BoardingBalance: totalBalance,
		VTXOAmount:      vtxoAmount,
		VTXOAmounts:     vtxoAmounts,
	}

	return fn.Ok[WalletResp](resp)
}

// splitBoardingAmount fans a confirmed boarding balance into count VTXO
// target amounts. A zero count preserves the legacy single-output behavior.
func splitBoardingAmount(total btcutil.Amount,
	count uint32) ([]btcutil.Amount, error) {

	if count == 0 {
		count = 1
	}
	if total <= 0 {
		return nil, fmt.Errorf("boarding balance must be positive")
	}

	base := int64(total) / int64(count)
	remainder := int64(total) % int64(count)
	if base <= 0 {
		return nil, fmt.Errorf(
			"boarding balance %v too small for %d VTXOs",
			total, count,
		)
	}

	amounts := make([]btcutil.Amount, count)
	for i := range amounts {
		amount := base
		if int64(i) < remainder {
			amount++
		}

		amounts[i] = btcutil.Amount(amount)
	}

	return amounts, nil
}

// sumBoardingAmounts returns the total of boarding target amounts.
func sumBoardingAmounts(amounts []btcutil.Amount) btcutil.Amount {
	var total btcutil.Amount
	for _, amount := range amounts {
		total += amount
	}

	return total
}

// buildBoardingTxProof fetches the confirmation block and computes a merkle
// inclusion proof for the boarding transaction. The blockHash parameter should
// be the UTXO's actual confirmation block (from GetTransaction), not the
// current epoch block, so proofs are correct even during catch-up. If anything
// fails, it returns None — the intent will still be persisted, but without a
// proof the server will need its own chain source to validate.
func (a *Ark) buildBoardingTxProof(ctx context.Context,
	blockHash chainhash.Hash, blockHeight int32,
	confTx *wire.MsgTx, outpoint wire.OutPoint,
	addr *BoardingAddress) fn.Option[proof.TxProof] {

	// Fetch the full block to compute the merkle proof.
	block, err := a.backend.GetBlock(ctx, blockHash)
	if err != nil {
		a.logger(ctx).WarnS(
			ctx, "Failed fetching block for TxProof", err,
			btclog.Fmt("block_hash", "%v", blockHash),
		)

		return fn.None[proof.TxProof]()
	}

	// Find the transaction index within the block.
	txHash := confTx.TxHash()
	txIdx := -1
	for i, blockTx := range block.Transactions {
		if blockTx.TxHash() == txHash {
			txIdx = i
			break
		}
	}
	if txIdx < 0 {
		a.logger(ctx).WarnS(
			ctx, "Boarding tx not found in block", nil,
			btclog.Fmt("txid", "%v", txHash),
			btclog.Fmt("block_hash", "%v", blockHash),
		)

		return fn.None[proof.TxProof]()
	}

	// Compute the merkle inclusion proof.
	merkleProof, err := proof.NewTxMerkleProof(
		block.Transactions, txIdx,
	)
	if err != nil {
		a.logger(ctx).WarnS(
			ctx, "Failed computing merkle proof", err,
			btclog.Fmt("txid", "%v", txHash),
		)

		return fn.None[proof.TxProof]()
	}

	// Extract the internal key and tapscript root hash from the boarding
	// address. VTXOTapScript populates both ControlBlock.InternalKey and
	// RootHash when constructing the tapscript.
	if addr.Tapscript == nil || addr.Tapscript.ControlBlock == nil ||
		addr.Tapscript.ControlBlock.InternalKey == nil {

		a.logger(ctx).WarnS(
			ctx, "Boarding address missing tapscript data",
			nil,
		)

		return fn.None[proof.TxProof]()
	}
	internalKey := addr.Tapscript.ControlBlock.InternalKey
	merkleRoot := addr.Tapscript.RootHash

	a.logger(ctx).InfoS(ctx, "Built TxProof for boarding UTXO",
		btclog.Fmt("outpoint", "%v", outpoint),
		slog.Int("block_height", int(blockHeight)),
	)

	return fn.Some(proof.TxProof{
		MsgTx:           *confTx,
		BlockHeader:     block.Header,
		BlockHeight:     uint32(blockHeight),
		MerkleProof:     *merkleProof,
		ClaimedOutPoint: outpoint,
		InternalKey:     *internalKey,
		MerkleRoot:      merkleRoot,
	})
}

// =============================================================================
// VTXO admission forwarding handlers
// =============================================================================
//
// These handlers forward admission requests to the VTXO manager actor via
// service key lookup. The manager owns the actual coin selection, reservation,
// and rollback logic; the wallet is a thin forwarding layer that translates
// between wallet messages and actormsg admission types.

// askManager sends a VTXOManagerMsg to the VTXO manager via service key and
// returns the response. This is a convenience wrapper around the Ask/Await
// pattern that reduces boilerplate at each call site.
func (a *Ark) askManager(ctx context.Context,
	msg actormsg.VTXOManagerMsg) (actormsg.VTXOManagerResp, error) {

	if a.actorSystem == nil {
		return nil, fmt.Errorf("actor system not configured")
	}

	serviceKey := actormsg.VTXOManagerServiceKey()
	managerRef := serviceKey.Ref(a.actorSystem)

	future := managerRef.Ask(ctx, msg)
	result := future.Await(ctx)

	return result.Unpack()
}

// releaseManagerForfeit is a best-effort helper that releases forfeit
// reservations when round registration fails after successful admission.
// Errors are logged but not propagated because the primary error (round
// rejection) has already been captured.
func (a *Ark) releaseManagerForfeit(ctx context.Context,
	outpoints []wire.OutPoint) {

	_, err := a.askManager(
		ctx, &actormsg.ReleaseForfeitRequest{
			Outpoints: outpoints,
		},
	)
	if err != nil {
		a.logger(ctx).WarnS(ctx,
			"Failed to release forfeit reservation", err)
	}
}

// handleSelectAndLockVTXOs forwards a spend selection request to the VTXO
// manager. The manager runs largest-first coin selection and atomically
// reserves VTXOs for OOR spending by transitioning them to SpendingState.
func (a *Ark) handleSelectAndLockVTXOs(ctx context.Context,
	req *SelectAndLockVTXOsRequest) fn.Result[WalletResp] {

	a.logger(ctx).InfoS(ctx, "Selecting and locking VTXOs for spend",
		slog.Int64("target", int64(req.TargetAmount)))

	resp, err := a.askManager(
		ctx, &actormsg.SelectAndReserveSpendRequest{
			TargetAmount: req.TargetAmount,
		},
	)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("select and reserve: %w", err),
		)
	}

	//nolint:forcetypeassert
	mgrResp := resp.(*actormsg.SelectAndReserveSpendResponse)

	// Translate manager response to wallet response.
	selected := make([]SelectedVTXO, len(mgrResp.SelectedVTXOs))
	for i, v := range mgrResp.SelectedVTXOs {
		selected[i] = SelectedVTXO{
			Outpoint: v.Outpoint,
			Amount:   v.Amount,
			PkScript: v.PkScript,
		}
	}

	a.logger(ctx).InfoS(ctx, "VTXOs selected and locked",
		slog.Int("count", len(selected)),
		slog.Int64("total", int64(mgrResp.TotalSelected)))

	return fn.Ok[WalletResp](&SelectAndLockVTXOsResponse{
		SelectedVTXOs: selected,
		TotalSelected: mgrResp.TotalSelected,
	})
}

// handleUnlockVTXOs forwards a spend release request to the VTXO manager.
// This transitions VTXOs from SpendingState back to LiveState when an OOR
// transfer fails or is cancelled.
func (a *Ark) handleUnlockVTXOs(ctx context.Context,
	req *UnlockVTXOsRequest) fn.Result[WalletResp] {

	a.logger(ctx).InfoS(ctx, "Unlocking VTXOs from spend reservation",
		slog.Int("count", len(req.Outpoints)))

	resp, err := a.askManager(
		ctx, &actormsg.ReleaseSpendRequest{
			Outpoints: req.Outpoints,
		},
	)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("release spend: %w", err),
		)
	}

	//nolint:forcetypeassert
	mgrResp := resp.(*actormsg.ReleaseSpendResponse)

	return fn.Ok[WalletResp](&UnlockVTXOsResponse{
		UnlockedCount: mgrResp.ReleasedCount,
	})
}

// handleCompleteSpendVTXOs forwards a spend completion request to the VTXO
// manager. This transitions VTXOs from SpendingState to terminal SpentState
// after an OOR transfer succeeds.
func (a *Ark) handleCompleteSpendVTXOs(ctx context.Context,
	req *CompleteSpendVTXOsRequest) fn.Result[WalletResp] {

	a.logger(ctx).InfoS(ctx, "Completing spend for VTXOs",
		slog.Int("count", len(req.Outpoints)))

	resp, err := a.askManager(
		ctx, &actormsg.CompleteSpendRequest{
			Outpoints: req.Outpoints,
		},
	)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("complete spend: %w", err),
		)
	}

	//nolint:forcetypeassert
	mgrResp := resp.(*actormsg.CompleteSpendResponse)

	a.logger(ctx).InfoS(ctx, "Spend completion confirmed",
		slog.Int("completed", mgrResp.CompletedCount))

	return fn.Ok[WalletResp](&CompleteSpendVTXOsResponse{
		CompletedCount: mgrResp.CompletedCount,
	})
}

// handleSendVTXOs processes an in-round directed send. It atomically
// selects and reserves VTXOs for cooperative consumption, builds an
// IntentPackage with forfeits + recipient VTXOs + change, and
// registers it with the round actor. On failure, all reservations are
// released. For dry-run, the reservation is immediately released
// after validation.
func (a *Ark) handleSendVTXOs(ctx context.Context,
	req *SendVTXOsRequest) fn.Result[WalletResp] {

	// Validate recipients.
	if len(req.Recipients) == 0 {
		return fn.Err[WalletResp](
			fmt.Errorf("no recipients provided"),
		)
	}

	var totalRecipientAmount btcutil.Amount
	for i, r := range req.Recipients {
		if len(r.PkScript) == 0 {
			return fn.Err[WalletResp](fmt.Errorf(
				"recipient %d: empty pk_script", i,
			))
		}

		if r.Amount <= 0 || r.Amount > btcutil.MaxSatoshi {
			return fn.Err[WalletResp](fmt.Errorf(
				"recipient %d: amount must be "+
					"between 1 and %d",
				i, int64(btcutil.MaxSatoshi),
			))
		}

		if totalRecipientAmount+r.Amount < 0 {
			return fn.Err[WalletResp](fmt.Errorf(
				"total recipient amount overflows",
			))
		}

		totalRecipientAmount += r.Amount
	}

	totalNeeded := totalRecipientAmount + req.OperatorFee

	a.logger(ctx).InfoS(ctx, "Processing directed send",
		slog.Int("recipients", len(req.Recipients)),
		slog.Int64("total_amount",
			int64(totalRecipientAmount)),
		slog.Int64("operator_fee",
			int64(req.OperatorFee)),
		slog.Bool("dry_run", req.DryRun))

	// Atomic select-and-reserve for cooperative consumption.
	resp, err := a.askManager(
		ctx,
		&actormsg.SelectAndReserveForfeitRequest{
			TargetAmount: totalNeeded,
		},
	)
	if err != nil {
		return fn.Err[WalletResp](fmt.Errorf(
			"select and reserve forfeit: %w", err,
		))
	}

	//nolint:forcetypeassert
	mgrResp := resp.(*actormsg.SelectAndReserveForfeitResponse)

	// Collect reserved outpoints for potential rollback.
	reservedOutpoints := make(
		[]wire.OutPoint, 0, len(mgrResp.SelectedVTXOs),
	)
	for _, v := range mgrResp.SelectedVTXOs {
		reservedOutpoints = append(
			reservedOutpoints, v.Outpoint,
		)
	}

	// Ensure reserved VTXOs are released if we don't reach the
	// successful registration at the end. Use a background
	// context so cleanup survives client disconnection.
	committed := false
	defer func() {
		if committed {
			return
		}

		releaseCtx := context.WithoutCancel(ctx)
		releaseErr := a.releaseManagerForfeitStrict(
			releaseCtx, reservedOutpoints,
		)
		if releaseErr != nil {
			a.logger(releaseCtx).WarnS(
				releaseCtx,
				"Failed to release reserved "+
					"VTXOs", releaseErr,
			)
		}
	}()

	// Compute change.
	change := mgrResp.TotalSelected - totalNeeded
	if change < 0 {
		// Should not happen since coin selection covers the
		// target, but be defensive.
		return fn.Err[WalletResp](fmt.Errorf(
			"selection shortfall: selected %d, need %d",
			mgrResp.TotalSelected, totalNeeded,
		))
	}

	if change > 0 && change <= req.DustLimit {
		return fn.Err[WalletResp](fmt.Errorf(
			"change %d is below dust limit %d; "+
				"adjust send amount",
			change, req.DustLimit,
		))
	}

	// Under the #270 fixed-output contract a multi-output intent
	// must carry exactly one IsChange=true marker so the server
	// knows which output absorbs the seal-time residual. When
	// coin selection covers the target exactly (change == 0) for
	// a multi-recipient send, no self-change output exists and the
	// intent would ship with zero markers — the server admission
	// path rejects that with INVALID_CHANGE_DESIGNATION, burning
	// a round slot for a deterministic failure. Surface the
	// mismatch locally instead; the operator can retry with a
	// value that allows change, or split the send.
	if change == 0 && len(req.Recipients) > 1 {
		return fn.Err[WalletResp](fmt.Errorf(
			"multi-recipient send must leave change for " +
				"the seal-time fee marker: coin " +
				"selection covered the target exactly",
		))
	}

	// Dry-run: validate coin selection then release immediately.
	if req.DryRun {
		// The deferred cleanup releases the reservation.
		return fn.Ok[WalletResp](&SendVTXOsResponse{
			Status:        "preview",
			SelectedCount: len(mgrResp.SelectedVTXOs),
			TotalSelected: mgrResp.TotalSelected,
			ChangeAmount:  change,
		})
	}

	// Build the intent package.
	forfeits := make(
		[]types.ForfeitRequest, 0,
		len(mgrResp.SelectedVTXOs),
	)
	for _, v := range mgrResp.SelectedVTXOs {
		op := v.Outpoint
		forfeits = append(forfeits, types.ForfeitRequest{
			VTXOOutpoint: &op,
			Amount:       v.Amount,
		})
	}

	// Build recipient + change VTXOs with fresh signing keys.
	vtxoRequests, buildErr := a.buildSendVTXORequests(
		ctx, req, change,
	)
	if buildErr != nil {
		return fn.Err[WalletResp](buildErr)
	}

	// Register the intent with the round actor.
	serviceKey := actormsg.RoundActorServiceKey()
	roundRef := serviceKey.Ref(a.actorSystem)

	future := roundRef.Ask(ctx, &actormsg.RegisterIntentMsg{
		Forfeits:            forfeits,
		VTXOs:               vtxoRequests,
		TriggerRegistration: true,
	})
	result := future.Await(ctx)
	if result.IsErr() {
		a.logger(ctx).WarnS(ctx,
			"Round rejected send intent", result.Err())

		return fn.Err[WalletResp](fmt.Errorf(
			"round rejected send intent: %w",
			result.Err(),
		))
	}

	committed = true

	a.logger(ctx).InfoS(ctx, "Directed send intent registered",
		slog.Int("forfeits", len(forfeits)),
		slog.Int("recipient_vtxos", len(req.Recipients)),
		slog.Int64("change", int64(change)))

	return fn.Ok[WalletResp](&SendVTXOsResponse{
		Status:        "submitted",
		SelectedCount: len(mgrResp.SelectedVTXOs),
		TotalSelected: mgrResp.TotalSelected,
		ChangeAmount:  change,
	})
}

// buildSendVTXORequests assembles VTXORequest entries for each recipient plus
// an optional change output. Recipient requests carry only the semantic
// policy and public owner key, while locally owned change also retains the
// owner descriptor so confirmation can persist it correctly.
func (a *Ark) buildSendVTXORequests(ctx context.Context,
	req *SendVTXOsRequest,
	change btcutil.Amount) ([]types.VTXORequest, error) {

	vtxoRequests := make(
		[]types.VTXORequest, 0,
		len(req.Recipients)+1,
	)
	for i, r := range req.Recipients {
		// Derive the VTXO policy template and pkScript from
		// (ownerKey, operatorKey, exitDelay). Signing keys are
		// NOT derived here — the round FSM derives them during
		// the RegistrationSent transition per #210.
		policyTemplate, pkScript, err := arkscript.
			EncodeStandardVTXOArtifacts(
				r.ClientKey, req.OperatorKey,
				req.VTXOExitDelay,
			)
		if err != nil {
			return nil, fmt.Errorf(
				"build recipient %d descriptor: %w",
				i, err,
			)
		}

		vtxoRequests = append(vtxoRequests, types.VTXORequest{
			Amount:         r.Amount,
			PolicyTemplate: policyTemplate,
			PkScript:       pkScript,
			Expiry:         req.VTXOExitDelay,
			ClientKey:      r.ClientKey,
			OperatorKey:    req.OperatorKey,
		})
	}

	// Add change VTXO if needed. The sender owns the change, so
	// keep the long-lived owner descriptor locally even though
	// only the pubkey goes on the wire.
	if change > 0 {
		changeClientKey, keyErr := a.backend.DeriveNextKey(
			ctx, types.VTXOOwnerKeyFamily,
		)
		if keyErr != nil {
			return nil, fmt.Errorf(
				"derive change client key: %w",
				keyErr,
			)
		}

		policyTemplate, pkScript, err := arkscript.
			EncodeStandardVTXOArtifacts(
				changeClientKey.PubKey,
				req.OperatorKey,
				req.VTXOExitDelay,
			)
		if err != nil {
			return nil, fmt.Errorf(
				"build change descriptor: %w", err,
			)
		}

		vtxoRequests = append(
			vtxoRequests, types.VTXORequest{
				Amount:         change,
				PolicyTemplate: policyTemplate,
				PkScript:       pkScript,
				Expiry:         req.VTXOExitDelay,
				ClientKey:      changeClientKey.PubKey,
				OwnerKey:       *changeClientKey,
				OperatorKey:    req.OperatorKey,
				// Self-change on a directed send: the
				// client forfeits one or more VTXOs and
				// receives part of the value back as
				// change. Same ledger semantics as a
				// refresh output — the change cancels a
				// portion of the forfeit on transfers_out
				// rather than counting as a new
				// counterparty receipt. Under #270 the
				// change output is the residual sink for
				// the seal-time quote: IsChange=true tells
				// the server which output to stamp with
				// Σin − Σ(fixed) − fee.
				Origin:   types.VTXOOriginRoundRefresh,
				IsChange: true,
			},
		)
	}

	return vtxoRequests, nil
}

// releaseManagerForfeitStrict releases forfeit reservations and returns
// the error rather than swallowing it. Used by dry-run where release
// failure must be surfaced to the caller.
func (a *Ark) releaseManagerForfeitStrict(ctx context.Context,
	outpoints []wire.OutPoint) error {

	_, err := a.askManager(
		ctx, &actormsg.ReleaseForfeitRequest{
			Outpoints: outpoints,
		},
	)

	return err
}

// buildBoardingTapscript constructs a 2-of-2 tapscript with CSV timeout for
// boarding. The tapscript has two spending paths:
//   - Collaborative: Requires both client and operator signatures (spendable
//     anytime)
//   - Timeout: Requires only client signature after CSV delay (unilateral
//     recovery)
//
// The internal key is the ARK NUMS key (nothing up my sleeve) which has no
// known discrete log, ensuring the key path is unspendable.
func buildBoardingTapscript(clientKey, operatorKey *btcec.PublicKey,
	exitDelay uint32) (*waddrmgr.Tapscript, error) {

	// Use the standard VTXO tapscript construction. Boarding outputs and
	// VTXOs use the same script structure. The client is the "owner" who
	// can recover funds after the CSV delay, and the operator is the
	// "cosigner" who collaborates with the client for immediate spends.
	tapscript, err := arkscript.VTXOTapScript(
		clientKey, operatorKey, exitDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("build VTXO tapscript: %w", err)
	}

	return tapscript, nil
}
