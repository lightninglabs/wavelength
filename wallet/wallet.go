package wallet

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/lib/scripts"
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

	// Subsystem is the log subsystem code for the boarding wallet actor.
	Subsystem = "ARKW"
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

	// roundActor is a reference to the round actor for forwarding refresh
	// requests. The round actor handles VTXO actor coordination.
	roundActor fn.Option[actor.TellOnlyRef[actormsg.RoundReceivable]]

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

	// log is the logger for this actor.
	log btclog.Logger
}

// NewArk creates a new Ark wallet actor. The logger should already have the
// subsystem set (e.g., created via handler.SubSystem(wallet.Subsystem)).
//
// The roundActor parameter is optional - if provided, refresh requests will be
// forwarded to the round actor for VTXO actor coordination.
func NewArk(backend BoardingBackend, store BoardingStore,
	chainSource actor.ActorRef[chainsource.ChainSourceMsg, chainsource.ChainSourceResp],
	roundActor fn.Option[actor.TellOnlyRef[actormsg.RoundReceivable]],
	log btclog.Logger) *Ark {

	return &Ark{
		backend:     backend,
		store:       store,
		chainSource: chainSource,
		roundActor:  roundActor,
		notifiers:   make(map[string]notifierInfo),
		seenUtxos:   fn.NewSet[UtxoKey](),
		log:         log,
	}
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

	a.log.InfoS(ctx, "Loaded boarding addresses from database",
		slog.Int("count", len(addresses)))

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

	a.log.InfoS(ctx, "Loaded existing boarding intents",
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

	a.log.InfoS(ctx, "Boarding wallet actor started")

	return nil
}

// Stop gracefully shuts down the wallet actor by unsubscribing from block
// notifications and waiting for any in-flight backlog deliveries to complete.
func (a *Ark) Stop(ctx context.Context) {
	a.log.InfoS(ctx, "Stopping boarding wallet actor")

	// Cancel the internal context to signal background goroutines to stop.
	if a.cancel != nil {
		a.cancel()
	}

	a.chainSource.Tell(ctx, &chainsource.UnsubscribeBlocksRequest{
		CallerID: "boarding-wallet",
	})

	a.wg.Wait()

	a.log.InfoS(ctx, "Boarding wallet actor stopped")
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

	case *UnregisterConfirmationNotifierRequest:
		return a.handleUnregisterNotifier(ctx, m)

	case BlockEpochNotification:
		return a.handleBlockEpoch(ctx, m.BlockEpoch)

	case *RefreshVTXOsRequest:
		return a.handleRefreshVTXOs(ctx, m)

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

	a.log.InfoS(ctx, "Created new boarding address",
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

	a.log.InfoS(ctx, "Registered confirmation notifier",
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

// handleUnregisterNotifier removes an actor from the notification list.
func (a *Ark) handleUnregisterNotifier(ctx context.Context,
	req *UnregisterConfirmationNotifierRequest) fn.Result[WalletResp] {

	_, existed := a.notifiers[req.NotifierID]
	delete(a.notifiers, req.NotifierID)

	a.log.InfoS(ctx, "Unregistered confirmation notifier",
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

	a.log.InfoS(ctx, "Processing new block epoch",
		slog.Int("height", int(epoch.Height)))

	// A new block just arrived, we'll now poll ListUnspent for any new
	// UTXOs since last time.
	utxos, err := a.backend.ListUnspent(
		ctx, MinBoardingConfs, MaxConfsForListUnspent,
	)
	if err != nil {
		a.log.WarnS(ctx, "Failed to list unspent UTXOs", err,
			slog.Int("height", int(epoch.Height)))

		// Return success to avoid disrupting the actor - we'll try
		// again on the next block.
		return fn.Ok[WalletResp](nil)
	}

	a.log.InfoS(ctx, "ListUnspent returned UTXOs",
		slog.Int("height", int(epoch.Height)),
		slog.Int("utxo_count", len(utxos)))

	// For Each UTXO, we'll check if it's new and belongs to a fresh
	// boarding intent, dispatching notifications if needed.
	for _, utxo := range utxos {
		a.processUtxo(ctx, epoch, utxo)
	}

	// Block epoch handling doesn't require a response.
	return fn.Ok[WalletResp](nil)
}

// processUtxo checks if a UTXO is new and belongs to a boarding address.
func (a *Ark) processUtxo(ctx context.Context,
	epoch chainsource.BlockEpoch, utxo *Utxo) {

	// Make sure we haven't already seen this UTXO.
	key := NewUtxoKey(utxo.Outpoint)
	if a.seenUtxos.Contains(key) {
		return
	}

	// Check if this UTXO pays to a boarding address.
	addr, err := a.store.LookupBoardingAddress(ctx, utxo.PkScript)
	if err != nil {
		// Not a boarding address, ignore.
		return
	}

	// New boarding UTXO detected!
	a.log.InfoS(ctx, "Detected new boarding UTXO",
		btclog.Fmt("outpoint", "%v", utxo.Outpoint),
		slog.Int("amount", int(utxo.Amount)),
		slog.Int("height", int(epoch.Height)),
	)

	// Fetch the full transaction to populate ChainInfo.ConfTx. This is
	// needed by the round FSM to extract the output value.
	confTx, err := a.backend.GetTransaction(ctx, utxo.Outpoint.Hash)
	if err != nil {
		a.log.WarnS(ctx, "Failed to fetch boarding transaction", err,
			btclog.Fmt("txid", "%v", utxo.Outpoint.Hash))

		return
	}

	intent := BoardingIntent{
		Address:  *addr,
		Outpoint: utxo.Outpoint,
		ChainInfo: BoardingChainInfo{
			ConfHeight: epoch.Height,
			ConfHash:   epoch.Hash,
			ConfTx:     confTx,
			OutPoint:   utxo.Outpoint,
			Amount:     utxo.Amount,
		},
		Status: BoardingStatusConfirmed,
	}

	// Persist first - only mark seen and notify if DB succeeds. On failure
	// we'll retry on the next block when ListUnspent returns this UTXO
	// again.
	err = a.store.InsertBoardingIntents(ctx, intent)
	if err != nil {
		a.log.WarnS(ctx, "Failed to persist boarding intent", err,
			btclog.Fmt("outpoint", "%v", utxo.Outpoint))

		return
	}

	a.seenUtxos.Add(key)

	// Notify registered actors that meet the confirmation threshold.
	event := BoardingUtxoConfirmedEvent{
		BoardingIntent: &intent,
	}
	for _, notifier := range a.notifiers {
		if uint32(utxo.Confirmations) >= notifier.minConf {
			notifier.actor.Tell(ctx, event)
		}
	}
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
		a.log.WarnS(ctx, "Failed to fetch confirmed intents for "+
			"backlog", err)
		return
	}

	for i := range intents {
		intent := &intents[i]

		event := BoardingUtxoConfirmedEvent{
			BoardingIntent: intent,
		}

		notifier.Tell(ctx, event)
	}

	a.log.InfoS(ctx, "Backlog delivery completed",
		slog.Int("from_height", int(fromHeight)),
		slog.Int("events_sent", len(intents)))
}

// handleRefreshVTXOs processes a request to refresh VTXOs. This forwards the
// request to the round actor which coordinates with VTXO actors to initiate
// the refresh flow.
func (a *Ark) handleRefreshVTXOs(ctx context.Context,
	req *RefreshVTXOsRequest) fn.Result[WalletResp] {

	a.log.InfoS(ctx, "Received VTXO refresh request",
		slog.Int("target_count", len(req.TargetOutpoints)),
		slog.Bool("force_refresh", req.ForceRefresh),
	)

	// Forward to round actor if configured. The round actor looks up VTXO
	// actors by service key and sends TriggerRefreshEvent to each one.
	a.roundActor.WhenSome(
		func(ref actor.TellOnlyRef[actormsg.RoundReceivable]) {
			ref.Tell(ctx, &actormsg.TriggerVTXORefreshMsg{
				TargetOutpoints: req.TargetOutpoints,
				ForceRefresh:    req.ForceRefresh,
			})
		},
	)

	resp := &RefreshVTXOsResponse{
		RefreshingCount: len(req.TargetOutpoints),
		Errors:          make(map[wire.OutPoint]error),
	}

	return fn.Ok[WalletResp](resp)
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
	tapscript, err := scripts.VTXOTapScript(
		clientKey, operatorKey, exitDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("build VTXO tapscript: %w", err)
	}

	return tapscript, nil
}
