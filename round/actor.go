//nolint:ll
package round

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/metrics"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/timeout"
	"github.com/lightninglabs/wavelength/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

// Compile-time assertion that RoundClientActor implements actor.Stoppable.
var _ actor.Stoppable = (*RoundClientActor)(nil)

const defaultForfeitCollectionTimeout = 2 * time.Minute

// defaultRegistrationTimeout bounds how long the client waits in
// IntentSentState for the server to acknowledge a JoinRoundRequest with a
// RoundJoined admission watermark. The ack is a lightweight reply (not the
// seal-time quote), so it should arrive within a network round-trip plus brief
// server-side queuing; 60s is generous enough to tolerate a slow server or
// link while still bounding how long forfeit-reserved inputs sit stranded in
// pending-forfeit when the server never responds (wavelength#653). It is
// configurable via RoundClientConfig.RegistrationTimeout for operators and
// tests that need a different bound.
const defaultRegistrationTimeout = 60 * time.Second

// defaultStatusReconcileTimeout bounds how long a forfeit-bearing round sits
// in InputSigSentState with no confirmation, no resolved failure, and no
// status answer before the client probes the operator with a
// QueryRoundStatus (wavelength#844). The window must comfortably exceed the
// operator's input-signature collection phase so a healthy round is not
// probed mid-ceremony; an early probe is harmless (the operator answers
// in-flight and the client keeps waiting), so the constant errs toward
// responsiveness rather than silence. It also serves as the retry interval
// between unanswered probes.
const defaultStatusReconcileTimeout = 90 * time.Second

// defaultRefreshRegistrationDelay is the quiet period used to coalesce
// expiry-driven refreshes before registering their round. Block epochs can
// make several VTXO actors request refreshes back-to-back; registering the
// first one immediately would move its FSM out of assembly and split the
// remaining VTXOs into competing same-client registrations.
const defaultRefreshRegistrationDelay = 500 * time.Millisecond

// RefreshVTXORequest is sent from a VTXO actor when its VTXO is approaching
// expiry and needs to be refreshed in a new round. The round actor should
// queue this VTXO for inclusion in the next batch swap.
//
// This request contains all information needed to build a forfeit request
// (for the connector tree) and a VTXORequest (for the new VTXO in the VTXT).
// The same client key is typically reused for the new VTXO.
//
// NOTE: This type is an actor message (RoundReceivable), not an FSM event.
// The actor translates it into an IntentPackage{Forfeits: [1], VTXOs: [1]}.
type RefreshVTXORequest struct {
	actor.BaseMessage

	// VTXOOutpoint identifies the VTXO to refresh.
	VTXOOutpoint wire.OutPoint

	// Amount is the VTXO value in satoshis.
	Amount int64

	// OperatorFee is an advisory-only hint under the seal-time fee
	// handshake (#270). The VTXO actor's RefreshFeeQuoter fills
	// this field during auto-refresh so downstream emitters (logs,
	// metrics) can see what the actor thought the fee would be
	// when it decided to refresh, but the round actor does NOT
	// subtract it from Amount — the new VTXO request is emitted
	// with IsChange=true and the server fills in the residual at
	// seal time. Zero is valid and expected under a zero fee
	// schedule or when the quote RPC was unreachable.
	OperatorFee int64

	// PolicyTemplate is the semantic arkscript policy for the refreshed
	// output. This is the authoritative round-registration representation.
	PolicyTemplate []byte

	// OwnerKey is the local owner descriptor for the refreshed output. This
	// remains stable across refreshes and is used when persisting the
	// confirmed VTXO locally.
	OwnerKey keychain.KeyDescriptor

	// SigningKey is the key descriptor for signing the new VTXO's tree.
	SigningKey keychain.KeyDescriptor
}

// RoundReceivable implements actormsg.RoundReceivable marker interface.
func (e *RefreshVTXORequest) RoundReceivable() {}

// MessageType returns the message type for logging.
func (e *RefreshVTXORequest) MessageType() string {
	return "RefreshVTXORequest"
}

// RegisterIntentRequest is the primary entry point for registering a
// pre-composed intent package with the round actor. The caller (typically
// the wallet) builds the full IntentPackage containing forfeits, VTXO
// requests, and/or leave requests. The round actor validates and registers
// it with the FSM, then notifies affected VTXO actors.
type RegisterIntentRequest struct {
	actor.BaseMessage

	// Package is the fully composed round intent bundle.
	Package *IntentPackage

	// TriggerRegistration when true causes immediate
	// IntentRequested after the intent is accepted.
	TriggerRegistration bool
}

// RoundReceivable implements actormsg.RoundReceivable marker interface.
func (e *RegisterIntentRequest) RoundReceivable() {}

// MessageType returns the message type for logging.
func (e *RegisterIntentRequest) MessageType() string {
	return "RegisterIntentRequest"
}

// buildVTXORequestFromRefresh constructs a types.VTXORequest from a
// RefreshVTXORequest. The refresh request contains all info needed to
// create the new VTXO output in the round. Origin is always
// VTXOOriginRoundRefresh so the round actor routes the downstream
// ledger emission to SourceRoundRefresh (cancels the paired forfeit on
// transfers_out rather than crediting wallet_balance).
//
// Under the seal-time fee handshake (#270) the server is the amount
// authority and requires exactly one IsChange=true marker across the
// composed intent (or zero markers when there is only one output). The
// per-VTXO request leaves IsChange unset; the FSM's IntentRequested
// handler runs designateChangeMarker over the fully-accumulated intent
// to stamp a single marker. Stamping here would produce N markers when
// N expiring VTXOs auto-refresh into the same assembling round, which
// the server rejects with INVALID_CHANGE_DESIGNATION.
func buildVTXORequestFromRefresh(req *RefreshVTXORequest) types.VTXORequest {
	return types.VTXORequest{
		PolicyTemplate: req.PolicyTemplate,
		Amount:         btcutil.Amount(req.Amount),
		ClientKey:      req.OwnerKey.PubKey,
		OwnerKey:       req.OwnerKey,
		SigningKey:     req.SigningKey,
		Origin:         types.VTXOOriginRoundRefresh,
	}
}

// makeTimeoutID builds a composite timeout ID from a round map key and phase.
// The key is used verbatim (it may itself contain ":" for temp keys, e.g.
// "temp:<uuid>"), so parseTimeoutID splits on the final ":" to recover the
// phase.
func makeTimeoutID(key RoundKeyStr, phase TimeoutPhase) timeout.ID {
	return timeout.ID(fmt.Sprintf("%s:%s", key, phase))
}

// parseTimeoutID extracts the round map key and phase from a composite timeout
// ID. The phase is the suffix after the final ":"; everything before it is the
// round key (which may contain ":" itself for temp-keyed rounds).
func parseTimeoutID(id timeout.ID) (RoundKeyStr, TimeoutPhase, error) {
	s := string(id)
	idx := strings.LastIndex(s, ":")
	if idx < 0 {
		return "", "", fmt.Errorf("invalid timeout ID format: %s", id)
	}

	return RoundKeyStr(s[:idx]), TimeoutPhase(s[idx+1:]), nil
}

// RoundFSM wraps a state machine instance for a specific round.
type RoundFSM struct {
	// FSM is the state machine for this round. The baselib protofsm uses 3
	// type parameters: InternalEvent, OutboxEvent, Env.
	FSM *ClientStateMachine

	// Key is the current key for this round in the actor's map. It starts
	// as a TempRoundKey and is upgraded to a RoundID when the server
	// assigns one.
	Key RoundKey

	// RoundID is the unique identifier for this round, assigned by the
	// server. Zero value until the server assigns an ID.
	RoundID RoundID

	// TxID is the commitment transaction ID for this round.
	TxID chainhash.Hash

	// CommitmentTx is the commitment transaction as a PSBT, used for
	// registering confirmation notifications with the correct pkScript.
	CommitmentTx fn.Option[*psbt.Packet]
}

// RoundClientActor wraps the client boarding FSM in an actor interface. The
// actor manages the FSM lifecycle, handles incoming actor messages, converts
// them to FSM events, processes outbox messages, and integrates with the
// chainsource actor for chain monitoring.
//
// Architecture:
//   - Actor holds FSMs (protofsm.StateMachine) in a unified map.
//   - Rounds start with TempRoundKey, re-keyed to RoundID on server response.
//   - Actor receives actor messages (ClientMsg).
//   - Actor converts messages to FSM events.
//   - FSM processes events producing new state and outbox.
//   - Actor processes outbox by sending messages to server/chainsource.
type RoundClientActor struct {
	// cfg contains all the configuration for this actor.
	cfg *RoundClientConfig

	// log is the logger for this actor instance.
	log btclog.Logger

	// runCtx is the actor lifecycle context captured at Start. It is used
	// for registrations that must outlive one Receive turn but should
	// still stop when the actor is stopped.
	runCtx context.Context //nolint:containedctx

	// rounds tracks all round FSMs keyed by their RoundKey. Rounds start
	// with a TempRoundKey and are re-keyed to their server-assigned RoundID
	// when received via RoundJoined. This enables concurrent round
	// assembly.
	rounds map[RoundKeyStr]*RoundFSM

	// commitmentTxIndex maps commitment transaction IDs to their round
	// keys for routing confirmation events.
	commitmentTxIndex map[chainhash.Hash]RoundKeyStr

	// pendingQuotes buffers JoinRoundQuoteReceived envelopes that
	// arrive before the matching RoundJoined re-keys the FSM. The
	// mailbox contract (docs/RPC_MAILBOX_CONTRACT.md:90-98) allows
	// out-of-order delivery, so a quote may land while the FSM is
	// still under its temp key. handleRoundJoined drains the buffer
	// after re-keying so the quote reaches its FSM without a
	// silent drop. Bounded by maxPendingQuotes to keep a hostile
	// server from flooding the buffer.
	pendingQuotes map[RoundID]*JoinRoundQuoteReceived

	// env is the base FSM environment template containing all dependencies.
	// Each new round FSM gets a copy with a fresh StartHeight.
	env *ClientEnvironment
}

// RoundClientConfig houses the configuration for a RoundClientActor.
type RoundClientConfig struct {
	// Name uniquely identifies this actor instance.
	Name string

	// Logger is the logger for this actor instance. If nil, uses the global
	// package logger.
	Logger btclog.Logger

	// Wallet provides MuSig2 signing capabilities needed for round
	// participation. Boarding address creation is handled by the wallet
	// actor.
	Wallet ClientWallet

	// SigningExecutor runs independent VTXO MuSig2 sessions with bounded
	// concurrency. If nil, the actor preserves serial behavior.
	SigningExecutor SigningExecutor

	// RoundStore persists round coordination and checkpointing.
	RoundStore RoundStore

	// VTXOStore persists off-chain balance.
	VTXOStore VTXOStore

	// OperatorTerms contains the operator's parameters.
	OperatorTerms *types.OperatorTerms

	// ServerConn is a reference to the ServerConnectionActor for sending
	// messages to the Ark server.
	ServerConn actor.TellOnlyRef[serverconn.ServerConnMsg]

	// ChainSource is a reference to the ChainSource actor for registering
	// confirmation notifications for commitment transactions and querying
	// block height.
	ChainSource actor.ActorRef[chainsource.ChainSourceMsg, chainsource.ChainSourceResp]

	// WalletActor is a reference to the Ark wallet actor. The round actor
	// registers to receive BoardingUtxoConfirmedEvent notifications when
	// new boarding UTXOs are confirmed.
	WalletActor actor.ActorRef[wallet.WalletMsg, wallet.WalletResp]

	// SelfRef is a reference to this actor for receiving asynchronous
	// notifications (e.g., confirmations from ChainSource).
	SelfRef actor.TellOnlyRef[actormsg.RoundReceivable]

	// TimeoutActor schedules and cancels round phase timeouts.
	TimeoutActor actor.TellOnlyRef[timeout.Msg]

	// ChainParams are the Bitcoin network parameters.
	ChainParams *chaincfg.Params

	// MaxOperatorFee is the maximum fee the client is willing to pay per
	// round. This limits the difference between total boarding input
	// amounts and total VTXO output amounts.
	MaxOperatorFee btcutil.Amount

	// VTXOManager receives VTXO creation notifications after rounds
	// complete. The round actor forwards VTXOCreatedNotification
	// messages so newly created VTXOs get an active VTXO actor.
	// Optional - if nil, notifications are not forwarded.
	VTXOManager actor.TellOnlyRef[VTXOManagerMsg]

	// DropCustomForfeitSigningContexts clears daemon-local signing
	// metadata for custom refresh inputs when a round fails before the
	// connector-bound forfeit signing request is produced. When nil, only
	// the VTXO manager's custom forfeit actors are dropped.
	DropCustomForfeitSigningContexts func(context.Context,
		[]wire.OutPoint) error

	// ActorSystem enables direct communication with VTXO actors via service
	// keys. Used to send ForfeitRequestEvent and ForfeitConfirmedEvent to
	// specific VTXO actors.
	ActorSystem *actor.ActorSystem

	// DisableJoinRequestAuth skips BIP-322 join authorization
	// generation. This should only be set in focused unit tests.
	DisableJoinRequestAuth bool

	// ForfeitCollectionTimeout is the max wall-clock duration to wait for
	// forfeit signatures after entering ForfeitSignaturesCollectingState.
	// If zero, a conservative default is used.
	ForfeitCollectionTimeout time.Duration

	// RegistrationTimeout is the max wall-clock duration to wait in
	// IntentSentState for the server's RoundJoined admission watermark
	// before failing the round (recoverable) and releasing any
	// forfeit-reserved inputs. If zero, defaultRegistrationTimeout is used.
	// A negative value disables the timeout (rounds wait indefinitely for
	// admission), which restores the pre-#653 behavior.
	RegistrationTimeout time.Duration

	// StatusReconcileTimeout bounds how long a forfeit-bearing round sits
	// in InputSigSentState — forfeit signatures out, no confirmation, no
	// resolved failure — before the client probes the operator with a
	// QueryRoundStatus (wavelength#844). It doubles as the retry interval
	// between probes. If zero, defaultStatusReconcileTimeout is used. A
	// negative value disables the reconcile, restoring the pre-#844
	// behavior (a stranded reservation waits for the #823 startup sweep).
	StatusReconcileTimeout time.Duration

	// OwnedScriptChecker determines whether a VTXO pkScript belongs
	// to the local wallet. When nil, all VTXOs pass the ownership
	// check (backward-compatible default for tests).
	OwnedScriptChecker OwnedScriptChecker

	// OwnedScriptRegistrar registers pkScripts as locally owned.
	// Called when building VTXO intents so the checker can
	// recognize them at confirmation time. When nil, registration
	// is skipped (tests).
	OwnedScriptRegistrar OwnedScriptRegistrar

	// LedgerSink is an optional reference to the client-side
	// ledger accounting actor. When set, the round actor forwards
	// VTXOReceivedMsg / VTXOSentMsg / FeePaidMsg events as round
	// confirmations land so the local accounting DB stays in sync
	// with on-chain reality. When None (tests, or rounds without
	// accounting wired), ledger emission is silently skipped.
	LedgerSink fn.Option[ledger.Sink]

	// MetricsSink is an optional reference to the client-side metrics
	// actor. When set, the round actor emits a RoundCompletedMsg as
	// each round reaches a terminal outcome so the
	// waved_rounds_completed_total counter reflects reality. When
	// None (metrics disabled, or tests), metric emission is silently
	// skipped. Mirrors LedgerSink: the round actor is the natural seam
	// because terminal round outcomes are observed here, not at any
	// RPC boundary.
	MetricsSink fn.Option[metrics.Sink]
}

// NewRoundClientActor creates a new client actor with the provided
// configuration. FSMs are created on-demand when boarding intents arrive.
//
// The FSM uses interfaces directly and calls lib package functions as needed.
// Chain operations are handled via outbox messages (not direct calls).
func NewRoundClientActor(cfg *RoundClientConfig) fn.Result[*RoundClientActor] {
	// Use the configured logger, falling back to a disabled logger during
	// construction when no instance logger was provided.
	actorLog := cfg.Logger
	if actorLog == nil {
		actorLog = btclog.Disabled
	}

	// Create base FSM environment template with direct interface
	// assignments. The FSM will call lib functions directly when needed
	// (e.g., lib.NewTreeSignerSession, signing helpers). StartHeight is set
	// to 0 here and will be set per-round when FSMs are created.
	env := &ClientEnvironment{
		RoundStore:             cfg.RoundStore,
		VTXOStore:              cfg.VTXOStore,
		Wallet:                 cfg.Wallet,
		SigningExecutor:        cfg.SigningExecutor,
		OperatorTerms:          cfg.OperatorTerms,
		ChainParams:            cfg.ChainParams,
		MaxOperatorFee:         cfg.MaxOperatorFee,
		Log:                    actorLog,
		DisableJoinRequestAuth: cfg.DisableJoinRequestAuth,
		OwnedScriptChecker:     cfg.OwnedScriptChecker,
	}
	if env.SigningExecutor == nil {
		env.SigningExecutor = NewSigningExecutor(1)
	}

	// The sweep delay is no longer a global operator term: it is delivered
	// per round in the batch info, so the sweep-vs-exit-delay security
	// check runs per round in CommitmentTxReceivedState rather than once at
	// actor construction.

	if cfg.TimeoutActor == nil {
		return fn.Err[*RoundClientActor](
			fmt.Errorf("timeout actor is required"),
		)
	}

	// No FSM is created here. FSMs are created on-demand when boarding
	// intents arrive via createNewRound().
	forfeitTimeout := cfg.ForfeitCollectionTimeout
	if forfeitTimeout <= 0 {
		forfeitTimeout = defaultForfeitCollectionTimeout
	}
	env.ForfeitCollectionTimeout = forfeitTimeout

	// A zero value selects the default; a negative value is an explicit
	// opt-out (wait for admission indefinitely). Only the zero case is
	// rewritten to the default.
	registrationTimeout := cfg.RegistrationTimeout
	if registrationTimeout == 0 {
		registrationTimeout = defaultRegistrationTimeout
	}
	env.RegistrationTimeout = registrationTimeout

	// Same zero-selects-default, negative-disables convention as the
	// registration timeout.
	statusReconcileTimeout := cfg.StatusReconcileTimeout
	if statusReconcileTimeout == 0 {
		statusReconcileTimeout = defaultStatusReconcileTimeout
	}
	env.StatusReconcileTimeout = statusReconcileTimeout

	actor := &RoundClientActor{
		cfg:               cfg,
		log:               actorLog,
		rounds:            make(map[RoundKeyStr]*RoundFSM),
		commitmentTxIndex: make(map[chainhash.Hash]RoundKeyStr),
		pendingQuotes:     make(map[RoundID]*JoinRoundQuoteReceived),
		env:               env,
	}

	// The base env is used as a template for per-round FSM environments.
	// Wire in the actor height query function so join-auth can anchor
	// intent validity metadata to the current chain height at signing time.
	actor.env.QueryBestHeight = actor.queryBestHeight

	return fn.Ok(actor)
}

// emitVTXOsReceived posts ledger entries per VTXO carried on a
// VTXOCreatedNotification, routing each one by the Origin tag
// the wallet stamped at intent-composition time.
//
// Mapping:
//
//   - VTXOOriginRoundBoarding: on-chain wallet funds entered the
//     Ark layer. Emits VTXOReceivedMsg{Source=SourceRoundBoarding}
//     which debits vtxo_balance and credits wallet_balance.
//
//   - VTXOOriginRoundRefresh: includes both straight refreshes and
//     directed-send self-change. Emits a paired
//     VTXOSentMsg{RoundID} + VTXOReceivedMsg{Source=
//     SourceRoundRefresh} for the gross amount. Their legs cancel
//     on transfers_out, leaving only the operator fee (booked
//     separately by the fee-paid emission path) as the real net
//     change on vtxo_balance. Crediting wallet_balance on a refresh
//     would be wrong since no wallet UTXO funded the new VTXO.
//
//   - VTXOOriginRoundTransfer: a true in-round receive from another
//     participant's directed send. Emits
//     VTXOReceivedMsg{Source=SourceRoundTransfer} crediting
//     transfers_in as counterparty revenue.
//
//   - VTXOOriginUnknown: origin was never set. The caller is a
//     legacy or test path; skip emission to avoid misclassifying.
//     A misclassification here silently corrupts the chart of
//     accounts, so "do nothing" is strictly safer than picking a
//     default.
//
// Emission is best-effort: Tell failures are logged but not
// propagated, so a momentary ledger outage never breaks the
// round actor's downstream dispatch loop.
func (a *RoundClientActor) emitVTXOsReceived(ctx context.Context,
	n *VTXOCreatedNotification) {

	a.cfg.LedgerSink.WhenSome(func(sink ledger.Sink) {
		if n == nil {
			return
		}

		roundID := roundIDBytes(n.RoundID)

		for _, outflow := range n.Outflows {
			if outflow.AmountSat <= 0 {
				continue
			}

			a.tellLedger(ctx, sink, &ledger.VTXOSentMsg{
				AmountSat:      outflow.AmountSat,
				RoundID:        roundID,
				IdempotencyKey: outflow.IdempotencyKey,
			}, "", n.RoundID)
		}

		for _, v := range n.VTXOs {
			if v == nil || v.Amount <= 0 {
				continue
			}

			a.emitOwnedVTXOLedgerEntry(ctx, sink, roundID, v, n)
		}

		a.emitRoundFee(ctx, sink, roundID, n)
	})
}

// emitRoundCompleted reports a terminal round outcome to the metrics
// actor so the waved_rounds_completed_total counter advances. The
// round actor is the natural seam: terminal outcomes surface here as
// RoundCompletedNotification / RoundFailedNotification, with no RPC
// boundary that could observe them. Like ledger emission, this is
// best-effort and fire-and-forget — a Tell failure is logged at debug
// level and never fails the enclosing notification dispatch. Status is
// "confirmed" or "failed", matching the counter's label set.
func (a *RoundClientActor) emitRoundCompleted(ctx context.Context, roundID,
	status string) {

	a.cfg.MetricsSink.WhenSome(func(sink metrics.Sink) {
		msg := &metrics.RoundCompletedMsg{
			RoundID: roundID,
			Status:  status,
		}
		if err := sink.Tell(ctx, msg); err != nil {
			a.log.DebugS(ctx, "Failed to emit round metric",
				err,
				slog.String("round_id", roundID),
				slog.String("status", status),
			)
		}
	})
}

// handleTerminalJobFailure reacts to a round that failed with a
// terminal-for-job code (e.g. the operator could not fund the commitment tx).
// The accompanying ReleaseForfeitReservation has already returned the reserved
// VTXOs to the live set, so here we drop the originating job's persisted
// pending intent, keyed by its forfeited outpoints. That halts the recoverable
// replay loop: without it, the send-onchain intent replayer would re-submit
// the same inputs into the same operator on every restart. Dropping is
// best-effort — a store error degrades to the pre-existing replay behavior
// rather than wedging the round actor, so it is logged, not returned.
func (a *RoundClientActor) handleTerminalJobFailure(ctx context.Context,
	m *TerminalJobFailedNotification) {

	roundIDStr := "none"
	m.RoundID.WhenSome(func(id RoundID) {
		roundIDStr = id.String()
	})

	a.log.WarnS(ctx, "Round failed terminally; dropping originating "+
		"job intent",
		nil,
		slog.String("round_id", roundIDStr),
		slog.String("reason", m.Reason),
		slog.Int("failure_code", int(m.FailureCode)),
		slog.Int("forfeit_outpoint_count", len(m.ForfeitOutpoints)),
	)

	// This mark runs in the same processOutbox turn that just enqueued the
	// ReleaseForfeitReservation Tell to the VTXO manager, before that
	// manager has moved the coins back to LiveState. A programmatic retry
	// of the same send therefore cannot observe the released coin and
	// re-persist a fresh intent between the release and this mark, so we
	// can key the mark on the forfeited outpoints without racing a live
	// retry. A future refactor that defers this mark to a later turn must
	// revisit that assumption.
	if err := a.cfg.RoundStore.FailForfeitIntents(
		ctx, m.ForfeitOutpoints, m.Reason, m.FailureCode,
	); err != nil {

		a.log.WarnS(ctx, "Failed to mark pending intent failed after "+
			"terminal round failure; it may replay on restart",
			err,
			slog.String("round_id", roundIDStr),
		)
	}
}

// emitRoundJoined reports a round-join attempt to the metrics actor so
// waved_rounds_joined_total advances. It is emitted from createNewRound
// so it counts every round the client assembles — manual and eager alike
// — keeping it symmetric with emitRoundCompleted. Best-effort and
// fire-and-forget: a Tell failure is logged at debug level and never
// fails round assembly.
func (a *RoundClientActor) emitRoundJoined(ctx context.Context,
	roundID string) {

	a.cfg.MetricsSink.WhenSome(func(sink metrics.Sink) {
		msg := &metrics.RoundJoinedMsg{RoundID: roundID}
		if err := sink.Tell(ctx, msg); err != nil {
			a.log.DebugS(ctx, "Failed to emit round joined metric",
				err,
				slog.String("round_id", roundID),
			)
		}
	})
}

// emitRoundFee sends a single FeePaidMsg to the ledger actor per
// round when the client actually paid an operator fee. Boarding
// rounds use FeeTypeBoarding; all other VTXO-spend fees use
// FeeTypeRefresh. OperatorFeeSat <= 0 suppresses emission; the
// transition helper clamps to zero when outputs exceed inputs so
// a malformed intent never produces a negative ledger row.
//
// Emission is best-effort: a Tell failure does not fail the
// enclosing notification dispatch.
func (a *RoundClientActor) emitRoundFee(ctx context.Context, sink ledger.Sink,
	roundID [16]byte, n *VTXOCreatedNotification) {

	if n.OperatorFeeSat <= 0 {
		return
	}

	feeType := n.OperatorFeeType
	if feeType == "" {
		feeType = ledger.FeeTypeRefresh
	}

	a.tellLedger(ctx, sink, &ledger.FeePaidMsg{
		RoundID:     roundID,
		AmountSat:   n.OperatorFeeSat,
		FeeType:     feeType,
		BlockHeight: uint32(n.CreatedHeight),
	}, "", n.RoundID)
}

// emitOwnedVTXOLedgerEntry sends the per-VTXO ledger traffic for
// one owned ClientVTXO according to its Origin. Split out of
// emitVTXOsReceived so each origin branch is narrow and reads
// linearly, and so the paired Send+Receive emission for
// refresh-origin stays co-located with the receive-only emissions
// for the other cases.
func (a *RoundClientActor) emitOwnedVTXOLedgerEntry(ctx context.Context,
	sink ledger.Sink, roundID [16]byte, v *ClientVTXO,
	n *VTXOCreatedNotification) {

	outpoint := v.Outpoint.String()

	switch v.Origin {
	case types.VTXOOriginRoundBoarding:
		// Genuine boarding: wallet \u2192 VTXO.
		a.tellLedger(ctx, sink, &ledger.VTXOReceivedMsg{
			OutpointHash:  v.Outpoint.Hash,
			OutpointIndex: v.Outpoint.Index,
			AmountSat:     int64(v.Amount),
			Source:        ledger.SourceRoundBoarding,
			RoundID:       roundID,
		}, outpoint, n.RoundID)

	case types.VTXOOriginRoundRefresh:
		// Paired emission so transfers_out nets to zero and
		// only the fee (emitted by task B) actually moves
		// vtxo_balance. The gross amount on both messages
		// must match for the cancellation to work; the
		// outpoint field on VTXOSentMsg gives handleVTXOSent
		// a per-VTXO idempotency key so two refreshes in one
		// round don't collide on the round-scoped partial
		// unique index.
		a.tellLedger(ctx, sink, &ledger.VTXOSentMsg{
			Outpoint:  v.Outpoint,
			AmountSat: int64(v.Amount),
			RoundID:   roundID,
		}, outpoint, n.RoundID)

		a.tellLedger(ctx, sink, &ledger.VTXOReceivedMsg{
			OutpointHash:  v.Outpoint.Hash,
			OutpointIndex: v.Outpoint.Index,
			AmountSat:     int64(v.Amount),
			Source:        ledger.SourceRoundRefresh,
			RoundID:       roundID,
		}, outpoint, n.RoundID)

	case types.VTXOOriginRoundTransfer:
		// Actual in-round receive from another participant.
		a.tellLedger(ctx, sink, &ledger.VTXOReceivedMsg{
			OutpointHash:  v.Outpoint.Hash,
			OutpointIndex: v.Outpoint.Index,
			AmountSat:     int64(v.Amount),
			Source:        ledger.SourceRoundTransfer,
			RoundID:       roundID,
		}, outpoint, n.RoundID)

	default:
		// Unknown origin: the composition path forgot to
		// tag. Logging-only is the strictly safer default
		// because a wrong source would corrupt the chart of
		// accounts silently.
		a.log.WarnS(ctx,
			"Skipping ledger emission for VTXO with "+
				"unknown origin", nil,
			slog.String("outpoint", outpoint),
			slog.String("round_id", n.RoundID),
			slog.String("origin", v.Origin.String()))
	}
}

// tellLedger is a small helper wrapping sink.Tell with the
// per-outpoint warning path, so the per-origin branches above
// stay short. Tell failures log-and-return rather than aborting
// the enclosing loop.
func (a *RoundClientActor) tellLedger(ctx context.Context, sink ledger.Sink,
	msg ledger.LedgerMsg, outpoint, roundID string) {

	if err := sink.Tell(ctx, msg); err != nil {
		a.log.WarnS(ctx,
			"Failed to emit ledger message", err,
			slog.String("msg_type",
				fmt.Sprintf("%T", msg)),
			slog.String("outpoint", outpoint),
			slog.String("round_id", roundID))
	}
}

// roundIDBytes parses the canonical UUID string form of a RoundID
// into its 16-byte representation for the ledger message. Returns
// the zero array on any parse failure -- the ledger actor treats a
// zero round_id as NULL via roundIDOrNil, so a malformed RoundID
// degrades to a non-round-tagged entry rather than rejecting the
// message entirely.
func roundIDBytes(s string) [16]byte {
	var out [16]byte
	if s == "" {
		return out
	}

	id, err := uuid.Parse(s)
	if err != nil {
		return out
	}

	copy(out[:], id[:])

	return out
}

// queryBestHeight queries the ChainSource for the current best block height.
// This wraps the Ask->Await->Unpack pattern for height queries, providing a
// clean interface for callers that need the current height.
func (a *RoundClientActor) queryBestHeight(ctx context.Context) (uint32,
	error) {

	heightFuture := a.cfg.ChainSource.Ask(
		ctx, &chainsource.BestHeightRequest{},
	)
	heightResult := heightFuture.Await(ctx)

	heightResp, err := heightResult.Unpack()
	if err != nil {
		return 0, fmt.Errorf("failed to query best height: %w", err)
	}

	bestHeightResp, ok := heightResp.(*chainsource.BestHeightResponse)
	if !ok {
		return 0, fmt.Errorf("unexpected height response type: %T",
			heightResp)
	}

	return uint32(bestHeightResp.Height), nil
}

// createRoundFSMFromDB creates a new FSM instance for a specific round,
// restoring from checkpointed state. Uses FetchState to load both round data
// and FSM state atomically. Used when loading active rounds from database on
// startup.
func (a *RoundClientActor) createRoundFSMFromDB(ctx context.Context,
	roundID RoundID) (*RoundFSM, error) {

	round, state, err := a.cfg.RoundStore.FetchState(ctx, roundID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch round state: %w", err)
	}

	// Use the StartHeight stored in the round when it was created. This
	// ensures we scan from the original starting point, not the current
	// height, which could miss confirmations if the tx was already mined.
	startHeight := round.StartHeight

	fsmPrefix := roundID.LogPrefix()
	fsmLogger := a.log.WithPrefix(fsmPrefix)

	env := &ClientEnvironment{
		RoundStore:             a.cfg.RoundStore,
		VTXOStore:              a.cfg.VTXOStore,
		Wallet:                 a.cfg.Wallet,
		SigningExecutor:        a.env.SigningExecutor,
		OperatorTerms:          a.cfg.OperatorTerms,
		ChainParams:            a.cfg.ChainParams,
		MaxOperatorFee:         a.cfg.MaxOperatorFee,
		Log:                    fsmLogger,
		StartHeight:            startHeight,
		QueryBestHeight:        a.queryBestHeight,
		DisableJoinRequestAuth: a.cfg.DisableJoinRequestAuth,
		ForfeitCollectionTimeout: a.
			env.ForfeitCollectionTimeout,
		RegistrationTimeout:    a.env.RegistrationTimeout,
		StatusReconcileTimeout: a.env.StatusReconcileTimeout,
		RoundKey:               RoundKeyStr(roundID.KeyString()),
		OwnedScriptChecker:     a.cfg.OwnedScriptChecker,
	}
	fsmCfg := ClientStateMachineCfg{
		Logger: fsmLogger,
		ErrorReporter: newContextErrorReporter(
			a.lifecycleCtx(ctx), fsmPrefix,
		),
		InitialState: state,
		Env:          env,
	}
	fsm := protofsm.NewStateMachine(fsmCfg)
	a.startRoundFSM(ctx, &fsm)

	a.log.InfoS(ctx, "Created round FSM from checkpoint",
		slog.String("round_id", round.RoundID.String()),
		slog.String("initial_state", state.String()),
	)

	txid := fn.MapOptionZ(
		round.CommitmentTx, func(p *psbt.Packet) chainhash.Hash {
			return p.UnsignedTx.TxHash()
		},
	)

	return &RoundFSM{
		FSM:          &fsm,
		Key:          roundID,
		RoundID:      round.RoundID,
		TxID:         txid,
		CommitmentTx: round.CommitmentTx,
	}, nil
}

// createNewRound creates a new round FSM with a temporary key when a boarding
// intent arrives. The round starts in Idle state and will be re-keyed to a
// server-assigned RoundID when RoundJoined is received.
func (a *RoundClientActor) createNewRound(ctx context.Context) (*RoundFSM,
	error) {

	// The client is starting a fresh round, so any rounds that previously
	// settled in the terminal failed state have served their observability
	// purpose and can be swept now (see reapFailedRounds).
	a.reapFailedRounds(ctx)

	tempKey, err := NewTempRoundKey()
	if err != nil {
		return nil, fmt.Errorf("failed to generate temp key: %w", err)
	}

	startHeight, err := a.queryBestHeight(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query start height: %w", err)
	}

	fsmPrefix := tempKey.LogPrefix()
	fsmLogger := a.log.WithPrefix(fsmPrefix)

	env := &ClientEnvironment{
		RoundStore:             a.cfg.RoundStore,
		VTXOStore:              a.cfg.VTXOStore,
		Wallet:                 a.cfg.Wallet,
		SigningExecutor:        a.env.SigningExecutor,
		OperatorTerms:          a.cfg.OperatorTerms,
		ChainParams:            a.cfg.ChainParams,
		MaxOperatorFee:         a.cfg.MaxOperatorFee,
		Log:                    fsmLogger,
		StartHeight:            startHeight,
		QueryBestHeight:        a.queryBestHeight,
		DisableJoinRequestAuth: a.cfg.DisableJoinRequestAuth,
		ForfeitCollectionTimeout: a.
			env.ForfeitCollectionTimeout,
		RegistrationTimeout:    a.env.RegistrationTimeout,
		StatusReconcileTimeout: a.env.StatusReconcileTimeout,
		RoundKey:               RoundKeyStr(tempKey.KeyString()),
		OwnedScriptChecker:     a.cfg.OwnedScriptChecker,
	}
	fsmCfg := ClientStateMachineCfg{
		Logger: fsmLogger,
		ErrorReporter: newContextErrorReporter(
			a.lifecycleCtx(ctx), fsmPrefix,
		),
		InitialState: &Idle{},
		Env:          env,
	}
	fsm := protofsm.NewStateMachine(fsmCfg)
	a.startRoundFSM(ctx, &fsm)

	roundFSM := &RoundFSM{
		FSM: &fsm,
		Key: tempKey,
	}

	keyStr := RoundKeyStr(tempKey.KeyString())
	a.rounds[keyStr] = roundFSM

	a.log.InfoS(ctx, "Created new round FSM",
		slog.String("temp_key", tempKey.String()),
		slog.Int("start_height", int(startHeight)),
	)

	// Count the join attempt here, at the one seam every round passes
	// through exactly once. This keeps waved_rounds_joined_total
	// symmetric with rounds_completed_total (also actor-emitted): both
	// manual JoinNextRound and eager/automatic joins assemble their
	// round through createNewRound, so counting at the RPC boundary
	// would miss eager joins and let the completed/joined ratio exceed
	// 100%. The round has only a temporary key at this point; the
	// counter is unlabelled, so the id is informational.
	a.emitRoundJoined(ctx, tempKey.String())

	return roundFSM, nil
}

// roundInState returns a predicate that checks if a RoundFSM is in the
// specified state type.
func roundInState[S ClientState]() fn.Pred[*RoundFSM] {
	return func(r *RoundFSM) bool {
		state, err := r.FSM.CurrentState()
		if err != nil {
			return false
		}
		_, ok := state.(S)

		return ok
	}
}

// findAssemblingRound finds a round that is currently assembling intents.
// It prioritizes PendingRoundAssembly (which already has boarding inputs)
// over Idle rounds. This ensures VTXOs are attached to rounds that have
// inputs, preventing registration failures from empty input sets.
func (a *RoundClientActor) findAssemblingRound() *RoundFSM {
	rounds := slices.Collect(maps.Values(a.rounds))

	// Prefer rounds that already have boarding intents.
	if assembling := fn.Filter(
		rounds, roundInState[*PendingRoundAssembly](),
	); len(assembling) > 0 {
		return assembling[0]
	}

	// Fall back to idle rounds.
	if idle := fn.Filter(rounds, roundInState[*Idle]()); len(idle) > 0 {
		return idle[0]
	}

	return nil
}

// findRoundByOutpoints finds a pending round (in IntentSentState)
// whose inputs match both the accepted boarding outpoints AND the
// accepted VTXO (forfeit) outpoints from a RoundJoined envelope.
// Used to correlate RoundJoined responses to the correct pending
// round when multiple rounds are in-flight concurrently.
//
// Under the seal-time fee handshake (#270) refresh / leave /
// directed-send rounds have an empty boarding set, so boarding
// alone is insufficient to tell concurrent VTXO-only rounds apart
// — the caller must match on the forfeit set as well. Returns nil
// when no round matches AND also when more than one candidate
// matches (ambiguous re-key), so the caller can log and fail rather
// than silently route into the wrong FSM.
func (a *RoundClientActor) findRoundByOutpoints(
	boardingOutpoints, vtxoOutpoints []wire.OutPoint) *RoundFSM {

	boardingSet := fn.NewSet(boardingOutpoints...)
	vtxoSet := fn.NewSet(vtxoOutpoints...)

	var match *RoundFSM
	for _, roundFSM := range a.rounds {
		state, err := roundFSM.FSM.CurrentState()
		if err != nil {
			continue
		}

		regState, ok := state.(*IntentSentState)
		if !ok {
			continue
		}

		if !boardingMatches(regState.Intents.Boarding, boardingSet) {
			continue
		}
		if !forfeitsMatch(regState.Intents.Forfeits, vtxoSet) {
			continue
		}

		if match != nil {

			// Ambiguous: at least two pending rounds have the
			// same accepted-input shape. This should not happen
			// under normal server operation (each round has a
			// distinct shape) but is possible with concurrent
			// boarding-less refreshes that all start from an
			// empty boarding set. Refusing to guess is strictly
			// safer than routing into the wrong FSM.
			return nil
		}
		match = roundFSM
	}

	return match
}

// boardingMatches checks whether the boarding outpoints of an
// intent match the supplied set exactly.
func boardingMatches(intents []BoardingIntent,
	outpoints fn.Set[wire.OutPoint]) bool {

	if uint(len(intents)) != outpoints.Size() {
		return false
	}

	for _, intent := range intents {
		if !outpoints.Contains(intent.Outpoint) {
			return false
		}
	}

	return true
}

// forfeitsMatch checks whether the forfeit VTXO outpoints of an
// intent match the supplied set exactly. Treats nil VTXOOutpoint
// entries as non-matches so the caller fails fast on malformed
// intent state rather than silently coalescing with the empty set.
func forfeitsMatch(forfeits []types.ForfeitRequest,
	outpoints fn.Set[wire.OutPoint]) bool {

	if uint(len(forfeits)) != outpoints.Size() {
		return false
	}

	for _, f := range forfeits {
		if f.VTXOOutpoint == nil {
			return false
		}
		if !outpoints.Contains(*f.VTXOOutpoint) {
			return false
		}
	}

	return true
}

// registerCommitmentConfirmation registers for confirmation monitoring of a
// commitment transaction with the chainsource actor. The commitmentTx is used
// to extract the pkScript for LND's confirmation tracking, and vtxoTrees (the
// round's validated VTXO trees, possibly nil) selects which output to watch.
func (a *RoundClientActor) registerCommitmentConfirmation(ctx context.Context,
	txid chainhash.Hash, commitmentTx fn.Option[*psbt.Packet],
	vtxoTrees map[int]*tree.Tree) {

	callerID := fmt.Sprintf("commitment-tx-%s", txid.String())

	mappedRef := chainsource.MapConfirmationEvent(
		a.cfg.SelfRef,
		func(ce chainsource.ConfirmationEvent) actormsg.RoundReceivable {
			return &ConfirmationEvent{
				Txid:          ce.Txid,
				BlockHeight:   ce.BlockHeight,
				Confirmations: ce.NumConfs,
				Tx:            ce.Tx,
			}
		},
	)

	// Extract the pkScript LND needs for confirmation tracking. Watch the
	// validated batch output (the output that receives this client's
	// funds) rather than assuming output 0; confirmationWatchScript falls
	// back to output 0 when the round carries no VTXO trees.
	var pkScript []byte
	commitmentTx.WhenSome(func(packet *psbt.Packet) {
		if packet.UnsignedTx != nil {
			pkScript = confirmationWatchScript(
				packet.UnsignedTx, vtxoTrees,
			)
		}
	})

	// Query ChainSource for current block height to use as HeightHint.
	// LND requires HeightHint > 0 for confirmation scanning.
	var heightHint uint32
	heightFuture := a.cfg.ChainSource.Ask(
		ctx, &chainsource.BestHeightRequest{},
	)
	heightResult := heightFuture.Await(ctx)
	heightResp, err := heightResult.Unpack()
	if err == nil {
		bestHeightResp, ok :=
			heightResp.(*chainsource.BestHeightResponse)
		if ok {
			heightHint = uint32(bestHeightResp.Height)
		}
	} else {
		a.log.WarnS(ctx, "Failed to get best height for confirmation",
			err,
			slog.String("txid", txid.String()),
		)
	}

	confReq := &chainsource.RegisterConfRequest{
		CallerID:    callerID,
		Txid:        &txid,
		PkScript:    pkScript,
		TargetConfs: a.cfg.OperatorTerms.MinConfirmations,
		HeightHint:  heightHint,
		NotifyActor: fn.Some(mappedRef),
	}

	if err := a.cfg.ChainSource.Tell(
		a.lifecycleCtx(ctx), confReq,
	); err != nil {

		a.log.WarnS(ctx, "Failed to register confirmation", err)
	}
}

// askEventAndProcessOutbox sends an event to the FSM and processes any
// emitted outbox messages. This consolidates a common pattern throughout
// the actor where FSM events trigger outbox processing.
func (a *RoundClientActor) askEventAndProcessOutbox(ctx context.Context,
	roundFSM *RoundFSM, event ClientEvent) error {

	future := roundFSM.FSM.AskEvent(ctx, event)
	result := future.Await(ctx)

	events, err := result.Unpack()
	if err != nil {
		return err
	}

	a.log.DebugS(
		ctx,
		"askEventAndProcessOutbox: FSM returned outbox events",
		slog.Int("event_count", len(events)),
		slog.String("input_event_type", fmt.Sprintf("%T", event)),
	)

	if len(events) > 0 {
		for i, e := range events {
			a.log.DebugS(
				ctx,
				"askEventAndProcessOutbox: outbox event",
				slog.Int("index", i),
				slog.String("type", fmt.Sprintf("%T", e)),
			)
		}
		if err := a.processOutbox(ctx, events); err != nil {
			return fmt.Errorf("failed to process outbox: %w", err)
		}
	}

	return nil
}

// replayCheckpointedServerMessages re-emits server-bound messages that are
// logically required after the InputSigSent checkpoint. This closes the gap
// where the daemon can restart after persisting the checkpoint but before the
// in-memory actor loop forwards those messages to the durable serverconn
// runtime.
func (a *RoundClientActor) replayCheckpointedServerMessages(
	ctx context.Context, roundFSM *RoundFSM,
) error {

	state, err := roundFSM.FSM.CurrentState()
	if err != nil {
		return fmt.Errorf("get current state: %w", err)
	}

	inputSigState, ok := state.(*InputSigSentState)
	if !ok {
		return nil
	}

	if len(inputSigState.InputSigs) == 0 {
		return nil
	}

	a.log.InfoS(ctx, "Replaying checkpointed boarding input signatures",
		slog.String("round_id", inputSigState.RoundID.String()),
		slog.Int("boarding_sig_count", len(inputSigState.InputSigs)),
	)

	return a.processOutbox(ctx, []ClientOutMsg{
		&SubmitForfeitSigRequest{
			RoundID:    inputSigState.RoundID,
			Signatures: inputSigState.InputSigs,
		},
	})
}

// signingSessionsFromState returns ephemeral MuSig2 sessions that still need
// cleanup when a round is stopped before partial signing completes.
func signingSessionsFromState(
	state ClientState) map[SignerKey]*tree.SignerSession {

	switch state := state.(type) {
	case *NoncesSentState:
		return state.Musig2Sessions

	case *NoncesAggregatedState:
		return state.Musig2Sessions

	default:
		return nil
	}
}

// OnStop implements actor.Stoppable to gracefully clean live MuSig2 sessions
// and stop all FSMs. This prevents external signer state and goroutines from
// leaking across a daemon restart.
func (a *RoundClientActor) OnStop(ctx context.Context) error {
	a.log.InfoS(ctx, "Stopping round client actor",
		slog.Int("rounds", len(a.rounds)),
	)

	// Stop all round FSMs.
	for keyStr, roundFSM := range a.rounds {
		a.log.DebugS(ctx, "Stopping round FSM",
			slog.String("key", string(keyStr)),
		)

		state, err := roundFSM.FSM.CurrentState()
		if err == nil {
			clientState, ok := state.(ClientState)
			if !ok {
				roundFSM.FSM.Stop()

				continue
			}

			cleanupErr := cleanupSignerSessions(
				signingSessionsFromState(clientState),
			)
			if cleanupErr != nil {
				a.log.WarnS(ctx, "Unable to clean up round "+
					"MuSig2 sessions", cleanupErr,
					slog.String("key", string(keyStr)),
				)
			}
		}

		roundFSM.FSM.Stop()
	}

	a.log.InfoS(ctx, "Round client actor stopped")

	return nil
}

// lifecycleCtx returns the actor-owned context for work that must outlive the
// current Receive call. Some tests construct actor shells without calling
// Start, so the fallback detaches cancellation from the current call while
// preserving context values for logs and tracing.
func (a *RoundClientActor) lifecycleCtx(ctx context.Context) context.Context {
	if a.runCtx != nil {
		return a.runCtx
	}

	return context.WithoutCancel(ctx)
}

// startRoundFSM starts a round state machine with the actor lifecycle rather
// than the context of the request that happened to create it.
func (a *RoundClientActor) startRoundFSM(ctx context.Context,
	fsm *ClientStateMachine) {

	fsm.Start(a.lifecycleCtx(ctx))
}

// Start initializes the actor by registering with the wallet actor to receive
// boarding UTXO confirmation notifications, and resuming any active rounds.
// This should be called once after actor creation to restore state.
func (a *RoundClientActor) Start(ctx context.Context) error {
	a.runCtx = ctx

	a.log.InfoS(ctx, "Starting round client actor",
		slog.String("name", a.cfg.Name),
	)

	// Register with the wallet actor to receive BoardingUtxoConfirmedEvent
	// notifications. The wallet handles all boarding address monitoring and
	// will notify us when new UTXOs are confirmed.
	mappedRef := actor.NewMapInputRef(
		a.cfg.SelfRef,
		func(
			evt wallet.BoardingUtxoConfirmedEvent,
		) actormsg.RoundReceivable {

			return &WalletBoardingConfirmed{
				Intent: evt.BoardingIntent,
			}
		},
	)

	// Request all historical confirmations. The wallet will send backlog
	// events for any confirmed intents.
	regReq := &wallet.RegisterConfirmationNotifierRequest{
		NotifierID:    fmt.Sprintf("round-actor-%s", a.cfg.Name),
		NotifyActor:   mappedRef,
		BacklogHeight: fn.None[int32](),
		MinConf:       fn.Some(a.cfg.OperatorTerms.MinConfirmations),
	}

	future := a.cfg.WalletActor.Ask(ctx, regReq)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("register with wallet: %w", result.Err())
	}

	a.log.InfoS(
		ctx,
		"Registered with wallet actor for boarding confirmations",
		slog.Int(
			"min_confirmations",
			int(a.cfg.OperatorTerms.MinConfirmations),
		),
	)

	// Load active rounds (commitment tx broadcast, not yet confirmed) and
	// resume their FSMs. These rounds have server-assigned RoundIDs from
	// the checkpoint.
	activeRounds, err := a.cfg.RoundStore.ListActiveRounds(ctx)
	if err != nil {
		return fmt.Errorf("failed to load active rounds: %w", err)
	}

	a.log.InfoS(ctx, "Loaded active rounds from database",
		slog.Int("count", len(activeRounds)),
	)

	for _, round := range activeRounds {
		roundFSM, err := a.createRoundFSMFromDB(ctx, round.RoundID)
		if err != nil {
			return fmt.Errorf("failed to create FSM for round "+
				"%s: %w", round.RoundID, err)
		}

		// Use the RoundID as the key (already server-assigned at
		// checkpoint).
		keyStr := RoundKeyStr(round.RoundID.KeyString())
		a.rounds[keyStr] = roundFSM

		// Register for confirmation of the commitment tx for this
		// round.
		if !roundFSM.TxID.IsEqual(&chainhash.Hash{}) {
			a.commitmentTxIndex[roundFSM.TxID] = keyStr
			a.registerCommitmentConfirmation(
				ctx, roundFSM.TxID, round.CommitmentTx,
				round.VTXOTreePaths.UnwrapOr(nil),
			)

			a.log.InfoS(ctx, "Resumed round awaiting confirmation",
				slog.String("round_id", round.RoundID.String()),
				slog.String(
					"commitment_txid",
					roundFSM.TxID.String(),
				),
			)
		}

		if err := a.replayCheckpointedServerMessages(
			ctx, roundFSM,
		); err != nil {
			return fmt.Errorf("replay checkpointed messages for "+
				"round %s: %w", round.RoundID, err)
		}

		// A reloaded round sits back in InputSigSentState with its
		// forfeit signatures already out, so the wavelength#844
		// hazard window reopens across the restart. Re-arm the
		// status-reconcile timeout for forfeit-bearing rounds so a
		// round whose failure raced the crash still converges on a
		// QueryRoundStatus probe rather than stranding.
		if len(round.Intents.Forfeits) > 0 &&
			a.env.StatusReconcileTimeout > 0 {

			if err := a.processOutbox(ctx, []ClientOutMsg{
				&StartTimeoutReq{
					RoundKey: RoundKeyStr(
						round.RoundID.KeyString(),
					),
					Phase:    TimeoutPhaseStatusReconcile,
					Duration: a.env.StatusReconcileTimeout,
				},
			}); err != nil {
				return fmt.Errorf("arm status reconcile "+
					"timeout for reloaded round %s: %w",
					round.RoundID, err)
			}
		}
	}

	a.log.InfoS(ctx, "Round client actor started")

	return nil
}

// Receive processes an actor message and returns a response. This is the main
// entry point for the actor. The method uses actormsg types (RoundReceivable
// and RoundActorResp) so that the wallet can look up the round actor via
// service key without import cycles.
func (a *RoundClientActor) Receive(ctx context.Context,
	msg actormsg.RoundReceivable) fn.Result[actormsg.RoundActorResp] {

	switch m := msg.(type) {
	case *WalletBoardingConfirmed:
		return a.handleWalletBoardingConfirmed(ctx, m)

	case *RegisterVTXORequestsRequest:
		return a.handleVTXORequests(ctx, m)

	case *VTXORequestsReceived:
		return a.handleVTXORequestsReceived(ctx, m)

	case *ServerMessageNotification:
		return a.handleServerMessage(ctx, m)

	case *GetClientStateRequest:
		return a.handleGetState(ctx, m)

	case *CancelRoundRequest:
		return a.handleCancelRound(ctx, m)

	case *ConfirmationEvent:
		return a.handleConfirmation(ctx, m)

	case *TimeoutMsg:
		return a.handleTimeout(ctx, m)

	case *RegisterIntentRequest:
		return a.handleRegisterIntent(ctx, m)

	// NOTE: RegisterIntentMsg mirrors the fields of IntentPackage.
	// If either type gains new fields, this adapter must be updated
	// to carry them through.
	case *actormsg.RegisterIntentMsg:
		return a.handleRegisterIntent(ctx, &RegisterIntentRequest{
			Package: &IntentPackage{Intents: Intents{
				Forfeits: m.Forfeits,
				VTXOs:    m.VTXOs,
				Leaves:   m.Leaves,
			}},
			TriggerRegistration: m.TriggerRegistration,
		})

	case *RefreshVTXORequest:
		return a.handleRefreshVTXORequest(ctx, m)

	case *ForfeitSignatureResponse:
		return a.handleForfeitSignatureResponse(ctx, m)

	case *actormsg.TriggerBoardMsg:
		return a.handleTriggerBoard(ctx, m)

	default:
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("unknown message type: %T", msg),
		)
	}
}

// handleWalletBoardingConfirmed processes a boarding UTXO confirmation event
// from the wallet actor. This creates the FSM event and drives the state
// machine forward. The wallet handles all persistence; we just react.
func (a *RoundClientActor) handleWalletBoardingConfirmed(ctx context.Context,
	msg *WalletBoardingConfirmed) fn.Result[actormsg.RoundActorResp] {

	walletIntent := msg.Intent
	if walletIntent == nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("wallet boarding confirmed event missing " +
				"intent"),
		)
	}

	a.log.InfoS(ctx, "Received boarding UTXO confirmation from wallet",
		btclog.Fmt("outpoint", "%v", walletIntent.Outpoint),
		slog.Int("amount", int(walletIntent.ChainInfo.Amount)),
		slog.Int("conf_height", int(walletIntent.ChainInfo.ConfHeight)),
	)

	// Validate chain data that the FSM previously checked.
	confTx := walletIntent.ChainInfo.ConfTx
	if confTx == nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("boarding confirmation missing tx"),
		)
	}
	if int(walletIntent.Outpoint.Index) >= len(confTx.TxOut) {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("invalid outpoint index %d for tx %s",
				walletIntent.Outpoint.Index,
				walletIntent.Outpoint.Hash),
		)
	}

	intent, err := buildBoardingIntentFromWallet(walletIntent)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("build boarding intent from wallet: %w",
				err),
		)
	}

	// Find an existing assembling round (Idle or PendingRoundAssembly) or
	// create a new one. This allows multiple boarding confirmations to
	// accumulate in the same round.
	roundFSM := a.findAssemblingRound()
	if roundFSM == nil {
		roundFSM, err = a.createNewRound(ctx)
		if err != nil {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("failed to create round for "+
					"boarding: %w", err),
			)
		}
	}

	// Send the boarding intent to the FSM as an IntentPackage.
	pkg := &IntentPackage{Intents: Intents{
		Boarding: []BoardingIntent{
			intent,
		},
	}}
	err = a.askEventAndProcessOutbox(ctx, roundFSM, pkg)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("FSM error processing boarding "+
				"confirmation: %w", err),
		)
	}

	return fn.Ok[actormsg.RoundActorResp](nil)
}

// buildBoardingIntentFromWallet converts the wallet's persisted boarding
// representation into the round actor's intent shape, validating the chain
// data needed for round registration and join-auth.
func buildBoardingIntentFromWallet(walletIntent *wallet.BoardingIntent) (
	BoardingIntent, error) {

	if walletIntent == nil {
		return BoardingIntent{}, fmt.Errorf("wallet intent is nil")
	}

	confTx := walletIntent.ChainInfo.ConfTx
	if confTx == nil {
		return BoardingIntent{}, fmt.Errorf("boarding confirmation " +
			"missing tx")
	}
	if int(walletIntent.Outpoint.Index) >= len(confTx.TxOut) {
		return BoardingIntent{}, fmt.Errorf("invalid outpoint index "+
			"%d for tx %s", walletIntent.Outpoint.Index,
			walletIntent.Outpoint.Hash)
	}

	// Chain-level information (ConfHeight, ConfHash, ConfTx, TxProof,
	// Amount) is carried through the embedded wallet.BoardingIntent and
	// remains available to downstream consumers such as join-auth.
	boardingReq := types.BoardingRequest{
		Outpoint: &walletIntent.Outpoint,
		TxProof:  walletIntent.ChainInfo.TxProof,
	}
	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		walletIntent.Address.KeyDesc.PubKey,
		walletIntent.Address.OperatorKey,
		walletIntent.Address.ExitDelay,
	)
	if err != nil {
		return BoardingIntent{}, fmt.Errorf("encode boarding policy "+
			"template: %w", err)
	}
	boardingReq.PolicyTemplate = policyTemplate

	return BoardingIntent{
		BoardingIntent: *walletIntent,
		Request:        boardingReq,
	}, nil
}

// handleVTXORequests processes client-submitted VTXO requests and forwards
// them to an idle round FSM. If no idle round exists, a new one is created.
func (a *RoundClientActor) handleVTXORequests(ctx context.Context,
	msg *RegisterVTXORequestsRequest) fn.Result[actormsg.RoundActorResp] {

	if len(msg.Amounts) == 0 {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("VTXO request amounts are empty"),
		)
	}

	requests := make([]types.VTXORequest, 0, len(msg.Amounts))
	for i, amount := range msg.Amounts {
		if amount <= 0 {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("VTXO amount %d is invalid: %v", i,
					amount),
			)
		}

		// This legacy "bare amounts" path carries no origin
		// context, so leave the Origin Unknown. The round
		// actor treats Unknown as "do not emit a ledger
		// event" so we never misclassify. Production paths
		// use handleTriggerBoard / handleRegisterIntent which
		// tag origin explicitly.
		req, err := a.buildVTXORequest(
			ctx, amount, types.VTXOOriginUnknown,
		)
		if err != nil {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("build VTXO request %d: %w", i, err),
			)
		}

		// The legacy bare-amounts path is used by tests that
		// feed in N amounts and expect N VTXO outputs. Under
		// the #270 admission rule the server requires exactly
		// one IsChange=true marker across a multi-output
		// intent; the FSM's IntentRequested handler stamps
		// that marker centrally via designateChangeMarker over
		// the fully-composed intent, so this loop leaves
		// IsChange unset.

		requests = append(requests, *req)
	}

	a.log.InfoS(ctx, "Received VTXO requests",
		slog.Int("count", len(requests)),
	)

	var err error

	// Find an existing assembling round (Idle or PendingRoundAssembly) or
	// create a new one. This allows VTXOs to join a round that already has
	// boarding intents being assembled.
	roundFSM := a.findAssemblingRound()
	if roundFSM == nil {
		roundFSM, err = a.createNewRound(ctx)
		if err != nil {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("create new round for VTXO "+
					"requests: %w", err),
			)
		}
	}

	pkg := &IntentPackage{Intents: Intents{
		VTXOs: requests,
	}}

	err = a.askEventAndProcessOutbox(ctx, roundFSM, pkg)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("FSM error processing VTXO requests: %w",
				err),
		)
	}

	return fn.Ok[actormsg.RoundActorResp](&RegisterVTXORequestsResponse{
		Success: true,
	})
}

// handleVTXORequestsReceived forwards pre-built VTXO requests from other
// actors into the pending round FSM via IntentPackage.
func (a *RoundClientActor) handleVTXORequestsReceived(ctx context.Context,
	req *VTXORequestsReceived) fn.Result[actormsg.RoundActorResp] {

	if len(req.Requests) == 0 {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("VTXO requests are empty"),
		)
	}

	a.log.InfoS(ctx, "Received VTXO requests",
		slog.Int("count", len(req.Requests)),
	)

	// Find an existing assembling round (Idle or PendingRoundAssembly) or
	// create a new one. This allows VTXOs to join a round that already has
	// boarding intents being assembled.
	roundFSM := a.findAssemblingRound()
	if roundFSM == nil {
		var err error
		roundFSM, err = a.createNewRound(ctx)
		if err != nil {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("create new round for VTXO "+
					"requests: %w", err),
			)
		}
	}

	pkg := &IntentPackage{Intents: Intents{
		VTXOs: req.Requests,
	}}
	err := a.askEventAndProcessOutbox(ctx, roundFSM, pkg)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("FSM error processing VTXO requests: %w",
				err),
		)
	}

	return fn.Ok[actormsg.RoundActorResp](nil)
}

// buildVTXORequest derives a fresh owner key and constructs a locally owned
// VTXO request for the provided amount. The round FSM derives the ephemeral
// signing key later during registration. Origin is set by the caller so
// the downstream ledger emission gets the right Source: boarding flows
// pass VTXOOriginRoundBoarding, other in-round producers pass their
// respective origin.
func (a *RoundClientActor) buildVTXORequest(ctx context.Context,
	amount btcutil.Amount, origin types.VTXOOrigin) (*types.VTXORequest,
	error) {

	keyDesc, err := a.cfg.Wallet.DeriveNextKey(
		ctx, types.VTXOOwnerKeyFamily,
	)
	if err != nil {
		return nil, fmt.Errorf("derive owner key: %w", err)
	}

	operatorKey := a.cfg.OperatorTerms.PubKey
	expiry := a.cfg.OperatorTerms.VTXOExitDelay
	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		keyDesc.PubKey, operatorKey, expiry,
	)
	if err != nil {
		return nil, fmt.Errorf("encode vtxo policy template: %w", err)
	}

	req := &types.VTXORequest{
		PolicyTemplate: policyTemplate,
		Amount:         amount,
		ClientKey:      keyDesc.PubKey,
		OwnerKey:       *keyDesc,
		Origin:         origin,
	}

	if a.cfg.OwnedScriptRegistrar != nil {
		pkScript, err := req.EffectivePkScript()
		if err != nil {
			return nil, fmt.Errorf("derive vtxo pkScript: %w", err)
		}

		regErr := a.cfg.OwnedScriptRegistrar.RegisterOwnedScript(
			ctx, pkScript, *keyDesc,
		)
		if regErr != nil {
			return nil, fmt.Errorf("register owned script: %w",
				regErr)
		}
	}

	return req, nil
}

// buildCustomBoardVTXORequest constructs a boarded VTXO request from a
// caller-supplied arkscript policy template. Unlike buildVTXORequest it derives
// no owner key and registers no owned script: the policy's owner is external to
// this daemon (for example an aggregate FROST key the client controls off-box),
// so ClientKey/OwnerKey are intentionally left zero and the FSM still assigns
// the ephemeral MuSig2 tree-signing key at registration time. This mirrors the
// custom-refresh output construction so a custom-policy board and a
// custom-policy refresh produce structurally identical VTXO requests. The
// template and pkScript are validated at the RPC boundary before the board
// intent is admitted.
func buildCustomBoardVTXORequest(amount btcutil.Amount, policyTemplate,
	pkScript []byte, origin types.VTXOOrigin) *types.VTXORequest {

	return &types.VTXORequest{
		PolicyTemplate: append([]byte(nil), policyTemplate...),
		PkScript:       append([]byte(nil), pkScript...),
		Amount:         amount,
		Origin:         origin,
	}
}

// handleRoundJoined handles the RoundJoined event which requires special
// re-keying logic. It matches the accepted outpoints to find the correct
// pending round, then re-keys the round from its TempRoundKey to the
// server-assigned RoundID.
func (a *RoundClientActor) handleRoundJoined(ctx context.Context,
	event *RoundJoined) fn.Result[actormsg.RoundActorResp] {

	// Find the pending round by matching outpoints. Currently we only match
	// boarding outpoints, but this will be extended for VTXO operations
	// (forfeit, leave, refresh) when implemented.
	roundFSM := a.findRoundByOutpoints(
		event.AcceptedBoardingOutpoints, event.AcceptedVTXOOutpoints,
	)
	if roundFSM == nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("no pending round matches: "+
				"boarding=%v, vtxo=%v",
				event.AcceptedBoardingOutpoints,
				event.AcceptedVTXOOutpoints),
		)
	}

	// Re-key: Remove old temp key, add with new RoundID.
	oldKeyStr := RoundKeyStr(roundFSM.Key.KeyString())
	delete(a.rounds, oldKeyStr)

	newKeyStr := RoundKeyStr(event.RoundID.KeyString())
	roundFSM.Key = event.RoundID
	roundFSM.RoundID = event.RoundID
	a.rounds[newKeyStr] = roundFSM

	a.log.InfoS(ctx, "Re-keyed round from temp to assigned",
		slog.String("old_key", string(oldKeyStr)),
		slog.String("round_id", event.RoundID.String()),
		slog.Int("num_boarding", len(event.AcceptedBoardingOutpoints)),
		slog.Int("num_vtxo", len(event.AcceptedVTXOOutpoints)),
	)

	// Now process the event normally.
	err := a.askEventAndProcessOutbox(ctx, roundFSM, event)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("FSM error processing RoundJoined: %w", err),
		)
	}

	// If a JoinRoundQuoteReceived for this round arrived before the
	// re-key, deliver it now. The mailbox contract allows
	// out-of-order delivery; draining here prevents the buffered
	// quote from stalling the FSM indefinitely.
	if pending, ok := a.pendingQuotes[event.RoundID]; ok {
		delete(a.pendingQuotes, event.RoundID)

		a.log.InfoS(ctx, "Delivering buffered quote after re-key",
			slog.String("round_id", event.RoundID.String()),
		)

		if drainErr := a.askEventAndProcessOutbox(
			ctx, roundFSM, pending,
		); drainErr != nil {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("FSM error draining buffered "+
					"quote: %w", drainErr),
			)
		}
	}

	return fn.Ok[actormsg.RoundActorResp](
		&ServerMessageResponse{
			Success: true,
		},
	)
}

// maxPendingQuotes caps the number of out-of-order quotes the
// actor is willing to buffer. Under normal operation the buffer
// holds at most one entry per concurrent round; the cap exists
// solely to prevent a hostile server from flooding memory with
// quote envelopes for round ids the client never admitted.
const maxPendingQuotes = 32

// bufferPendingQuote stashes a JoinRoundQuoteReceived whose
// RoundID does not yet correspond to any tracked FSM. The matching
// handleRoundJoined call later drains the buffer, so the FSM
// sees the quote exactly as if delivery had been strictly in
// order. Returns a success response so the mailbox does not retry
// redelivery in a tight loop; if the corresponding RoundJoined
// never arrives (the server abandoned the round), the buffered
// quote simply expires with the actor.
func (a *RoundClientActor) bufferPendingQuote(ctx context.Context,
	quote *JoinRoundQuoteReceived) fn.Result[actormsg.RoundActorResp] {

	if len(a.pendingQuotes) >= maxPendingQuotes {
		a.log.WarnS(
			ctx,
			"Dropping buffered quote: pending-quote buffer full",
			nil,
			slog.String("round_id", quote.RoundID.String()),
			slog.Int("buffered", len(a.pendingQuotes)),
		)

		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("pending-quote buffer full"),
		)
	}

	a.log.InfoS(ctx, "Buffering quote for round not yet admitted locally",
		slog.String("round_id", quote.RoundID.String()),
	)

	a.pendingQuotes[quote.RoundID] = quote

	return fn.Ok[actormsg.RoundActorResp](&ServerMessageResponse{
		Success: true,
	})
}

// extractRoundID returns the RoundID from events that carry one. Returns the
// zero value for events without a RoundID field.
func extractRoundID(event ClientEvent) (RoundID, bool) {
	switch e := event.(type) {
	case *RoundJoined:
		return e.RoundID, true

	case *JoinRoundQuoteReceived:
		return e.RoundID, true

	case *CommitmentTxBuilt:
		return e.RoundID, true

	case *NoncesAggregated:
		return e.RoundID, true

	case *OperatorSigned:
		return e.RoundID, true

	case *AwaitingBoardingSigs:
		return e.RoundID, true

	case *RoundStatusReported:
		return e.RoundID, true

	default:
		return RoundID{}, false
	}
}

// handleServerMessage processes a message from the server (delivered via
// Outbox). The actor routes the message to the appropriate FSM based on the
// event type and RoundID.
func (a *RoundClientActor) handleServerMessage(ctx context.Context,
	msg *ServerMessageNotification) fn.Result[actormsg.RoundActorResp] {

	// RoundJoined requires special handling for re-keying.
	if joined, ok := msg.Message.(*RoundJoined); ok {
		return a.handleRoundJoined(ctx, joined)
	}

	// Try to route by RoundID first.
	roundID, hasRoundID := extractRoundID(msg.Message)

	var (
		roundFSM *RoundFSM
		routeRes fn.Result[actormsg.RoundActorResp]
		routed   bool
	)
	if hasRoundID {
		roundFSM, routeRes, routed = a.routeServerMessageByRoundID(
			ctx, roundID, msg.Message,
		)
	} else {
		roundFSM, routeRes, routed = a.routeServerMessageToPending(
			ctx, msg.Message,
		)
	}
	if routed {
		return routeRes
	}

	err := a.askEventAndProcessOutbox(ctx, roundFSM, msg.Message)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("FSM error processing server message: %w",
				err),
		)
	}

	return fn.Ok[actormsg.RoundActorResp](&ServerMessageResponse{
		Success: true,
	})
}

// routeServerMessageByRoundID looks up the FSM for a RoundID-keyed
// server message. Returns (fsm, _, false) when the FSM was found and
// the caller should drive the event into it. Returns
// (_, response, true) when the routing decision is terminal — either
// because the message was buffered as an out-of-order quote pending a
// future RoundJoined, or because no FSM exists and the miss is fatal.
func (a *RoundClientActor) routeServerMessageByRoundID(ctx context.Context,
	roundID RoundID, msg ClientEvent) (*RoundFSM,
	fn.Result[actormsg.RoundActorResp], bool) {

	keyStr := RoundKeyStr(roundID.KeyString())
	roundFSM, exists := a.rounds[keyStr]
	if exists {
		a.log.DebugS(ctx, "Routing server message by RoundID",
			slog.String("event_type", fmt.Sprintf("%T", msg)),
			slog.String("round_id", roundID.String()),
		)

		return roundFSM, fn.Result[actormsg.RoundActorResp]{}, false
	}

	// The mailbox contract allows envelopes to arrive out of order. If
	// a JoinRoundQuoteReceived lands before its matching RoundJoined
	// has re-keyed the FSM, buffer it so handleRoundJoined can drain
	// it after re-keying. Every other event type without a live
	// routing target is a real miss.
	if quote, ok := msg.(*JoinRoundQuoteReceived); ok {
		return nil, a.bufferPendingQuote(ctx, quote), true
	}

	return nil, fn.Err[actormsg.RoundActorResp](
		fmt.Errorf("no round for ID: %s", roundID),
	), true
}

// ErrNoPendingRound is returned when a server message — most notably an
// IntentRequested join trigger — arrives with no pending round to route
// it to. It is deliberately typed rather than a bare fmt.Errorf so
// callers can tell this benign "nothing to join" shape (an auto-join
// after a refresh or leave that queued nothing) apart from a genuine
// internal fault, and report it as a no-op instead of an error.
var ErrNoPendingRound = errors.New("no pending round")

// routeServerMessageToPending dispatches a non-RoundID-keyed server
// message to a pending (temp-keyed) round. Returns (fsm, _, false)
// when an FSM was found; otherwise returns (_, errResult, true) so
// the caller short-circuits with a routing failure.
func (a *RoundClientActor) routeServerMessageToPending(ctx context.Context,
	msg ClientEvent) (*RoundFSM, fn.Result[actormsg.RoundActorResp], bool) {

	roundFSM := a.findPendingRound()

	// Round failures can arrive after the round has been re-keyed by a
	// server-assigned RoundID, so findPendingRound (which only matches
	// temp-keyed rounds) misses them. Route them deterministically by the
	// RoundID the failure carries, falling back to the sole-round heuristic
	// for failures that predate round assignment (e.g. ClientErrorResp).
	if roundFSM == nil {
		bf, isBoardingFailed := msg.(*BoardingFailed)

		// Prefer the deterministic lookup by the server-assigned
		// RoundID. This works regardless of how many rounds (including
		// lingering terminal ones) are tracked.
		if isBoardingFailed {
			bf.RoundID.WhenSome(func(rid RoundID) {
				keyStr := RoundKeyStr(rid.KeyString())
				if candidate, ok := a.rounds[keyStr]; ok {
					roundFSM = candidate

					a.log.DebugS(ctx,
						"Routing BoardingFailed by "+
							"RoundID",
						slog.String(
							"key", candidate.Key.
								KeyString(),
						))
				}
			})
		}

		// Fall back to the sole-round heuristic only for failures that
		// carry no RoundID (pre-assignment failures, e.g.
		// ClientErrorResp) when exactly one round is tracked. A failure
		// that carries a RoundID which matched nothing above is a
		// genuine miss (e.g. the round was already reaped); routing it
		// to an unrelated sole round would fail the wrong round, so we
		// let it miss instead.
		if roundFSM == nil && isBoardingFailed && bf.RoundID.IsNone() &&
			len(a.rounds) == 1 {

			for _, candidate := range a.rounds {
				roundFSM = candidate
			}

			if roundFSM != nil {
				a.log.DebugS(ctx,
					"Routing BoardingFailed to sole "+
						"tracked round",
					slog.String(
						"key", roundFSM.Key.KeyString(),
					))
			}
		}
	}

	if roundFSM == nil {
		return nil, fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("no pending round for event %T: %w", msg,
				ErrNoPendingRound),
		), true
	}

	a.log.DebugS(ctx, "Routing server message to pending round",
		slog.String("event_type", fmt.Sprintf("%T", msg)),
		slog.String("key", roundFSM.Key.KeyString()),
	)

	return roundFSM, fn.Result[actormsg.RoundActorResp]{}, false
}

// findPendingRound returns a round with a temp key (not yet assigned a RoundID
// by the server). Returns nil if no pending rounds exist.
func (a *RoundClientActor) findPendingRound() *RoundFSM {
	for _, roundFSM := range a.rounds {
		if roundFSM.Key.IsTemp() {
			return roundFSM
		}
	}

	return nil
}

// handleGetState returns the current FSM state for monitoring/debugging.
// This includes all round FSMs (both temp-keyed and RoundID-keyed).
func (a *RoundClientActor) handleGetState(ctx context.Context,
	_ *GetClientStateRequest) fn.Result[actormsg.RoundActorResp] {

	states := make(map[string]FSMStateInfo)

	for keyStr, roundFSM := range a.rounds {
		roundState, err := roundFSM.FSM.CurrentState()
		if err != nil {
			a.log.WarnS(ctx, "Failed to get FSM state for round",
				err,
				slog.String("key", string(keyStr)),
			)

			continue
		}

		clientState, ok := roundState.(ClientState)
		if !ok {
			a.log.WarnS(ctx, "Round FSM state is not a ClientState",
				nil,
				slog.String("key", string(keyStr)),
				slog.String(
					"state_type",
					fmt.Sprintf("%T", roundState),
				),
			)

			continue
		}

		states[string(keyStr)] = FSMStateInfo{
			State:   clientState,
			IsTemp:  roundFSM.Key.IsTemp(),
			RoundID: roundFSM.RoundID,
		}
	}

	return fn.Ok[actormsg.RoundActorResp](&GetClientStateResponse{
		States: states,
	})
}

// handleCancelRound attempts to cancel a pending round participation.
// If a RoundKey is specified in the request, that round is cancelled;
// otherwise, the first temp-keyed round is cancelled.
func (a *RoundClientActor) handleCancelRound(ctx context.Context,
	req *CancelRoundRequest) fn.Result[actormsg.RoundActorResp] {

	a.log.InfoS(ctx, "Cancelling round participation by user request")

	// Find the round to cancel.
	var targetFSM *RoundFSM
	if req.RoundKey.IsSome() {
		// Cancel specific round by key.
		keyStr := req.RoundKey.UnsafeFromSome()
		var exists bool
		targetFSM, exists = a.rounds[keyStr]
		if !exists {
			return fn.Ok[actormsg.RoundActorResp](
				&CancelRoundResponse{
					Success: false,
					Error: fmt.Sprintf(
						"no round with key: %s",
						keyStr,
					),
				},
			)
		}
	} else {
		// Cancel the first temp-keyed round.
		for _, roundFSM := range a.rounds {
			if roundFSM.Key.IsTemp() {
				targetFSM = roundFSM
				break
			}
		}
	}

	if targetFSM == nil {
		return fn.Ok[actormsg.RoundActorResp](&CancelRoundResponse{
			Success: false,
			Error:   "no pending round to cancel",
		})
	}

	// Inject a BoardingFailed event to transition the FSM to failed state.
	// This will trigger any cleanup logic in the FSM transitions.
	cancelEvent := &BoardingFailed{
		Reason:      "User requested cancellation",
		Error:       fmt.Errorf("round cancelled by user"),
		Recoverable: true,
	}

	err := a.askEventAndProcessOutbox(ctx, targetFSM, cancelEvent)
	if err != nil {
		a.log.WarnS(ctx, "Failed to cancel round", err)

		return fn.Ok[actormsg.RoundActorResp](&CancelRoundResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to cancel: %v", err),
		})
	}

	// Remove the cancelled round. Failed rounds are now reaped lazily at
	// the next assembly (reapFailedRounds), not on entry, so an explicit
	// cancel must still stop and drop the round here. Guard on presence so
	// we don't double-stop an FSM a concurrent path already cleaned up.
	keyStr := RoundKeyStr(targetFSM.Key.KeyString())
	if _, exists := a.rounds[keyStr]; exists {
		targetFSM.FSM.Stop()
		delete(a.rounds, keyStr)
	}

	a.log.InfoS(ctx, "Round participation cancelled successfully")

	return fn.Ok[actormsg.RoundActorResp](&CancelRoundResponse{
		Success: true,
	})
}

// onRoundComplete is called when a round finishes successfully. This removes
// the round from active tracking and archives the round data.
func (a *RoundClientActor) onRoundComplete(ctx context.Context, roundID RoundID,
	txid chainhash.Hash, confInfo ConfInfo) error {

	a.log.InfoS(ctx, "Round completed successfully",
		slog.String("round_id", roundID.String()),
		slog.String("commitment_txid", txid.String()),
		slog.Int("conf_height", int(confInfo.Height)),
	)

	keyStr := RoundKeyStr(roundID.KeyString())
	if roundFSM, exists := a.rounds[keyStr]; exists {
		roundFSM.FSM.Stop()
		delete(a.rounds, keyStr)
	}
	delete(a.commitmentTxIndex, txid)

	return a.cfg.RoundStore.FinalizeRound(ctx, roundID, txid, confInfo)
}

// reapFailedRounds drops every round FSM that has settled in the terminal
// ClientFailedState from active tracking. Rounds that fail or time out
// (registration/admission timeout, server rejection, quote rejection,
// forfeit-collection timeout, etc.) would otherwise linger in the rounds map
// for the lifetime of the daemon — and keep surfacing in ListRounds — because
// only successful completion (onRoundComplete) and explicit cancellation
// (handleCancelRound) removed rounds. Nothing reuses a failed round in
// production: findAssemblingRound only returns Idle / PendingRoundAssembly
// rounds, and the FSM's IntentPackage / RecoveryInitiated recovery transitions
// have no production producer.
//
// Reaping is deliberately deferred to the next round assembly (createNewRound)
// rather than fired the instant a round enters ClientFailedState. A failure is
// only useful if a consumer can observe it: GetClientState (and the RPC
// ListRounds surface it backs) must be able to report a round as FAILED at
// least until the client moves on to a fresh round. Reaping on entry made the
// terminal state vanish within the same actor turn, so a poller could never
// see it (wavelength#602 systests). Sweeping at the start of the next
// assembly keeps the window open while still bounding accumulation to the
// failures since the last new round.
//
// Only the *settled* terminal ClientFailedState is reaped.
// RecoveryInitiatedState is deliberately NOT reaped: it is semi-terminal
// (states.go) and represents a round whose CSV-timeout sweep tx has been
// broadcast and is awaiting confirmation — in-flight work, not a settled
// failure.
func (a *RoundClientActor) reapFailedRounds(ctx context.Context) {
	for keyStr, roundFSM := range a.rounds {
		state, err := roundFSM.FSM.CurrentState()
		if err != nil {
			continue
		}

		if _, failed := state.(*ClientFailedState); !failed {
			continue
		}

		a.log.InfoS(ctx, "Reaping failed round",
			slog.String("round_key", string(keyStr)),
		)

		roundFSM.FSM.Stop()
		delete(a.rounds, keyStr)
		delete(a.commitmentTxIndex, roundFSM.TxID)
	}
}

// handleConfirmation processes a commitment transaction confirmation event
// from ChainSource. Boarding address confirmations are now handled via
// WalletBoardingConfirmed events from the wallet actor.
//
// Concurrency: The actor framework serializes all messages through Receive(),
// so no synchronization is needed for rounds map access.
func (a *RoundClientActor) handleConfirmation(ctx context.Context,
	event *ConfirmationEvent) fn.Result[actormsg.RoundActorResp] {

	a.log.InfoS(ctx, "Received commitment transaction confirmation",
		slog.String("txid", event.Txid.String()),
		slog.Int("block_height", int(event.BlockHeight)),
		slog.Int("confirmations", int(event.Confirmations)),
	)

	// Look up the round by commitment transaction index.
	keyStr, exists := a.commitmentTxIndex[event.Txid]
	if !exists {
		// Not a commitment tx we're tracking. This shouldn't happen
		// since we only register for commitment tx confirmations.
		// Log for observability.
		a.log.WarnS(ctx, "Commitment tx not in index",
			nil,
			slog.String("txid", event.Txid.String()),
		)

		return fn.Ok[actormsg.RoundActorResp](nil)
	}

	// Route to the specific round's FSM.
	roundFSM, exists := a.rounds[keyStr]
	if !exists {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("round FSM not found for key %s", keyStr),
		)
	}

	a.log.InfoS(ctx, "Routing confirmation to round FSM",
		slog.String("key", string(keyStr)),
		slog.String("round_id", roundFSM.RoundID.String()),
	)

	confirmEvt := &BoardingConfirmed{
		TxID:          event.Txid,
		BlockHeight:   event.BlockHeight,
		BlockHash:     event.BlockHash,
		Confirmations: int32(event.Confirmations),
	}

	err := a.askEventAndProcessOutbox(ctx, roundFSM, confirmEvt)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("FSM error processing commitment "+
				"confirmation: %w", err),
		)
	}

	return fn.Ok[actormsg.RoundActorResp](nil)
}

// handleTimeout parses a composite timeout ID and forwards the corresponding
// timeout event into the target round FSM.
func (a *RoundClientActor) handleTimeout(ctx context.Context,
	msg *TimeoutMsg) fn.Result[actormsg.RoundActorResp] {

	keyStr, phase, err := parseTimeoutID(msg.TimeoutID)
	if err != nil {
		a.log.WarnS(ctx, "Failed to parse timeout ID",
			err,
			slog.String("timeout_id", string(msg.TimeoutID)),
		)

		return fn.Ok[actormsg.RoundActorResp](nil)
	}

	roundFSM, exists := a.rounds[keyStr]
	if !exists {
		a.log.DebugS(ctx, "Ignoring timeout for unknown round",
			slog.String("round_key", string(keyStr)),
			slog.String("phase", string(phase)),
		)

		return fn.Ok[actormsg.RoundActorResp](nil)
	}

	var timeoutEvt ClientEvent
	switch phase {
	case TimeoutPhaseRefreshRegistration:
		state, stateErr := roundFSM.FSM.CurrentState()
		if stateErr != nil {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("read round state for automatic "+
					"refresh registration: %w", stateErr),
			)
		}

		if _, ok := state.(*PendingRoundAssembly); !ok {
			a.log.DebugS(ctx, "Ignoring stale refresh registration "+
				"timeout",
				slog.String("round_key", string(keyStr)),
				slog.String("state", fmt.Sprintf("%T", state)),
			)

			return fn.Ok[actormsg.RoundActorResp](nil)
		}

		err = a.askEventAndProcessOutbox(
			ctx, roundFSM, &IntentRequested{},
		)
		if err != nil {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("trigger automatic refresh "+
					"registration: %w", err),
			)
		}

		return fn.Ok[actormsg.RoundActorResp](nil)

	case TimeoutPhaseForfeitCollection:
		// Forfeit collection only runs after the round has been
		// re-keyed to its server-assigned RoundID, so the map key
		// parses back to a RoundID.
		roundID, perr := ParseRoundID(string(keyStr))
		if perr != nil {
			a.log.WarnS(ctx, "Forfeit timeout with non-RoundID key",
				perr,
				slog.String("round_key", string(keyStr)),
			)

			return fn.Ok[actormsg.RoundActorResp](nil)
		}
		timeoutEvt = &ForfeitCollectionTimedOut{
			RoundID: roundID,
		}

	case TimeoutPhaseRegistration:
		timeoutEvt = &RegistrationTimedOut{}

	case TimeoutPhaseStatusReconcile:
		// The reconcile timeout is only armed after the round has been
		// re-keyed to its server-assigned RoundID (the forfeit
		// signatures cannot leave the box before admission), so the
		// map key parses back to a RoundID.
		roundID, perr := ParseRoundID(string(keyStr))
		if perr != nil {
			a.log.WarnS(ctx, "Status reconcile timeout with "+
				"non-RoundID key",
				perr,
				slog.String("round_key", string(keyStr)),
			)

			return fn.Ok[actormsg.RoundActorResp](nil)
		}
		timeoutEvt = &StatusReconcileTimedOut{
			RoundID: roundID,
		}

	default:
		a.log.WarnS(ctx, "Ignoring timeout with unknown phase",
			nil,
			slog.String("round_key", string(keyStr)),
			slog.String("phase", string(phase)),
		)

		return fn.Ok[actormsg.RoundActorResp](nil)
	}

	err = a.askEventAndProcessOutbox(ctx, roundFSM, timeoutEvt)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("FSM error processing timeout for phase "+
				"%s: %w", phase, err),
		)
	}

	return fn.Ok[actormsg.RoundActorResp](nil)
}

// processOutbox processes messages emitted by the FSM via Outbox and routes
// them to the appropriate destination (server or chainsource).
//
//nolint:funlen
func (a *RoundClientActor) processOutbox(ctx context.Context,
	outbox []ClientOutMsg) error {

	for _, msg := range outbox {
		// Check if this message should be sent to the server. All
		// server-bound messages implement the ServerMessage interface.
		if serverMsg, ok := msg.(serverconn.ServerMessage); ok {
			sm := serverMsg.ServiceMethod()
			sendReq := &serverconn.SendClientEventRequest{
				Message: serverMsg,
				Service: sm.Service,
				Method:  sm.Method,
			}

			if err := a.cfg.ServerConn.Tell(
				ctx, sendReq,
			); err != nil {
				return fmt.Errorf("send to server: %w", err)
			}

			continue
		}

		// Handle non-server messages.
		switch m := msg.(type) {
		case *RegisterConfirmationRequest:
			if err := a.processConfirmationRequest(
				ctx, m,
			); err != nil {
				return err
			}

		case *StartTimeoutReq:
			compositeID := makeTimeoutID(m.RoundKey, m.Phase)

			mapFn := func(expired timeout.ExpiredMsg,
			) actormsg.RoundReceivable {

				return &TimeoutMsg{
					TimeoutID: expired.ID,
				}
			}
			callbackRef := timeout.MapTimeoutExpired(
				a.cfg.SelfRef, mapFn,
			)

			req := &timeout.ScheduleTimeoutRequest{
				ID:       compositeID,
				Duration: m.Duration,
				Callback: callbackRef,
			}
			if err := a.cfg.TimeoutActor.Tell(
				ctx, req,
			); err != nil {
				return fmt.Errorf("schedule timeout: %w", err)
			}

		case *CancelTimeoutReq:
			compositeID := makeTimeoutID(m.RoundKey, m.Phase)
			req := &timeout.CancelTimeoutRequest{
				ID: compositeID,
			}
			if err := a.cfg.TimeoutActor.Tell(
				ctx, req,
			); err != nil {
				return fmt.Errorf("cancel timeout: %w", err)
			}

		case *ReleaseForfeitReservation:
			// Release forfeit-reserved VTXOs back to LiveState via
			// the VTXO manager. Best-effort and fire-and-forget: a
			// failed release is logged but must not halt outbox
			// processing for the (already failed) round. Routing
			// through the manager keeps its reservation set in sync
			// so the released inputs can be re-selected for a
			// retry.
			if a.cfg.VTXOManager == nil || len(m.Outpoints) == 0 {
				continue
			}
			if err := a.cfg.VTXOManager.Tell(
				ctx, &actormsg.ReleaseForfeitRequest{
					Outpoints: m.Outpoints,
				},
			); err != nil {

				a.log.WarnS(
					ctx,
					"Failed to release forfeit reservation",
					err,
					slog.Int(
						"outpoints", len(m.Outpoints),
					),
				)
			}

		case *DropCustomForfeitReservation:
			// Drop custom PendingForfeit signer actors created for
			// caller-supplied refresh inputs. They are not wallet
			// VTXOs, so they must not be released into LiveState.
			if len(m.Outpoints) == 0 {
				continue
			}
			if a.cfg.VTXOManager != nil {
				if err := a.cfg.VTXOManager.Tell(
					ctx, &actormsg.DropCustomForfeitInputsRequest{
						Outpoints: m.Outpoints,
					},
				); err != nil {

					a.log.WarnS(
						ctx,
						"Failed to drop custom "+
							"forfeit inputs",
						err,
						slog.Int(
							"outpoints",
							len(m.Outpoints),
						),
					)
				}
			}
			if a.cfg.DropCustomForfeitSigningContexts != nil {
				err := a.cfg.DropCustomForfeitSigningContexts(
					ctx, m.Outpoints,
				)
				if err != nil {
					a.log.WarnS(
						ctx,
						"Failed to drop custom forfeit "+
							"signing contexts",
						err,
						slog.Int(
							"outpoints",
							len(m.Outpoints),
						),
					)
				}
			}

		case *VTXOCreatedNotification:
			// Forward to VTXO manager to spawn actors for the new
			// VTXOs if configured.
			if a.cfg.VTXOManager != nil {
				if err := a.cfg.VTXOManager.Tell(
					ctx, m,
				); err != nil {

					a.log.WarnS(
						ctx,
						"Failed to notify VTXO manager",
						err,
					)
				}
			}

			// Mirror each newly-confirmed VTXO into the client
			// ledger so vtxo_balance follows round confirmation.
			// Source is posted as SourceRoundTransfer with the
			// on-wire (net) amount: distinguishing boarding vs
			// transfer (and pairing FeePaidMsg for the operator
			// fee) requires the round FSM to surface the fee and
			// the boarding-input provenance, which is TODO.
			a.emitVTXOsReceived(ctx, m)

		case *RoundCompletedNotification:
			a.log.InfoS(
				ctx,
				"Processing round completion notification",
				slog.String("round_id", m.RoundID.String()),
				slog.String("txid", m.TxID.String()),
			)

			// Round FSM reached ConfirmedState. Perform actor
			// cleanup.
			err := a.onRoundComplete(
				ctx, m.RoundID, m.TxID, m.ConfInfo,
			)
			if err != nil {
				return fmt.Errorf("failed to complete round "+
					"%s: %w", m.RoundID, err)
			}

			// Count the confirmed round for observability.
			a.emitRoundCompleted(
				ctx, m.RoundID.String(),
				"confirmed",
			)

		case *RoundCheckpointedNotification:
			a.log.InfoS(
				ctx,
				"Processing round checkpoint notification",
				slog.String("round_id", m.RoundID.String()),
			)

			// Find the round by its RoundID (should already be
			// re-keyed at this point).
			keyStr := RoundKeyStr(m.RoundID.KeyString())
			roundFSM, exists := a.rounds[keyStr]
			if !exists {
				return fmt.Errorf("round not found for "+
					"checkpoint: %s", m.RoundID)
			}

			// Get the current state to extract commitment tx info.
			state, err := roundFSM.FSM.CurrentState()
			if err != nil {
				return fmt.Errorf("failed to get state: %w",
					err)
			}

			inputSigState, ok := state.(*InputSigSentState)
			if !ok {
				return fmt.Errorf("round not in "+
					"InputSigSentState, got %T", state)
			}

			// Update round FSM with commitment tx info.
			txid := inputSigState.CommitmentTx.UnsignedTx.TxHash()
			roundFSM.TxID = txid
			roundFSM.CommitmentTx = fn.Some(
				inputSigState.CommitmentTx,
			)

			// Index for confirmation routing and register.
			a.commitmentTxIndex[txid] = keyStr
			a.registerCommitmentConfirmation(
				ctx, txid, roundFSM.CommitmentTx,
				inputSigState.VTXOTreePaths,
			)

			a.log.InfoS(ctx, "Round checkpoint processed",
				slog.String("round_id", m.RoundID.String()),
				slog.String("commitment_txid", txid.String()),
			)

		case *RoundFailedNotification:
			// Round entered failed state. Log for observability.
			roundIDStr := "none"
			m.RoundID.WhenSome(func(id RoundID) {
				roundIDStr = id.String()
			})
			a.log.WarnS(ctx, "Round failed",
				m.OriginalError,
				slog.String("round_id", roundIDStr),
				slog.String("reason", m.Reason),
				slog.Bool("recoverable", m.Recoverable),
			)

			// Count the failed round for observability. The
			// counter pairs with the confirmed branch above so an
			// operator can track the join-to-completion ratio.
			a.emitRoundCompleted(ctx, roundIDStr, "failed")

		case *TerminalJobFailedNotification:
			// A terminal-for-job round failure (e.g. the operator
			// could not fund the commitment tx). The accompanying
			// ReleaseForfeitReservation has already returned the
			// VTXOs to the live set; here we drop the originating
			// job's persisted pending intent so restart replay does
			// not re-submit the same inputs into the same wall, and
			// surface the job's activity entry as failed.
			a.handleTerminalJobFailure(ctx, m)

		case *ForfeitRequestToVTXO:
			// Route forfeit request to VTXO actor via service key.
			// The VTXO actor will sign the forfeit tx and respond
			// with ForfeitSignatureResponse.
			a.log.DebugS(ctx, "Processing ForfeitRequestToVTXO",
				slog.String("outpoint",
					m.VTXOOutpoint.String()),
				slog.String("round_id", m.RoundID),
				slog.Bool(
					"actor_system_nil",
					a.cfg.ActorSystem == nil,
				),
			)

			if a.cfg.ActorSystem != nil {
				serviceKey := actormsg.VTXOActorServiceKey(
					m.VTXOOutpoint,
				)
				a.log.DebugS(
					ctx,
					"Looking up VTXO actor by service key",
					slog.String(
						"outpoint",
						m.VTXOOutpoint.String(),
					),
				)

				// The VTXO actor may process this request after
				// the current server-message handler returns.
				// Detach the context so its follow-up
				// ForfeitSignatureResponse relay is not
				// canceled before it reaches this round.
				reqCtx := context.WithoutCancel(ctx)
				err := serviceKey.Ref(a.cfg.ActorSystem).Tell(
					reqCtx, &ForfeitRequestEvent{
						RoundID:               m.RoundID,
						ConnectorOutpoint:     m.ConnectorOutpoint,
						ConnectorPkScript:     m.ConnectorPkScript,
						ConnectorAmount:       m.ConnectorAmount,
						ServerForfeitPkScript: m.ServerForfeitPkScript,
						ForfeitSpend:          m.ForfeitSpend,
					},
				)
				if err != nil {
					a.log.WarnS(
						ctx,
						"Failed to send forfeit "+
							"request to VTXO actor",
						err,
						slog.String(
							"outpoint",
							m.VTXOOutpoint.String(),
						),
					)

					return fmt.Errorf("send forfeit "+
						"request to VTXO actor: %w",
						err)
				}
				a.log.InfoS(
					ctx,
					"Sent forfeit request to VTXO actor",
					slog.String(
						"outpoint",
						m.VTXOOutpoint.String(),
					),
					slog.String("round_id", m.RoundID),
				)
			} else {
				a.log.WarnS(
					ctx,
					"Cannot send forfeit request: "+
						"ActorSystem is nil",
					nil,
					slog.String(
						"outpoint",
						m.VTXOOutpoint.String(),
					),
				)
			}

		case *ForfeitConfirmedToVTXO:
			// Notify VTXO actor that forfeit is confirmed. The old
			// VTXO is now permanently forfeited.
			if a.cfg.ActorSystem != nil {
				serviceKey := actormsg.VTXOActorServiceKey(
					m.VTXOOutpoint,
				)
				err := serviceKey.Ref(a.cfg.ActorSystem).Tell(
					ctx, &ForfeitConfirmedEvent{
						CommitmentTxID: m.CommitmentTxID,
						BlockHeight:    m.BlockHeight,
					},
				)
				if err != nil {
					a.log.WarnS(ctx,
						"Failed to send forfeit "+
							"confirmation",
						err,
						slog.String(
							"outpoint",
							m.VTXOOutpoint.String(),
						))
				}
				a.log.InfoS(ctx,
					"Sent forfeit confirmed to VTXO",
					slog.String(
						"outpoint",
						m.VTXOOutpoint.String(),
					),
					slog.String(
						"commitment_txid",
						m.CommitmentTxID.String(),
					))
			}

		default:
			// Unknown outbox message type. Log for debugging.
			a.log.DebugS(
				ctx,
				"Ignoring unknown outbox message type",
				slog.String("type", fmt.Sprintf("%T", msg)),
			)
		}
	}

	return nil
}

// processConfirmationRequest handles a RegisterConfirmationRequest emitted by
// the round FSM. It builds a caller ID, creates a mapped actor ref for
// confirmation delivery, queries the current block height for HeightHint, and
// sends the registration to ChainSource.
func (a *RoundClientActor) processConfirmationRequest(
	ctx context.Context, m *RegisterConfirmationRequest,
) error {

	// Build a unique caller ID from the pkscript or txid.
	var sessionID string
	switch {
	case len(m.PkScript) > 0:
		sessionID = hex.EncodeToString(m.PkScript)

	case m.Txid != nil:
		sessionID = m.Txid.String()

	default:
		sessionID = "unknown"
	}
	callerID := fmt.Sprintf("boarding-%s-%s", sessionID, m.CallerID)

	// Use the shared mapper helper so ChainSource can deliver
	// confirmation events directly without an intermediate actor.
	mappedRef := chainsource.MapConfirmationEvent(
		a.cfg.SelfRef,
		func(ce chainsource.ConfirmationEvent) actormsg.RoundReceivable {
			return &ConfirmationEvent{
				Txid:          ce.Txid,
				BlockHeight:   ce.BlockHeight,
				Confirmations: ce.NumConfs,
				Tx:            ce.Tx,
			}
		},
	)

	// Query ChainSource for current block height to use as
	// HeightHint. LND requires HeightHint > 0 for confirmation
	// scanning.
	heightHint := m.HeightHint
	if heightHint == 0 {
		heightFuture := a.cfg.ChainSource.Ask(
			ctx, &chainsource.BestHeightRequest{},
		)
		heightResult := heightFuture.Await(ctx)
		heightResp, err := heightResult.Unpack()
		if err != nil {
			return fmt.Errorf("get best height for "+
				"confirmation: %w", err)
		}
		bestHeightResp, ok :=
			heightResp.(*chainsource.BestHeightResponse)
		if !ok {
			return fmt.Errorf("unexpected height response type")
		}
		heightHint = uint32(bestHeightResp.Height)
	}

	// Build the complete RegisterConfRequest with the mapper as
	// the NotifyActor target.
	confReq := &chainsource.RegisterConfRequest{
		CallerID:    callerID,
		Txid:        m.Txid,
		PkScript:    m.PkScript,
		TargetConfs: m.TargetConfs,
		HeightHint:  heightHint,
		NotifyActor: fn.Some(mappedRef),
	}

	a.log.InfoS(ctx, "Sending RegisterConfRequest to ChainSource",
		slog.String("caller_id", callerID),
		slog.Int("pkscript_len", len(m.PkScript)),
		slog.Int("height_hint", int(heightHint)),
		slog.Int("target_confs", int(m.TargetConfs)),
	)

	if err := a.cfg.ChainSource.Tell(
		a.lifecycleCtx(ctx), confReq,
	); err != nil {

		a.log.WarnS(ctx,
			"Failed to register confirmation",
			err,
		)
	}

	return nil
}

// handleRefreshVTXORequest processes a refresh request from a VTXO actor.
// The VTXO is approaching expiry and needs to be included in the next batch
// swap round. The actor translates the request into a single IntentPackage
// containing one forfeit input and one new VTXO output.
//
// NOTE: Unlike handleRegisterIntent, no PendingForfeitEvent is sent back to
// the VTXO actor here. The VTXO actor has already self-transitioned to
// PendingForfeitState before sending this relay message, so the notification
// would be a no-op.
func (a *RoundClientActor) handleRefreshVTXORequest(ctx context.Context,
	req *RefreshVTXORequest) fn.Result[actormsg.RoundActorResp] {

	// Find an assembling round (Idle or PendingRoundAssembly) or create
	// one. We must not use findPendingRound here because it matches by
	// temp-key status, which includes rounds in IntentSentState.
	// Feeding an IntentPackage to IntentSentState would self-loop
	// silently, discarding the intent.
	var err error
	roundFSM := a.findAssemblingRound()
	if roundFSM == nil {
		roundFSM, err = a.createNewRound(ctx)
		if err != nil {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("failed to create round for "+
					"refresh: %w", err),
			)
		}
	}

	vtxoReq := buildVTXORequestFromRefresh(req)
	if a.cfg.OwnedScriptRegistrar != nil && vtxoReq.HasLocalOwner() {
		pkScript, err := vtxoReq.EffectivePkScript()
		if err != nil {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("derive refresh vtxo pkScript: %w",
					err),
			)
		}

		regErr := a.cfg.OwnedScriptRegistrar.RegisterOwnedScript(
			ctx, pkScript, vtxoReq.OwnerKey,
		)
		if regErr != nil {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("register refresh owned script: %w",
					regErr),
			)
		}
	}

	// Bundle the forfeit input and new VTXO output atomically.
	pkg := &IntentPackage{Intents: Intents{
		Forfeits: []types.ForfeitRequest{{
			VTXOOutpoint: &req.VTXOOutpoint,
			Amount:       btcutil.Amount(req.Amount),
		}},
		VTXOs: []types.VTXORequest{
			vtxoReq,
		},
	}}
	err = a.askEventAndProcessOutbox(ctx, roundFSM, pkg)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("FSM error processing refresh package: %w",
				err),
		)
	}

	// Reschedule one timeout for the assembling round on every refresh.
	// This gives other VTXOs from the same block epoch a brief window to
	// join the same intent before its single registration is emitted.
	err = a.processOutbox(ctx, []ClientOutMsg{
		&StartTimeoutReq{
			RoundKey: RoundKeyStr(roundFSM.Key.KeyString()),
			Phase:    TimeoutPhaseRefreshRegistration,
			Duration: defaultRefreshRegistrationDelay,
		},
	})
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("schedule automatic refresh "+
				"registration: %w", err),
		)
	}

	a.log.InfoS(ctx, "Queued VTXO for refresh",
		slog.String("outpoint", req.VTXOOutpoint.String()),
		slog.Int64("amount", req.Amount),
	)

	return fn.Ok[actormsg.RoundActorResp](nil)
}

// handleRegisterIntent processes a pre-composed intent package from the
// wallet. The wallet has already loaded VTXO descriptors and built the full
// IntentPackage. The round actor registers it with the FSM.
//
// The wallet is responsible for reserving forfeit inputs through the VTXO
// manager before sending this message. By the time the round receives the
// intent, the affected VTXOs are already in PendingForfeitState. If round
// registration fails, the wallet releases the reservations.
func (a *RoundClientActor) handleRegisterIntent(ctx context.Context,
	req *RegisterIntentRequest) fn.Result[actormsg.RoundActorResp] {

	if req.Package == nil || req.Package.isEmpty() {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("empty intent package"),
		)
	}

	// Find an assembling round (Idle or PendingRoundAssembly) or create
	// one. We must not use findPendingRound here because it matches by
	// temp-key status, which includes rounds in IntentSentState.
	// Feeding an IntentPackage to IntentSentState would self-loop
	// silently, discarding the intent.
	roundFSM := a.findAssemblingRound()
	if roundFSM == nil {
		var err error
		roundFSM, err = a.createNewRound(ctx)
		if err != nil {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("failed to create round for "+
					"intent: %w", err),
			)
		}
	}

	// Register locally-owned VTXO pkScripts from the intent so
	// the OwnedScriptChecker recognizes them at confirmation
	// time. Local ownership is carried explicitly by the
	// presence of an owner descriptor rather than inferred from
	// a non-zero key locator.
	if a.cfg.OwnedScriptRegistrar != nil {
		for _, vtxo := range req.Package.VTXOs {
			if !vtxo.HasLocalOwner() {
				continue
			}

			pkScript, err := vtxo.EffectivePkScript()
			if err != nil {
				return fn.Err[actormsg.RoundActorResp](
					fmt.Errorf("derive owned script "+
						"pkScript: %w", err),
				)
			}

			regErr := a.cfg.OwnedScriptRegistrar.RegisterOwnedScript( //nolint:ll
				ctx, pkScript, vtxo.OwnerKey,
			)
			if regErr != nil {
				return fn.Err[actormsg.RoundActorResp](
					fmt.Errorf("register owned script: %w",
						regErr),
				)
			}
		}
	}

	// Feed the pre-composed package to the FSM.
	err := a.askEventAndProcessOutbox(ctx, roundFSM, req.Package)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("FSM error processing intent package: %w",
				err),
		)
	}

	// NOTE: PendingForfeitEvent is no longer sent here. The wallet
	// reserves forfeit inputs through the VTXO manager before
	// sending RegisterIntentMsg, so VTXOs are already in
	// PendingForfeitState by the time the round registers the
	// intent. The manager handles atomic reservation and rollback.

	// For directed sends, immediately trigger registration to
	// advance from PendingRoundAssembly to RegistrationSent.
	// Other flows (refresh, leave) accumulate intents before
	// registering.
	if req.TriggerRegistration {
		regEvent := &IntentRequested{}
		err = a.askEventAndProcessOutbox(
			ctx, roundFSM, regEvent,
		)
		if err != nil {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("trigger send registration: %w",
					err),
			)
		}
	}

	a.log.InfoS(ctx, "Registered intent package",
		slog.Int("forfeits", len(req.Package.Forfeits)),
		slog.Int("vtxos", len(req.Package.VTXOs)),
		slog.Int("leaves", len(req.Package.Leaves)),
	)

	return fn.Ok[actormsg.RoundActorResp](nil)
}

// handleForfeitSignatureResponse processes a forfeit signature from a VTXO
// actor. The VTXO actor has signed the forfeit transaction as part of a batch
// swap round. The signature is forwarded to the round's FSM for tracking.
func (a *RoundClientActor) handleForfeitSignatureResponse(ctx context.Context,
	resp *ForfeitSignatureResponse) fn.Result[actormsg.RoundActorResp] {

	roundIDStr := resp.RoundID

	keyStr := RoundKeyStr(roundIDStr)
	roundFSM, exists := a.rounds[keyStr]
	if !exists {
		a.log.WarnS(ctx, "Forfeit signature for unknown round",
			nil,
			slog.String("outpoint", resp.VTXOOutpoint.String()),
			slog.String("round_id", roundIDStr),
		)

		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("unknown round %s for forfeit signature",
				roundIDStr),
		)
	}

	// Forward to round FSM. The FSM tracks collected signatures and emits
	// a server message when all expected signatures are collected.
	err := a.askEventAndProcessOutbox(ctx, roundFSM, resp)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("FSM error processing forfeit "+
				"signature: %w", err),
		)
	}

	a.log.InfoS(ctx, "Collected forfeit signature",
		slog.String("outpoint", resp.VTXOOutpoint.String()),
		slog.String("round_id", roundIDStr),
	)

	return fn.Ok[actormsg.RoundActorResp](nil)
}

// handleTriggerBoard processes a board request forwarded from the wallet actor.
// It registers the VTXO output amounts into a round FSM and then triggers
// IntentRequested to kick off the round join flow. This combines the
// RegisterVTXORequests + TriggerRegistration steps that the Board RPC
// previously performed directly.
func (a *RoundClientActor) handleTriggerBoard(ctx context.Context,
	cmd *actormsg.TriggerBoardMsg) fn.Result[actormsg.RoundActorResp] {

	if len(cmd.Amounts) == 0 {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("board amounts are empty"),
		)
	}

	// Resolve the boarding inputs before minting any VTXO owner keys, so a
	// redundant trigger short-circuits without advancing the wallet key
	// ring or registering owned scripts for outputs we never send.
	confirmedBoarding, err := a.cfg.WalletActor.Ask(
		ctx, &wallet.GetConfirmedBoardingIntentsRequest{},
	).Await(ctx).Unpack()
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("fetch confirmed boarding intents: %w", err),
		)
	}

	boardingResp, ok :=
		confirmedBoarding.(*wallet.GetConfirmedBoardingIntentsResponse)
	if !ok {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("unexpected wallet response type: %T",
				confirmedBoarding),
		)
	}

	// When the trigger names the boarding outpoints it sized its amounts
	// over, register exactly those inputs. The wallet excludes outpoints
	// it has already shipped into an in-flight round, so honoring the set
	// keeps the proven inputs coherent with the amounts and prevents a
	// second deposit's trigger from re-proving an already-in-flight
	// outpoint under a fresh owner key. An empty set means "all confirmed
	// inputs" — the pre-existing behavior for legacy callers and tests.
	var wantOutpoints fn.Set[wire.OutPoint]
	if len(cmd.Outpoints) > 0 {
		wantOutpoints = fn.NewSet(cmd.Outpoints...)
	}

	boardingIntents := make(
		[]BoardingIntent, 0, len(boardingResp.Intents),
	)
	for i := range boardingResp.Intents {
		if wantOutpoints != nil &&
			!wantOutpoints.Contains(
				boardingResp.Intents[i].Outpoint,
			) {

			continue
		}

		intent, err := buildBoardingIntentFromWallet(
			&boardingResp.Intents[i],
		)
		if err != nil {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("convert confirmed boarding "+
					"intent %d: %w", i, err),
			)
		}

		boardingIntents = append(boardingIntents, intent)
	}

	// A named-outpoint trigger whose inputs are all gone (adopted between
	// the wallet's dispatch and this fetch) is redundant: skip it rather
	// than mint owner keys for outputs with no inputs to fund them.
	if wantOutpoints != nil && len(boardingIntents) == 0 {
		a.log.InfoS(ctx, "Skipping board trigger; named boarding "+
			"outpoints are no longer confirmed",
			slog.Int("named_outpoint_count", len(cmd.Outpoints)),
		)

		return fn.Ok[actormsg.RoundActorResp](nil)
	}

	// Build VTXO requests from the provided amounts.
	requests := make([]types.VTXORequest, 0, len(cmd.Amounts))
	for i, amount := range cmd.Amounts {
		if amount <= 0 {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("board VTXO amount %d is "+
					"invalid: %v", i, amount),
			)
		}

		// Boarding flow: the output VTXO is funded by the
		// client's on-chain wallet input, so the ledger
		// emission must credit wallet_balance via
		// SourceRoundBoarding. Tag origin here so the
		// classification flows through the FSM to the
		// VTXOCreatedNotification dispatch.
		//
		// When the board request pins a custom policy template,
		// build the output from it verbatim (mirroring the
		// custom-refresh path) instead of synthesizing the
		// standard policy with a freshly derived owner key. This
		// lets a client board straight into a custom-owned VTXO,
		// e.g. one owned by an external FROST aggregate key.
		var req *types.VTXORequest
		if len(cmd.PolicyTemplate) > 0 {
			req = buildCustomBoardVTXORequest(
				amount, cmd.PolicyTemplate, cmd.PkScript,
				types.VTXOOriginRoundBoarding,
			)
		} else {
			req, err = a.buildVTXORequest(
				ctx, amount, types.VTXOOriginRoundBoarding,
			)
			if err != nil {
				return fn.Err[actormsg.RoundActorResp](
					fmt.Errorf("build board VTXO request "+
						"%d: %w", i, err),
				)
			}
		}

		requests = append(requests, *req)
	}

	a.log.InfoS(ctx, "Processing board request",
		slog.Int("vtxo_count", len(requests)),
		slog.Int("boarding_input_count", len(boardingIntents)),
	)

	// Find an existing assembling round or create a new one.
	roundFSM := a.findAssemblingRound()
	if roundFSM == nil {
		roundFSM, err = a.createNewRound(ctx)
		if err != nil {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("create round for board: %w", err),
			)
		}
	}

	// Register the VTXO output requests into the round FSM. A change
	// leave output rides along when the wallet clipped the boarding
	// balance to the operator's limits: it pays the remainder back to
	// a fresh boarding script owned by the wallet.
	var leaves []*types.LeaveRequest
	if cmd.Change != nil {
		if cmd.Change.Output == nil ||
			cmd.Change.Output.Value <= 0 {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("board change output is invalid"),
			)
		}

		leaves = append(leaves, cmd.Change)
	}

	pkg := &IntentPackage{Intents: Intents{
		Boarding: boardingIntents,
		VTXOs:    requests,
		Leaves:   leaves,
	}}

	err = a.askEventAndProcessOutbox(ctx, roundFSM, pkg)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("register board VTXO requests: %w", err),
		)
	}

	// Trigger registration to kick off the round join flow.
	// This transitions the FSM from PendingRoundAssembly to
	// RegistrationSent.
	regEvent := &IntentRequested{}
	err = a.askEventAndProcessOutbox(ctx, roundFSM, regEvent)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("trigger board registration: %w", err),
		)
	}

	a.log.InfoS(ctx, "Board registration triggered",
		slog.Int("vtxo_count", len(requests)),
	)

	return fn.Ok[actormsg.RoundActorResp](nil)
}
