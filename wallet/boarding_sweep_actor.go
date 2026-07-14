package wallet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/txconfirm"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// boardingSweepCallerIDPrefix is the chainsource caller-id prefix
	// used when registering per-input spend watches. The full caller ID
	// is "boarding-sweep-spend:<txid>:<vout>" so unregistration is
	// deterministic.
	boardingSweepCallerIDPrefix = "boarding-sweep-spend"

	// boardingSweepBroadcastLabel is attached to broadcasts of the
	// aggregate sweep transaction for chain-backend-side log
	// correlation.
	boardingSweepBroadcastLabel = "ark boarding timeout sweep"
)

// SweepBoardingUTXOsRequest asks the wallet actor to build, sign, and
// optionally broadcast an aggregate boarding-timeout sweep transaction.
type SweepBoardingUTXOsRequest struct {
	actor.BaseMessage

	// Outpoints optionally restricts the sweep to a specific set of
	// boarding outpoints. When empty the actor walks every sweepable
	// boarding intent.
	Outpoints []wire.OutPoint

	// FeeRateSatPerVByte is the explicit fee rate. When zero the actor
	// falls back to chainsource fee estimation at ConfTarget.
	FeeRateSatPerVByte int64

	// ConfTarget is the confirmation target used when estimating fees.
	ConfTarget uint32

	// SweepAddress is an optional human-readable destination address.
	// When empty and Broadcast=true the wallet allocates a fresh
	// internal address. When empty and Broadcast=false the actor uses
	// a placeholder script for fee estimation.
	SweepAddress string

	// Broadcast controls whether to publish the aggregate sweep. When
	// false the actor returns a preview without persisting or
	// broadcasting.
	Broadcast bool
}

// MessageType returns the message type identifier.
func (m *SweepBoardingUTXOsRequest) MessageType() string {
	return "SweepBoardingUTXOsRequest"
}

func (m *SweepBoardingUTXOsRequest) walletMsgSealed() {}

// SweepBoardingUTXOsResponse is the wallet actor's reply to a
// SweepBoardingUTXOsRequest.
type SweepBoardingUTXOsResponse struct {
	actor.BaseMessage

	// Status is one of "preview", "published", or "failed".
	Status string

	// CurrentHeight is the chain best height observed by the actor when
	// processing this request.
	CurrentHeight int32

	// Txid is the aggregate sweep transaction id, when one was built.
	Txid chainhash.Hash

	// HasTxid is true when Txid is meaningful (i.e. a transaction was
	// built; empty previews leave HasTxid=false).
	HasTxid bool

	// SweepableOutputs carries the boarding outputs that were included
	// in the (preview or published) aggregate sweep.
	SweepableOutputs []BoardingSweepOutput

	// TotalAmountSat is the gross input value.
	TotalAmountSat int64

	// EstimatedFeeSat is the aggregate fee estimate for previews.
	EstimatedFeeSat int64

	// NetAmountSat is TotalAmountSat - EstimatedFeeSat for previews.
	NetAmountSat int64

	// FeePaidSat is the absolute fee for published sweeps.
	FeePaidSat int64

	// FeeRateSatPerVByte is the fee rate used to build the
	// transaction.
	FeeRateSatPerVByte int64

	// ConfTarget is the confirmation target used during fee estimation.
	ConfTarget uint32

	// TxVBytes is the signed-tx virtual size.
	TxVBytes int64

	// FailureReason is populated when Status == "failed".
	FailureReason string
}

// MessageType returns the message type identifier.
func (m *SweepBoardingUTXOsResponse) MessageType() string {
	return "SweepBoardingUTXOsResponse"
}

func (m *SweepBoardingUTXOsResponse) walletRespSealed() {}

// BoardingSweepOutput describes one mature boarding output included in an
// aggregate sweep response.
type BoardingSweepOutput struct {
	// Outpoint is the boarding UTXO outpoint.
	Outpoint wire.OutPoint

	// AmountSat is the boarding UTXO value.
	AmountSat int64

	// MaturityHeight is the first block height at which the timeout
	// path can be spent.
	MaturityHeight int32
}

// ResumeBoardingSweepsRequest is sent (typically self-Tell at startup) to
// re-arm spend watches and re-submit pending sweeps to the broadcaster.
type ResumeBoardingSweepsRequest struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier.
func (m *ResumeBoardingSweepsRequest) MessageType() string {
	return "ResumeBoardingSweepsRequest"
}

func (m *ResumeBoardingSweepsRequest) walletMsgSealed() {}

// ResumeBoardingSweepsResponse reports the number of pending sweeps that
// were resumed.
type ResumeBoardingSweepsResponse struct {
	actor.BaseMessage

	// Resumed is the count of pending sweeps the actor fully recovered
	// (every input watch armed and the sweep tx submitted to txconfirm).
	Resumed int32

	// Failed is the count of pending sweeps that did not fully recover
	// during this resume call (transient chainsource Ask failure, DB
	// error loading an intent, txconfirm submit error, etc.). The next
	// block-epoch tick re-runs the resume flow so partial recoveries
	// converge without operator intervention.
	Failed int32
}

// MessageType returns the message type identifier.
func (m *ResumeBoardingSweepsResponse) MessageType() string {
	return "ResumeBoardingSweepsResponse"
}

func (m *ResumeBoardingSweepsResponse) walletRespSealed() {}

// ReplayPendingIntentsRequest is an Ask the daemon sends to the wallet
// once every dependent actor (in particular the round-client actor)
// is registered, asking it to replay any persisted user intent (Board,
// SendOnChain, ...) issued before the last shutdown. The wallet's
// self-Tell pattern inside Start would otherwise process the replay
// BEFORE the round actor is reachable through the receptionist,
// silently dropping the downstream round-actor dispatch.
type ReplayPendingIntentsRequest struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier.
func (m *ReplayPendingIntentsRequest) MessageType() string {
	return "ReplayPendingIntentsRequest"
}

func (m *ReplayPendingIntentsRequest) walletMsgSealed() {}

// ReplayPendingIntentsResponse acknowledges that the wallet has either
// re-issued at least one persisted intent into its own mailbox or
// determined there is nothing live to replay.
type ReplayPendingIntentsResponse struct {
	actor.BaseMessage

	// Replayed is true when the wallet self-Telled at least one
	// re-issued command as part of replay; false when no live pending
	// intent was found.
	Replayed bool
}

// MessageType returns the message type identifier.
func (m *ReplayPendingIntentsResponse) MessageType() string {
	return "ReplayPendingIntentsResponse"
}

func (m *ReplayPendingIntentsResponse) walletRespSealed() {}

// BoardingSweepSpendNotification is a Tell carrying a chainsource spend
// event for a boarding-sweep input. Emitted by the chainsource subscription
// the wallet actor sets up via MapSpendEvent.
type BoardingSweepSpendNotification struct {
	actor.BaseMessage

	// Outpoint is the boarding UTXO that was spent.
	Outpoint wire.OutPoint

	// SpendingTxid is the transaction that confirmed the spend.
	SpendingTxid chainhash.Hash

	// SpendingHeight is the block height of the spending transaction.
	SpendingHeight int32
}

// MessageType returns the message type identifier.
func (m BoardingSweepSpendNotification) MessageType() string {
	return "BoardingSweepSpendNotification"
}

func (m BoardingSweepSpendNotification) walletMsgSealed() {}

// BoardingSweepTxNotification is a Tell carrying a txconfirm terminal
// notification (confirmation or failure) for a tracked sweep tx,
// re-wrapped from txconfirm.TxConfirmed / txconfirm.TxFailed via
// txconfirm.MapNotification.
type BoardingSweepTxNotification struct {
	actor.BaseMessage

	// Confirmed is true when the underlying txconfirm.TxConfirmed event
	// fired; false when it was txconfirm.TxFailed.
	Confirmed bool

	// Txid identifies the tracked sweep transaction.
	Txid chainhash.Hash

	// BlockHeight is the height at which the sweep confirmed when
	// Confirmed=true; zero otherwise.
	BlockHeight int32

	// NumConfs is the confirmation count when Confirmed=true; zero
	// otherwise.
	NumConfs uint32

	// Reason is the human-readable failure reason when Confirmed=false.
	Reason string
}

// MessageType returns the message type identifier.
func (m BoardingSweepTxNotification) MessageType() string {
	return "BoardingSweepTxNotification"
}

func (m BoardingSweepTxNotification) walletMsgSealed() {}

// BoardingSweepNotificationAck is the empty reply to spend / tx
// notifications. Notifications are Tell semantically; the wallet's
// generic Receive shape requires a typed response so we return this
// no-op ack.
type BoardingSweepNotificationAck struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier.
func (m *BoardingSweepNotificationAck) MessageType() string {
	return "BoardingSweepNotificationAck"
}

func (m *BoardingSweepNotificationAck) walletRespSealed() {}

// pendingSweepState tracks one in-flight aggregate sweep. The wallet actor
// keeps these in memory so it can correlate spend / txconfirm
// notifications to the sweep txid without a DB round-trip.
type pendingSweepState struct {
	// txid is the aggregate sweep tx id.
	txid chainhash.Hash

	// inputs maps each tracked input outpoint to its caller-id used for
	// chainsource spend-watch deregistration.
	inputs map[wire.OutPoint]string

	// totalAmount is the gross input value of the sweep.
	totalAmount btcutil.Amount

	// fee is the absolute miner fee paid by the sweep tx.
	fee btcutil.Amount

	// destWalletDerived is true when the destination pkScript was
	// allocated from the local wallet (NewWalletPkScript), false when
	// the caller passed an explicit external SweepAddress. Used by PR
	// B's ledger emission path to decide whether to emit a
	// UTXOCreatedMsg for the sweep destination.
	destWalletDerived bool

	// submitted is true once the sweep tx has been successfully handed
	// off to the txconfirm broadcaster. The resume retry path uses this
	// to distinguish "fully recovered, skip" from "partial recovery,
	// re-attempt the txconfirm submit and any missing input watches"
	// when the same sweep is observed across a block-epoch retry.
	submitted bool
}

// boardingSweepCallerID returns the deterministic chainsource caller-id
// used to register or cancel a per-outpoint spend watch.
func boardingSweepCallerID(op wire.OutPoint) string {
	return fmt.Sprintf("%s:%s:%d", boardingSweepCallerIDPrefix, op.Hash,
		op.Index)
}

// boardingSweepEnabled reports whether all boarding-sweep dependencies were
// supplied at NewArk time. When false, sweep messages return a clear error
// rather than silently no-oping. The shared txconfirm broadcaster is
// resolved lazily via the receptionist (see txconfirm.LookupRef) so this
// gate only checks for the locally injected dependencies; the broadcaster
// resolution itself is gated by actor-system availability inside
// submitSweepConfirmer, which is only reached on the broadcast path.
func (a *Ark) boardingSweepEnabled() bool {
	return a.sweepStore != nil && a.sweepSigner != nil
}

// askBestHeight asks the chainsource actor for the current best block
// height.
func (a *Ark) askBestHeight(ctx context.Context) (int32, error) {
	future := a.chainSource.Ask(ctx, &chainsource.BestHeightRequest{})
	result := future.Await(ctx)
	if result.IsErr() {
		return 0, result.Err()
	}

	resp, ok := result.UnwrapOr(nil).(*chainsource.BestHeightResponse)
	if !ok || resp == nil {
		return 0, fmt.Errorf("unexpected best-height response")
	}

	return resp.Height, nil
}

// askFeeEstimate asks the chainsource actor for a fee estimate.
func (a *Ark) askFeeEstimate(ctx context.Context, target uint32) (
	btcutil.Amount, error) {

	future := a.chainSource.Ask(ctx, &chainsource.FeeEstimateRequest{
		TargetConf: target,
	})
	result := future.Await(ctx)
	if result.IsErr() {
		return 0, result.Err()
	}
	resp, ok := result.UnwrapOr(nil).(*chainsource.FeeEstimateResponse)
	if !ok || resp == nil {
		return 0, fmt.Errorf("unexpected fee-estimate response")
	}

	return resp.SatPerVByte, nil
}

// resolveSweepFeeRate is the actor-bound counterpart of BoardingSweepFeeRate
// that uses chainsource Asks instead of a direct interface call.
func (a *Ark) resolveSweepFeeRate(ctx context.Context, feeRateSatPerVByte int64,
	confTarget uint32) (int64, uint32, error) {

	if feeRateSatPerVByte > 0 {
		return feeRateSatPerVByte, confTarget, nil
	}
	if confTarget == 0 {
		confTarget = defaultBoardingSweepConfTarget
	}

	feeRate, err := a.askFeeEstimate(ctx, confTarget)
	if err != nil {
		return defaultBoardingSweepFallbackFeeRateSatPerVByte,
			confTarget, err
	}
	if int64(feeRate) <= 0 {
		return defaultBoardingSweepFallbackFeeRateSatPerVByte,
			confTarget, nil
	}

	return int64(feeRate), confTarget, nil
}

// loadSweepCandidates loads boarding intents that are candidates for an
// aggregate sweep. When outpoints is empty, every sweepable intent is
// returned; otherwise only the requested outpoints are loaded.
func (a *Ark) loadSweepCandidates(ctx context.Context,
	outpoints []wire.OutPoint) ([]BoardingIntent, error) {

	if len(outpoints) == 0 {
		return a.sweepStore.FetchBoardingIntentsBySweepableStatuses(
			ctx, defaultBoardingSweepStatuses(),
		)
	}

	intents := make([]BoardingIntent, 0, len(outpoints))
	seen := make(map[wire.OutPoint]struct{}, len(outpoints))
	for _, op := range outpoints {
		if _, ok := seen[op]; ok {
			continue
		}
		seen[op] = struct{}{}

		intent, err := a.sweepStore.GetIntent(ctx, op)
		if err != nil {
			return nil, fmt.Errorf("load boarding intent %s: %w",
				op, err)
		}
		if intent == nil {
			return nil, fmt.Errorf("boarding intent %s not found",
				op)
		}
		if !boardingIntentSweepable(intent.Status) {
			continue
		}
		intents = append(intents, *intent)
	}

	return intents, nil
}

// defaultBoardingSweepStatuses are the boarding-intent statuses considered
// candidates for an aggregate timeout sweep when no outpoint set is
// supplied.
func defaultBoardingSweepStatuses() []BoardingStatus {
	return []BoardingStatus{
		BoardingStatusConfirmed,
		BoardingStatusFailed,
		BoardingStatusExpired,
	}
}

// boardingIntentSweepable reports whether an intent in the given status is
// eligible for inclusion in a new aggregate sweep.
func boardingIntentSweepable(status BoardingStatus) bool {
	switch status {
	case BoardingStatusConfirmed, BoardingStatusFailed,
		BoardingStatusExpired:
		return true

	case BoardingStatusAdopted, BoardingStatusSwept,
		BoardingStatusSweepPending:
		// Already accounted for elsewhere — adopted intents are
		// committed to a round, swept intents have already been
		// recovered, and sweep_pending intents are part of an
		// in-flight aggregate sweep.
		return false
	}

	return false
}

// failedSweepResponse builds a "failed" SweepBoardingUTXOsResponse carrying
// the human-readable failure reason. The response is returned with
// Status="failed" and FailureReason set; the RPC transport remains
// successful so clients see the failure as application-level.
func failedSweepResponse(err error, feeRate int64,
	confTarget uint32) *SweepBoardingUTXOsResponse {

	reason := ""
	if err != nil {
		reason = err.Error()
	}

	return &SweepBoardingUTXOsResponse{
		Status:             "failed",
		FailureReason:      reason,
		FeeRateSatPerVByte: feeRate,
		ConfTarget:         confTarget,
	}
}

// handleSweepBoardingUTXOs is the wallet actor's primary boarding-sweep
// entry point.
func (a *Ark) handleSweepBoardingUTXOs(ctx context.Context,
	req *SweepBoardingUTXOsRequest) fn.Result[WalletResp] {

	if !a.boardingSweepEnabled() {
		return fn.Err[WalletResp](
			errors.New("boarding sweep subsystem not initialised"),
		)
	}
	if req.FeeRateSatPerVByte < 0 {
		return fn.Err[WalletResp](
			errors.New("fee_rate_sat_per_vbyte must be " +
				"non-negative"),
		)
	}

	log := a.logger(ctx)

	candidates, err := a.loadSweepCandidates(ctx, req.Outpoints)
	if err != nil {
		return fn.Ok[WalletResp](failedSweepResponse(err, 0, 0))
	}

	feeRate, confTarget, feeErr := a.resolveSweepFeeRate(
		ctx, req.FeeRateSatPerVByte, req.ConfTarget,
	)
	if feeErr != nil && req.FeeRateSatPerVByte <= 0 {
		log.DebugS(ctx, "Falling back to default boarding sweep fee "+
			"rate", feeErr,
			slog.Int64("fee_rate_sat_per_vbyte", feeRate),
			slog.Uint64("conf_target", uint64(confTarget)))
	}
	if feeRate >= boardingSweepHighFeeRateWarningSatPerVByte {
		log.WarnS(ctx, "Boarding sweep fee rate is unusually high",
			nil,
			slog.Int64("fee_rate_sat_per_vbyte", feeRate),
		)
	}

	bestHeight, err := a.askBestHeight(ctx)
	if err != nil {
		return fn.Ok[WalletResp](
			failedSweepResponse(
				fmt.Errorf("resolve best height: %w", err),
				feeRate, confTarget,
			),
		)
	}

	mature := candidates[:0]
	resp := &SweepBoardingUTXOsResponse{
		Status:             "preview",
		CurrentHeight:      bestHeight,
		FeeRateSatPerVByte: feeRate,
		ConfTarget:         confTarget,
	}
	for _, intent := range candidates {
		maturity := boardingSweepMaturityHeight(intent)
		if bestHeight < maturity {
			continue
		}
		mature = append(mature, intent)

		resp.SweepableOutputs = append(
			resp.SweepableOutputs, BoardingSweepOutput{
				Outpoint:       intent.Outpoint,
				AmountSat:      int64(intent.ChainInfo.Amount),
				MaturityHeight: maturity,
			},
		)
		resp.TotalAmountSat += int64(intent.ChainInfo.Amount)
	}
	if len(mature) == 0 {

		// Empty preview; nothing to sign.
		return fn.Ok[WalletResp](resp)
	}

	pkScript, scriptErr := boardingSweepPkScript(
		ctx, a.sweepSigner, a.sweepChainParams, req.SweepAddress,
		req.Broadcast,
	)
	if scriptErr != nil {
		return fn.Ok[WalletResp](
			failedSweepResponse(
				scriptErr, feeRate, confTarget,
			),
		)
	}
	destWalletDerived := req.SweepAddress == "" && req.Broadcast

	signed, err := buildBoardingSweepTx(
		a.sweepSigner, mature, pkScript, feeRate,
	)
	if err != nil {
		return fn.Ok[WalletResp](
			failedSweepResponse(
				err, feeRate, confTarget,
			),
		)
	}

	resp.HasTxid = true
	resp.Txid = signed.Tx.TxHash()
	resp.EstimatedFeeSat = int64(signed.Fee)
	resp.NetAmountSat = resp.TotalAmountSat - int64(signed.Fee)
	resp.TxVBytes = signed.VBytes

	if !req.Broadcast {

		// Preview only; nothing persisted, nothing broadcast.
		return fn.Ok[WalletResp](resp)
	}

	return a.publishBoardingSweep(ctx, publishBoardingSweepArgs{
		mature:            mature,
		signed:            signed,
		pkScript:          pkScript,
		feeRate:           feeRate,
		confTarget:        confTarget,
		bestHeight:        bestHeight,
		destWalletDerived: destWalletDerived,
		persistAddress:    req.SweepAddress,
		resp:              resp,
	})
}

// publishBoardingSweepArgs bundles the parameters threaded through the
// boarding-sweep persistence + broadcast path. Keeping them in one struct
// avoids re-plumbing the same eight values through every helper return.
type publishBoardingSweepArgs struct {
	mature            []BoardingIntent
	signed            *boardingSweepTx
	pkScript          []byte
	feeRate           int64
	confTarget        uint32
	bestHeight        int32
	destWalletDerived bool
	persistAddress    string
	resp              *SweepBoardingUTXOsResponse
}

// publishBoardingSweep persists the signed sweep, sets up spend-watch
// tracking, submits the parent to the txconfirm broadcaster, and marks the
// sweep as published. Any error along the way rolls back the in-memory and
// on-disk state and returns a failed-sweep response to the caller.
func (a *Ark) publishBoardingSweep(ctx context.Context,
	args publishBoardingSweepArgs) fn.Result[WalletResp] {

	log := a.logger(ctx)

	inputs := make([]NewBoardingSweepInput, 0, len(args.mature))
	for _, intent := range args.mature {
		inputs = append(inputs, NewBoardingSweepInput{
			Outpoint:       intent.Outpoint,
			Amount:         intent.ChainInfo.Amount,
			PreviousStatus: intent.Status,
		})
	}

	newSweep := NewBoardingSweep{
		Tx:                 args.signed.Tx,
		DestinationAddress: args.persistAddress,
		TotalAmount:        btcutil.Amount(args.resp.TotalAmountSat),
		FeeAmount:          args.signed.Fee,
		FeeRateSatPerVByte: args.feeRate,
		VBytes:             args.signed.VBytes,
		CreatedHeight:      args.bestHeight,
		Inputs:             inputs,
	}

	if err := a.sweepStore.CreatePendingBoardingSweep(
		ctx, newSweep,
	); err != nil {
		return fn.Ok[WalletResp](
			failedSweepResponse(
				fmt.Errorf("persist sweep: %w", err),
				args.feeRate, args.confTarget,
			),
		)
	}

	matureN := len(args.mature)
	pending := &pendingSweepState{
		txid:              args.signed.Tx.TxHash(),
		inputs:            make(map[wire.OutPoint]string, matureN),
		totalAmount:       btcutil.Amount(args.resp.TotalAmountSat),
		fee:               args.signed.Fee,
		destWalletDerived: args.destWalletDerived,
	}

	// Defend against silently overwriting an existing tracking entry
	// (e.g. a duplicate publish call on identical inputs that produces
	// the same deterministic txid). The previous entry's spend watches
	// are still live and routing to selfRef, so reuse the existing
	// pending state and skip re-registration.
	if existing, ok := a.pendingSweeps[pending.txid]; ok {
		log.InfoS(ctx, "Boarding sweep already in flight; "+
			"reusing existing tracking entry",
			slog.String("txid", pending.txid.String()))

		pending = existing
	} else {
		a.pendingSweeps[pending.txid] = pending
	}

	for _, intent := range args.mature {
		err := a.registerSweepSpendWatch(
			ctx, intent, uint32(args.bestHeight), pending,
		)
		if err != nil {
			log.WarnS(ctx, "Failed to register sweep spend watch",
				err, slog.String(
					"outpoint", intent.Outpoint.String(),
				))
		}
	}

	if err := a.submitSweepConfirmer(
		ctx, args.signed.Tx, args.pkScript, uint32(args.bestHeight),
	); err != nil {

		failErr := a.sweepStore.MarkBoardingSweepFailed(
			ctx, pending.txid, err,
		)
		if failErr != nil {
			log.WarnS(ctx, "Failed to roll back failed sweep",
				failErr, slog.String(
					"txid", pending.txid.String(),
				))
		}

		delete(a.pendingSweeps, pending.txid)
		a.cancelSweepSpendWatches(ctx, pending)

		return fn.Ok[WalletResp](
			failedSweepResponse(
				fmt.Errorf("submit sweep to broadcaster: %w",
					err),
				args.feeRate,
				args.confTarget,
			),
		)
	}
	pending.submitted = true

	// The tx is already in the broadcaster's hands at this point, so a
	// MarkBoardingSweepPublished failure cannot be rolled back. Surface
	// the inconsistency to the caller: the published status is correct
	// (txconfirm accepted the parent) but the persisted lifecycle row
	// is still at "pending" until the next resume tick re-runs the
	// post-broadcast bookkeeping. The block-epoch resume retry will
	// re-issue MarkBoardingSweepPublished on the next tick because the
	// sweep is still in pending status on disk.
	if err := a.sweepStore.MarkBoardingSweepPublished(
		ctx, pending.txid,
	); err != nil {

		log.WarnS(ctx, "Failed to mark boarding sweep published",
			err,
			slog.String("txid", pending.txid.String()),
		)
		args.resp.FailureReason = fmt.Sprintf("published but "+
			"persistence write failed: %s; store will reconcile "+
			"on next resume tick", err)
	}

	args.resp.Status = "published"
	args.resp.FeePaidSat = int64(args.signed.Fee)

	return fn.Ok[WalletResp](args.resp)
}

// registerSweepSpendWatch registers a chainsource spend watch for one
// boarding-sweep input. The notify-target translates each chainsource
// SpendEvent into a BoardingSweepSpendNotification routed to the wallet
// actor's own ref.
//
// Registration is deduped against pendingSweepInputs so a duplicate call for
// an outpoint already being watched (e.g. an idempotent resume, or two
// publish paths racing on overlapping inputs) does not Spawn a second
// chainsource SpendActor. The pre-existing watch already routes spend
// events to the wallet's selfRef, so the second caller observes the same
// notifications without leaking a sub-actor.
func (a *Ark) registerSweepSpendWatch(ctx context.Context,
	intent BoardingIntent, heightHint uint32,
	pending *pendingSweepState) error {

	op := intent.Outpoint
	if existing, ok := a.pendingSweepInputs[op]; ok {
		a.logger(ctx).DebugS(ctx,
			"Boarding-sweep spend watch already registered; "+
				"skipping duplicate",
			slog.String("outpoint", op.String()),
			slog.String("owning_sweep", existing.String()))

		return nil
	}

	out, err := boardingSweepTargetOutput(intent)
	if err != nil {
		return fmt.Errorf("derive target output: %w", err)
	}

	notify := chainsource.MapSpendEvent(a.selfRef,
		func(ev chainsource.SpendEvent) WalletMsg {
			return BoardingSweepSpendNotification{
				Outpoint:       ev.Outpoint,
				SpendingTxid:   ev.SpendingTxid,
				SpendingHeight: ev.SpendingHeight,
			}
		},
	)

	callerID := boardingSweepCallerID(op)
	notifyOpt := fn.Some[actor.TellOnlyRef[chainsource.SpendEvent]](
		notify,
	)
	req := &chainsource.RegisterSpendRequest{
		CallerID:    callerID,
		Outpoint:    &op,
		PkScript:    out.PkScript,
		HeightHint:  heightHint,
		NotifyActor: notifyOpt,
	}

	future := a.chainSource.Ask(ctx, req)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("register spend watch: %w", result.Err())
	}

	pending.inputs[op] = callerID
	a.pendingSweepInputs[op] = pending.txid

	return nil
}

// cancelSweepSpendWatches unregisters every spend watch tracked by the
// given pending sweep state. Called on terminal sweep transitions to
// prevent dangling chainsource sub-actors.
func (a *Ark) cancelSweepSpendWatches(ctx context.Context,
	pending *pendingSweepState) {

	cancellations := make(map[wire.OutPoint]string, len(pending.inputs))
	for op, id := range pending.inputs {
		cancellations[op] = id
	}
	pending.inputs = make(map[wire.OutPoint]string)

	for op, callerID := range cancellations {
		op := op
		delete(a.pendingSweepInputs, op)
		err := a.chainSource.Tell(ctx,
			&chainsource.UnregisterSpendRequest{
				CallerID: callerID,
				Outpoint: &op,
			})
		if err != nil {
			a.logger(ctx).WarnS(
				ctx,
				"Failed to unregister sweep spend watch",
				err,
				slog.String("outpoint", op.String()),
			)
		}
	}
}

// submitSweepConfirmer hands the signed sweep transaction off to the
// shared txconfirm broadcaster. Confirmation tracking arrives
// asynchronously on the wallet actor's selfRef as
// BoardingSweepTxNotification messages.
//
// The txconfirm subscriber type is re-wrapped via txconfirm.MapNotification
// so the wallet actor never has to receive a txconfirm.Notification
// directly; only the wallet-domain BoardingSweepTxNotification flows on
// the actor's mailbox.
//
// The broadcaster is resolved lazily through the receptionist
// (txconfirm.LookupRef) so the wallet actor can be constructed before the
// txconfirm actor has been registered, mirroring how round / vtxo / oor
// wire their cross-actor refs.
func (a *Ark) submitSweepConfirmer(ctx context.Context, tx *wire.MsgTx,
	pkScript []byte, heightHint uint32) error {

	walletNotif := actor.NewMapInputRef[
		BoardingSweepTxNotification, WalletMsg,
	](
		a.selfRef,
		func(n BoardingSweepTxNotification) WalletMsg {
			return n
		},
	)

	subscriber := txconfirm.MapNotification(walletNotif,
		func(n txconfirm.Notification) BoardingSweepTxNotification {
			switch ev := n.(type) {
			case *txconfirm.TxConfirmed:
				return BoardingSweepTxNotification{
					Confirmed:   true,
					Txid:        ev.Txid,
					BlockHeight: ev.BlockHeight,
					NumConfs:    ev.NumConfs,
				}

			case *txconfirm.TxFailed:
				return BoardingSweepTxNotification{
					Confirmed: false,
					Txid:      ev.Txid,
					Reason:    ev.Reason,
				}
			}

			return BoardingSweepTxNotification{}
		},
	)

	return a.submitSweepToConfirm(
		ctx, tx, pkScript, heightHint, boardingSweepBroadcastLabel,
		subscriber,
	)
}

// submitSweepToConfirm registers a signed sweep transaction with the shared
// txconfirm broadcaster and surfaces a synchronous registration failure as an
// error. The subscriber is built per caller (boarding vs general wallet
// sweep) so each receives its own wallet-domain notification type; everything
// downstream of that — the broadcaster lookup, the Ask, and the
// TxStateFailed guard — is identical and lives here.
//
// The broadcaster is resolved lazily through the receptionist
// (txconfirm.LookupRef) so the wallet actor can be constructed before the
// txconfirm actor has been registered, mirroring how round / vtxo / oor wire
// their cross-actor refs.
func (a *Ark) submitSweepToConfirm(ctx context.Context, tx *wire.MsgTx,
	pkScript []byte, heightHint uint32, label string,
	subscriber actor.TellOnlyRef[txconfirm.Notification]) error {

	if a.actorSystem == nil {
		return fmt.Errorf("actor system unavailable")
	}

	req := &txconfirm.EnsureConfirmedReq{
		Tx:                   tx,
		ConfirmationPkScript: pkScript,
		Label:                label,
		HeightHint:           heightHint,
		TargetConfs:          1,
		Subscriber:           subscriber,
	}

	ref := txconfirm.LookupRef(a.actorSystem)
	future := ref.Ask(ctx, req)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("ensure confirmed: %w", result.Err())
	}

	// txconfirm.handleEnsure returns an OK actor response even when the
	// tracked tx has been moved into TxStateFailed (e.g. broadcast or
	// confirmation-watch setup failed synchronously). Surface that as an
	// error so the caller rolls the sweep back to failed instead of
	// reporting status=published while the broadcaster's async failure
	// notification is still in flight.
	raw := result.UnwrapOr(nil)
	resp, ok := raw.(*txconfirm.EnsureConfirmedResp)
	if !ok || resp == nil {
		return fmt.Errorf("ensure confirmed: unexpected response %T",
			raw)
	}
	if resp.State == txconfirm.TxStateFailed {
		return fmt.Errorf("ensure confirmed: txconfirm entered " +
			"failed state during registration")
	}

	return nil
}

// handleSweepSpendNotification updates persistent state when an input of
// an in-flight aggregate sweep is observed spent on chain.
func (a *Ark) handleSweepSpendNotification(ctx context.Context,
	notif BoardingSweepSpendNotification) fn.Result[WalletResp] {

	if a.sweepStore == nil {
		return fn.Ok[WalletResp](&BoardingSweepNotificationAck{})
	}

	resolved, err := a.sweepStore.MarkBoardingSweepInputSpent(
		ctx, notif.Outpoint, notif.SpendingTxid, notif.SpendingHeight,
	)
	switch {
	// A duplicate spend event for an input row that has already been
	// resolved (e.g. a re-org-then-respend, or a stale buffered event
	// arriving after the sweep moved to a terminal state) returns
	// ErrNoRows from the store. This is benign — the row is already in
	// its target state — so suppress it to Debug rather than alerting on
	// every duplicate.
	case errors.Is(err, sql.ErrNoRows):
		a.logger(ctx).DebugS(ctx,
			"Boarding sweep input already resolved; "+
				"ignoring duplicate spend",
			slog.String("outpoint", notif.Outpoint.String()),
			slog.String(
				"spending_txid", notif.SpendingTxid.String(),
			))

		return fn.Ok[WalletResp](&BoardingSweepNotificationAck{})

	case err != nil:
		a.logger(ctx).WarnS(ctx,
			"Failed to mark boarding sweep input spent", err,
			slog.String("outpoint", notif.Outpoint.String()),
			slog.String(
				"spending_txid", notif.SpendingTxid.String(),
			))

		return fn.Ok[WalletResp](&BoardingSweepNotificationAck{})
	}

	var callerID string
	var pending *pendingSweepState
	for txid, p := range a.pendingSweeps {
		if id, ok := p.inputs[notif.Outpoint]; ok {
			callerID = id
			pending = p
			delete(p.inputs, notif.Outpoint)
			delete(a.pendingSweepInputs, notif.Outpoint)
			if resolved {
				delete(a.pendingSweeps, txid)
			}

			break
		}
	}

	if callerID != "" {
		op := notif.Outpoint
		if err := a.chainSource.Tell(ctx,
			&chainsource.UnregisterSpendRequest{
				CallerID: callerID,
				Outpoint: &op,
			}); err != nil {

			a.logger(ctx).DebugS(
				ctx,
				"Best-effort unregister spend failed",
				err,
				slog.String("outpoint", op.String()),
			)
		}
	}

	if resolved && pending != nil {
		// Cancel any straggler spend watches for the same sweep so
		// chainsource sub-actors do not leak.
		a.cancelSweepSpendWatches(ctx, pending)
	}

	return fn.Ok[WalletResp](&BoardingSweepNotificationAck{})
}

// handleSweepTxNotification processes a SweepConfirmer terminal
// notification for a tracked aggregate sweep. On confirmation the handler
// also reconciles the store by marking every still-pending input of the
// sweep as spent: chainsource spend events drive the per-input store
// transitions in the happy path, but a missed spend notification (e.g.
// registration failure, restart-time gap before resume re-arms a watch)
// would otherwise leave the sweep stuck at "published" indefinitely.
// MarkBoardingSweepInputSpent is idempotent, so re-marking inputs that
// already transitioned via the chainsource fast path is a no-op. Failure
// is mirrored into the store as a terminal-failed sweep.
func (a *Ark) handleSweepTxNotification(ctx context.Context,
	notif BoardingSweepTxNotification) fn.Result[WalletResp] {

	if a.sweepStore == nil {
		return fn.Ok[WalletResp](&BoardingSweepNotificationAck{})
	}

	switch {
	case notif.Confirmed:
		a.logger(ctx).DebugS(
			ctx,
			"Boarding sweep confirmation observed by broadcaster",
			nil,
			slog.String("txid", notif.Txid.String()),
			slog.Int("block_height", int(notif.BlockHeight)),
		)
		a.reconcileSweepInputsOnConfirm(ctx, notif)

		a.emitSweepConfirmedLedger(ctx, notif)

	default:
		a.logger(ctx).WarnS(
			ctx,
			"Boarding sweep broadcaster reported failure",
			errors.New(notif.Reason),
			slog.String("txid", notif.Txid.String()),
		)

		// Count the terminal failure of this daemon-owned sweep so
		// operators can alert on a stuck boarding-sweep watcher.
		a.emitBackgroundTaskError(ctx, "boarding_sweep_watcher")

		err := a.sweepStore.MarkBoardingSweepFailed(
			ctx, notif.Txid, errors.New(notif.Reason),
		)
		if err != nil {
			a.logger(ctx).WarnS(
				ctx,
				"Failed to mark boarding sweep failed",
				err,
				slog.String("txid", notif.Txid.String()),
			)
		}

		pending := a.pendingSweeps[notif.Txid]
		delete(a.pendingSweeps, notif.Txid)

		if pending != nil {
			a.cancelSweepSpendWatches(ctx, pending)
		}
	}

	return fn.Ok[WalletResp](&BoardingSweepNotificationAck{})
}

// reconcileSweepInputsOnConfirm marks every still-pending input of the
// confirmed sweep as spent in the store. It acts as a fallback for
// chainsource spend notifications that may have been missed (registration
// errors, gaps at startup). Because the sweep tx is what spent each input,
// the spending txid is the sweep's own txid. MarkBoardingSweepInputSpent
// is idempotent, so inputs already resolved via the spend-notification path
// are left untouched.
func (a *Ark) reconcileSweepInputsOnConfirm(ctx context.Context,
	notif BoardingSweepTxNotification) {

	pending, ok := a.pendingSweeps[notif.Txid]
	if !ok || pending == nil {
		return
	}

	for op := range pending.inputs {
		_, err := a.sweepStore.MarkBoardingSweepInputSpent(
			ctx, op, notif.Txid, notif.BlockHeight,
		)
		if err != nil {
			a.logger(ctx).WarnS(
				ctx,
				"Failed to mark sweep input spent on confirm",
				err,
				slog.String("outpoint", op.String()),
				slog.String("txid", notif.Txid.String()),
			)
		}
	}
}

// emitSweepConfirmedLedger emits the double-entry ledger and UTXO audit
// events corresponding to a boarding-sweep confirmation as a single
// BoardingSweepConfirmedMsg. The ledger actor expands that one message into
// every clearing leg inside one Commit:
//
//   - the L1 chain cost (debit onchain_fees, credit wallet_clearing);
//   - one audit row + wallet_clearing debit per swept boarding outpoint;
//   - the destination leg — a wallet-return deposit or, for a
//     caller-supplied external address, a transfers_out settlement.
//
// Folding the legs into one message makes them atomic on the ledger side, so
// a partial failure can never strand value in wallet_clearing. Emission is
// still best-effort: a Tell error is logged but does not fail the
// confirmation path, and a redelivery is idempotent via the per-leg keys the
// handler derives.
func (a *Ark) emitSweepConfirmedLedger(ctx context.Context,
	notif BoardingSweepTxNotification) {

	if a.ledgerSink.IsNone() || a.sweepStore == nil {
		return
	}

	// The persisted sweep record is the sole source of truth for
	// inputs / destination / amounts at confirmation time.
	// pendingSweeps[txid] is routinely cleared by
	// handleSweepSpendNotification before / during confirmation as
	// inputs resolve, and is also absent across restarts. Gating the
	// audit and balance legs on in-memory state would silently drop
	// them in the common case.
	record, ok := a.lookupSweepRecord(ctx, notif.Txid)
	if !ok {
		a.logger(ctx).WarnS(ctx, "emit ledger: sweep record not found",
			nil,
			slog.String("txid", notif.Txid.String()),
		)

		return
	}

	a.ledgerSink.WhenSome(func(sink ledger.Sink) {
		// The clearing-account legs only net to zero when the sweep's
		// destination output is readable from the persisted tx: chain
		// cost is (total - destination) = miner fee + P2A anchor, and
		// the destination leg credits wallet_clearing by that same
		// destination value. A record missing its tx (corruption, or a
		// legacy row predating tx persistence) cannot produce a
		// balanced set, so skip emission rather than drift the clearing
		// account. The store requires the tx at write time, so this
		// only ever fires on a genuinely inconsistent record.
		if record.Tx == nil || len(record.Tx.TxOut) == 0 {
			a.logger(ctx).WarnS(ctx,
				"emit ledger: sweep record missing tx, "+
					"skipping clearing legs",
				nil, slog.String("txid", notif.Txid.String()))

			return
		}

		destSat := boardingSweepDestinationAmount(record)
		if destSat <= 0 {
			a.logger(ctx).WarnS(ctx,
				"emit ledger: sweep destination non-positive, "+
					"skipping clearing legs",
				nil, slog.String("txid", notif.Txid.String()))

			return
		}

		// Every clearing leg ships in a single
		// BoardingSweepConfirmedMsg so the ledger actor books the fee,
		// per-input, and destination legs atomically inside one Commit.
		// Splitting them into independent Tells previously risked a
		// partial failure that stranded value in wallet_clearing; one
		// message either lands in full or not at all.
		// DestinationAddress is empty when the daemon allocated a fresh
		// wallet output (a wallet-derived return) and non-empty when
		// the caller supplied an external address (the persisted
		// equivalent of destWalletDerived).
		inputs := make([]ledger.SweepInput, 0, len(record.Inputs))
		for _, in := range record.Inputs {
			inputs = append(inputs, ledger.SweepInput{
				Outpoint:  in.Outpoint,
				AmountSat: int64(in.Amount),
			})
		}

		msg := &ledger.BoardingSweepConfirmedMsg{
			Txid:        notif.Txid,
			BlockHeight: uint32(notif.BlockHeight),
			ChainCostSat: boardingSweepLedgerChainCost(
				record,
			),
			Inputs:              inputs,
			DestinationSat:      destSat,
			DestinationExternal: record.DestinationAddress != "",
		}
		if err := sink.Tell(ctx, msg); err != nil {
			a.logger(ctx).WarnS(
				ctx,
				"emit ledger: BoardingSweepConfirmedMsg failed",
				err,
				slog.String("txid", notif.Txid.String()),
			)
		}
	})
}

// boardingSweepDestinationAmount returns the value paid to the sweep
// destination. buildBoardingSweepTx always places the destination output at
// vout 0 and the P2A anchor after it.
func boardingSweepDestinationAmount(record *BoardingSweepRecord) int64 {
	if record == nil || record.Tx == nil || len(record.Tx.TxOut) == 0 {
		return 0
	}

	return record.Tx.TxOut[0].Value
}

// boardingSweepLedgerChainCost returns the value that left the wallet as
// sweep chain cost: the miner fee plus the P2A anchor output, derived as
// (total input value - destination output value). emitSweepConfirmedLedger
// only calls this once it has verified record.Tx is present, so the
// destination output is readable and the result captures both the miner fee
// and the anchor. record.FeeAmount is deliberately NOT used as a fallback: it
// is the miner fee alone and omitting the anchor would leave wallet_clearing
// non-zero. A degenerate record where the total does not exceed the
// destination yields 0, which the fee handler rejects as non-positive rather
// than booking a silently wrong cost.
func boardingSweepLedgerChainCost(record *BoardingSweepRecord) int64 {
	if record == nil {
		return 0
	}

	destSat := boardingSweepDestinationAmount(record)
	totalSat := int64(record.TotalAmount)
	if destSat > 0 && totalSat > destSat {
		return totalSat - destSat
	}

	return 0
}

// lookupSweepRecord returns the persisted sweep record for the given txid.
// Used by the ledger-emission path at confirmation time, where in-memory
// pendingSweeps state has already been cleared (or was never present after
// restart) and the persisted record is the only complete source of truth
// for inputs, destination, and amounts. Returns (nil, false) on error or
// when no matching record is found.
func (a *Ark) lookupSweepRecord(ctx context.Context, txid chainhash.Hash) (
	*BoardingSweepRecord, bool) {

	record, err := a.sweepStore.GetBoardingSweep(ctx, txid)
	if err != nil {
		a.logger(ctx).WarnS(ctx, "lookupSweepRecord: get failed",
			err,
			slog.String("txid", txid.String()),
		)

		return nil, false
	}
	if record == nil {
		return nil, false
	}

	return record, true
}

// handleResumeBoardingSweeps reloads non-terminal sweeps from the
// persistent store at startup and re-registers chainsource spend watches
// plus re-submits the persisted transaction to the SweepConfirmer.
// SweepConfirmer impls are expected to dedup by txid so a duplicate submit
// is a no-op when the previous run still has the tracking entry; on a
// fresh process it transparently rebroadcasts.
func (a *Ark) handleResumeBoardingSweeps(ctx context.Context,
	_ *ResumeBoardingSweepsRequest) fn.Result[WalletResp] {

	if !a.boardingSweepEnabled() {
		return fn.Ok[WalletResp](&ResumeBoardingSweepsResponse{})
	}

	sweeps, err := a.sweepStore.ListPendingBoardingSweeps(ctx)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("list pending boarding sweeps: %w", err),
		)
	}

	log := a.logger(ctx)
	log.InfoS(ctx, "Resuming pending boarding sweeps",
		slog.Int("count", len(sweeps)),
	)

	var (
		resumed int32
		failed  int32
	)
	for _, sweep := range sweeps {
		if a.resumeOneBoardingSweep(ctx, sweep) {
			resumed++
		} else {
			failed++
		}
	}

	if failed > 0 {
		log.WarnS(ctx, "Boarding sweep resume completed with failures",
			nil,
			slog.Int("resumed", int(resumed)),
			slog.Int("failed", int(failed)),
		)
	}

	return fn.Ok[WalletResp](&ResumeBoardingSweepsResponse{
		Resumed: resumed,
		Failed:  failed,
	})
}

// resumeOneBoardingSweep recovers a single persisted sweep, taking care to
// converge from any partial state left by a previous resume attempt. The
// boolean return signals "fully recovered" — false means the caller should
// surface the failure (and a future block-epoch retry will try again).
//
// A sweep is fully recovered when (a) every input row in pending/published
// status has an active chainsource spend watch and (b) the sweep tx has
// been handed off to the txconfirm broadcaster.
func (a *Ark) resumeOneBoardingSweep(ctx context.Context,
	sweep BoardingSweepRecord) bool {

	log := a.logger(ctx)

	pending := a.pendingSweeps[sweep.Txid]
	if pending == nil {
		pending = &pendingSweepState{
			txid:        sweep.Txid,
			inputs:      make(map[wire.OutPoint]string),
			totalAmount: sweep.TotalAmount,
			fee:         sweep.FeeAmount,
			// destWalletDerived is unknown after restart. The
			// ledger emission path treats it conservatively
			// (skips destination-leg emission) when not known to
			// be wallet-derived.
			destWalletDerived: false,
		}
		a.pendingSweeps[sweep.Txid] = pending
	}

	allInputsArmed := true
	for _, in := range sweep.Inputs {
		switch in.Status {
		case BoardingSweepInputStatusPending,
			BoardingSweepInputStatusPublished:
		default:
			continue
		}

		// Skip inputs whose watches were already registered by a
		// previous resume pass. registerSweepSpendWatch also dedups
		// against pendingSweepInputs, but the explicit guard avoids
		// the per-input GetIntent round-trip.
		if _, armed := pending.inputs[in.Outpoint]; armed {
			continue
		}

		intent, err := a.sweepStore.GetIntent(ctx, in.Outpoint)
		if err != nil || intent == nil {
			log.WarnS(ctx, "Failed to load intent for sweep resume",
				err,
				slog.String("outpoint", in.Outpoint.String()),
			)

			allInputsArmed = false

			continue
		}

		// Use the boarding intent's confirmation height (not the
		// current best height) so chainsource scans forward from the
		// original confirmation. A daemon offline for several blocks
		// while a sweep was in flight may have missed the on-chain
		// spend; bestHeight as a hint would cause lnd to skip past
		// it and strand the sweep at "published" forever.
		err = a.registerSweepSpendWatch(
			ctx, *intent, uint32(intent.ChainInfo.ConfHeight),
			pending,
		)
		if err != nil {
			log.WarnS(
				ctx,
				"Failed to re-register sweep spend watch",
				err,
				slog.String("outpoint", in.Outpoint.String()),
			)

			allInputsArmed = false
		}
	}

	if !pending.submitted {
		var confPkScript []byte
		if len(sweep.Tx.TxOut) > 0 {
			confPkScript = sweep.Tx.TxOut[0].PkScript
		}

		// Use the sweep's persisted creation height (not the current
		// best height) so the txconfirm broadcaster's confirmation
		// watch scans forward from when the sweep was first built.
		// Otherwise a sweep that already confirmed during the daemon
		// outage would never trigger TxConfirmed.
		err := a.submitSweepConfirmer(
			ctx, sweep.Tx, confPkScript,
			uint32(sweep.CreatedHeight),
		)
		if err != nil {
			log.WarnS(ctx,
				"Failed to re-submit boarding sweep at "+
					"startup",
				err, slog.String("txid", sweep.Txid.String()))

			return false
		}
		pending.submitted = true

		// Advance the on-disk status to 'published' if the original
		// publish path crashed before MarkBoardingSweepPublished
		// landed. Without this the sweep row stays at 'pending' and
		// — when the spend cascade later fires —
		// MarkBoardingSweepInputSpent jumps the row directly from
		// 'pending' to 'confirmed' / 'external_resolved', producing
		// a non-monotonic operator-visible status timeline that CLI
		// tooling diffing snapshots cannot reason about. The store
		// mutation is idempotent: re-flipping an already-'published'
		// row is a no-op.
		err = a.sweepStore.MarkBoardingSweepPublished(
			ctx, sweep.Txid,
		)
		if err != nil {
			log.WarnS(ctx, "Failed to mark resumed sweep published",
				err,
				slog.String("txid", sweep.Txid.String()),
			)
		}
	}

	return allInputsArmed
}
