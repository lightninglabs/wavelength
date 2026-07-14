package vtxo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/metrics"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

// OwnedReceiveScript holds the metadata returned when looking up a
// pkScript in the owned receive scripts store.
type OwnedReceiveScript struct {
	// ClientKey is the local wallet key descriptor for this script.
	ClientKey keychain.KeyDescriptor

	// OperatorPubKey is the operator pubkey used for this script.
	OperatorPubKey *btcec.PublicKey

	// ExitDelay is the relative CSV delay for the exit path.
	ExitDelay int64
}

// OwnedScriptLookup provides read access to the owned receive scripts
// store. Implementations return sql.ErrNoRows when the script is not
// found.
type OwnedScriptLookup interface {
	// LookupOwnedReceiveScript returns the metadata for a
	// registered owned receive script, or sql.ErrNoRows if the
	// script is not tracked.
	LookupOwnedReceiveScript(ctx context.Context,
		pkScript []byte) (*OwnedReceiveScript, error)
}

// VTXOSaver persists materialized VTXO descriptors.
type VTXOSaver interface {
	// SaveVTXO persists the given VTXO descriptor.
	SaveVTXO(ctx context.Context, desc *Descriptor) error
}

// IncomingVTXOMsg wraps an IncomingVTXOEvent for the handler actor.
type IncomingVTXOMsg struct {
	actor.BaseMessage
	Event *arkrpc.IncomingVTXOEvent
}

// MessageType returns a human-readable message identifier.
func (m IncomingVTXOMsg) MessageType() string {
	return fmt.Sprintf("IncomingVTXOMsg(event_id=%d)", m.Event.GetEventId())
}

// IncomingVTXOResp is the handler's response type.
type IncomingVTXOResp = any

const incomingVTXOServiceKeyName = "incoming-vtxo-handler"

// IncomingVTXOServiceKey returns the well-known service key for the
// incoming VTXO handler actor.
func IncomingVTXOServiceKey() actor.ServiceKey[
	IncomingVTXOMsg, IncomingVTXOResp] {

	return actor.NewServiceKey[IncomingVTXOMsg, IncomingVTXOResp](
		incomingVTXOServiceKeyName,
	)
}

// IncomingVTXOExtras carries the descriptor fields the unilateral-exit
// unroll path needs but the lightweight IncomingVTXOEvent push doesn't
// carry. Resolved synchronously by IncomingAncestryFetcher.
type IncomingVTXOExtras struct {
	// Ancestry is the set of rooted commitment-tree fragments
	// required to claim this VTXO unilaterally on-chain. Empty
	// fails the validateProofDescriptorShape gate; the unroll FSM
	// rejects empty-ancestry targets before advancing.
	Ancestry []Ancestry

	// CreatedHeight is the block height at which the VTXO became
	// relevant on-chain. validateProofDescriptorShape rejects a
	// zero value (a freshly-materialized descriptor must commit
	// to which block its round commit confirmed in, or sweep
	// scheduling has no reference point).
	CreatedHeight int32
}

// IncomingAncestryFetcher resolves the per-VTXO metadata the
// unilateral-exit unroll path needs to construct a CPFP child:
// ancestry slice plus the created-height anchor. Invoked
// synchronously inside IncomingVTXOHandler.Receive after the receive
// script is matched and the descriptor is built, so the persisted
// descriptor carries full lineage from the first save.
//
// The handler routes per-script signing via clientKey so the
// implementation can issue an indexer ListVTXOsByScripts query under
// the owner's proof-of-control. A nil fetcher (legacy harnesses /
// non-waved consumers) causes the handler to persist without
// extras — the cooperative spend paths (refresh, OOR, leave) still
// work; only unilateral exit is impossible until backfill. Production
// wiring (see waved) supplies an indexer-backed implementation.
type IncomingAncestryFetcher func(ctx context.Context,
	outpoint wire.OutPoint, pkScript []byte,
	clientKey keychain.KeyDescriptor) (IncomingVTXOExtras, error)

// IncomingVTXOHandlerConfig holds the handler's dependencies.
type IncomingVTXOHandlerConfig struct {
	// Log is the optional logger for the handler.
	Log fn.Option[btclog.Logger]

	// ScriptStore is the persistence store used to look up owned
	// receive scripts by pkScript.
	ScriptStore OwnedScriptLookup

	// VTXOStore is the persistence store used to save materialized
	// VTXO descriptors.
	VTXOStore VTXOSaver

	// VTXOManager is a tell-only reference to the VTXO manager
	// actor, used to notify it of newly materialized VTXOs.
	VTXOManager actor.TellOnlyRef[ManagerMsg]

	// AncestryFetcher resolves the round commit tree fragments
	// required to unilaterally exit each incoming VTXO. Nil falls
	// back to persisting without ancestry; production must supply
	// a non-nil implementation to keep the unilateral exit path
	// usable for received VTXOs (see bug-3 in BUGS_FOUND.md).
	AncestryFetcher IncomingAncestryFetcher

	// MetricsSink is an optional reference to the client-side metrics
	// actor. When set, the handler emits OORTransferReceivedMsg once
	// it knows the terminal outcome of an owned incoming VTXO:
	// "materialized" after the descriptor is persisted, or "failed"
	// when a relevant receive cannot be persisted. Emission lives here
	// (not at the waved routing boundary) because only this handler
	// observes whether the event was relevant and the save succeeded;
	// counting at adapt time would report success for events that are
	// later ignored or fail to persist.
	MetricsSink fn.Option[metrics.Sink]
}

// IncomingVTXOHandler materializes VTXOs from IncomingVTXOEvent
// notifications pushed by the server's indexer after round
// confirmation.
type IncomingVTXOHandler struct {
	cfg IncomingVTXOHandlerConfig
	log btclog.Logger
}

// NewIncomingVTXOHandler creates a new handler.
func NewIncomingVTXOHandler(
	cfg IncomingVTXOHandlerConfig) *IncomingVTXOHandler {

	return &IncomingVTXOHandler{
		cfg: cfg,
		log: cfg.Log.UnwrapOr(btclog.Disabled),
	}
}

// emitReceived reports the terminal outcome of an owned incoming VTXO to
// the metrics actor so oor_transfers_received_total reflects reality.
// Status is "materialized" (persisted) or "failed" (a relevant receive
// that could not be persisted). It is best-effort and fire-and-forget: a
// Tell failure is logged at debug level and never fails the receive. The
// pre-ownership ignore paths (non-CREATED events, malformed pushes,
// scripts that are not ours) emit nothing — they are not transfers to
// this wallet.
func (h *IncomingVTXOHandler) emitReceived(ctx context.Context, status string) {
	h.cfg.MetricsSink.WhenSome(func(sink metrics.Sink) {
		msg := &metrics.OORTransferReceivedMsg{Status: status}
		if err := sink.Tell(ctx, msg); err != nil {
			h.log.DebugS(ctx, "Failed to emit received metric",
				err,
				slog.String("status", status),
			)
		}
	})
}

// Receive processes IncomingVTXOEvent messages.
func (h *IncomingVTXOHandler) Receive(ctx context.Context,
	msg IncomingVTXOMsg) fn.Result[IncomingVTXOResp] {

	evt := msg.Event
	if evt == nil {
		return fn.Ok[IncomingVTXOResp](nil)
	}

	// We only handle VTXO_CREATED events. Log unexpected types
	// so we notice if the server starts sending new event kinds.
	if evt.Type != arkrpc.VTXOEventType_VTXO_EVENT_TYPE_CREATED {
		h.log.DebugS(ctx, "Ignoring non-CREATED VTXO event",
			slog.Int("type", int(evt.Type)),
		)

		return fn.Ok[IncomingVTXOResp](nil)
	}

	op := evt.GetOutpoint()
	if op == nil || len(op.Txid) != 32 {
		h.log.WarnS(ctx, "IncomingVTXOEvent has invalid "+
			"or missing outpoint", nil)

		return fn.Ok[IncomingVTXOResp](nil)
	}

	pkScript := evt.GetPkScript()
	if len(pkScript) == 0 {
		h.log.WarnS(ctx, "IncomingVTXOEvent has empty "+
			"pkScript", nil)

		return fn.Ok[IncomingVTXOResp](nil)
	}

	var outpoint wire.OutPoint
	copy(outpoint.Hash[:], op.Txid)
	outpoint.Index = op.Vout

	h.log.InfoS(ctx, "Received IncomingVTXOEvent",
		slog.String("outpoint", outpoint.String()),
		slog.Uint64("value_sat", evt.ValueSat),
		slog.String("round_id", evt.RoundId),
	)

	if h.cfg.ScriptStore == nil {
		return fn.Ok[IncomingVTXOResp](nil)
	}

	// Look up the pkScript in owned receive scripts.
	rec, err := h.cfg.ScriptStore.LookupOwnedReceiveScript(
		ctx, pkScript,
	)
	if err != nil {
		// Not-found means the script isn't ours — ignore.
		// Any other error is a real store failure that
		// should be surfaced.
		if errors.Is(err, sql.ErrNoRows) {
			return fn.Ok[IncomingVTXOResp](nil)
		}

		// A real store failure means we could not process an
		// incoming event; count it as a failed receive.
		h.emitReceived(ctx, "failed")

		return fn.Err[IncomingVTXOResp](
			fmt.Errorf("lookup owned receive script: %w", err),
		)
	}

	if rec.ClientKey.PubKey == nil {
		h.log.WarnS(ctx, "Owned receive script has nil "+
			"client pubkey", nil,
			slog.String("outpoint", outpoint.String()))

		return fn.Ok[IncomingVTXOResp](nil)
	}

	// Reject server-provided values that would overflow int64
	// when cast to btcutil.Amount.
	if evt.ValueSat > uint64(math.MaxInt64) ||
		evt.ValueSat > uint64(btcutil.MaxSatoshi) {

		h.log.WarnS(ctx, "Incoming VTXO value exceeds "+
			"maximum", nil,
			slog.String("outpoint", outpoint.String()),
			slog.Uint64("value_sat", evt.ValueSat))

		return fn.Ok[IncomingVTXOResp](nil)
	}

	// Build the tapscript for the descriptor.
	operatorKey := rec.OperatorPubKey
	exitDelay := uint32(rec.ExitDelay)

	tapscript, err := arkscript.VTXOTapScript(
		rec.ClientKey.PubKey, operatorKey, exitDelay,
	)
	if err != nil {
		h.log.WarnS(ctx, "Failed to derive tapscript "+
			"for incoming VTXO", err,
			slog.String("outpoint", outpoint.String()))

		return fn.Ok[IncomingVTXOResp](nil)
	}

	// Use the commitment tx ID from the event, which references
	// the round's commitment transaction. This is distinct from
	// the leaf txid in the outpoint.
	var commitTxID chainhash.Hash
	if len(evt.CommitmentTxid) == chainhash.HashSize {
		copy(commitTxID[:], evt.CommitmentTxid)
	}

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		rec.ClientKey.PubKey, operatorKey, exitDelay,
	)
	if err != nil {
		h.log.WarnS(ctx, "Failed to encode policy for incoming VTXO",
			err,
			slog.String("outpoint", outpoint.String()),
		)

		return fn.Ok[IncomingVTXOResp](nil)
	}

	desc := &Descriptor{
		Outpoint:       outpoint,
		Amount:         btcutil.Amount(evt.ValueSat),
		PolicyTemplate: policyTemplate,
		PkScript:       pkScript,
		ClientKey:      rec.ClientKey,
		OperatorKey:    operatorKey,
		TapScript:      tapscript,
		RoundID:        evt.RoundId,
		CommitmentTxID: commitTxID,
		BatchExpiry:    evt.BatchExpiryHeight,
		RelativeExpiry: evt.RelativeExpiry,
		Status:         VTXOStatusLive,
	}

	// Resolve ancestry before persisting so the descriptor lands
	// with full unilateral-exit material from the first write. The
	// indexer push (IncomingVTXOEvent) is intentionally thin and
	// carries no tree fragments; without this fetch the unroll
	// path fails with "descriptor missing ancestry" on every
	// in-round-received VTXO (see bug-3 in BUGS_FOUND.md). A nil
	// fetcher preserves the legacy degraded behavior: cooperative
	// spend paths still work, only unilateral exit is blocked.
	// Fetch failures are warn-logged but do not block
	// materialization, since the receive must still succeed for
	// cooperative use.
	if h.cfg.AncestryFetcher != nil {
		extras, err := h.cfg.AncestryFetcher(
			ctx, outpoint, pkScript, rec.ClientKey,
		)
		if err != nil {
			h.log.WarnS(ctx, "Failed to fetch incoming VTXO "+
				"ancestry; persisting without — unilateral "+
				"exit will be unavailable until backfill",
				err,
				slog.String("outpoint", outpoint.String()),
			)
		} else {
			desc.Ancestry = extras.Ancestry
			desc.CreatedHeight = extras.CreatedHeight
		}
	}

	// Persist the VTXO. A save failure signals a database or
	// schema inconsistency that must be surfaced.
	if h.cfg.VTXOStore != nil {
		saveErr := h.cfg.VTXOStore.SaveVTXO(ctx, desc)
		if saveErr != nil {
			h.emitReceived(ctx, "failed")

			return fn.Err[IncomingVTXOResp](
				fmt.Errorf(
					"save incoming VTXO %s: %w",
					outpoint.String(), saveErr,
				),
			)
		}
	}

	// The owned incoming VTXO is now persisted: count it as a
	// materialized receive. This is the authoritative success point —
	// the waved routing boundary cannot observe it because dispatch
	// to this handler is an async durable Tell.
	h.emitReceived(ctx, "materialized")

	// Notify the VTXO manager to spawn an actor.
	if h.cfg.VTXOManager != nil {
		tellErr := h.cfg.VTXOManager.Tell(
			ctx,
			&VTXOsMaterializedNotification{
				VTXOs: []*Descriptor{desc},
			},
		)
		if tellErr != nil {
			h.log.WarnS(ctx, "Failed to notify "+
				"VTXO manager", tellErr)
		}
	}

	h.log.InfoS(ctx, "Materialized incoming VTXO",
		slog.String("outpoint", outpoint.String()),
		slog.Int64("amount", int64(desc.Amount)),
		slog.String("round_id", evt.RoundId),
	)

	return fn.Ok[IncomingVTXOResp](nil)
}
