package vtxo

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"time"

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
	"github.com/lightningnetwork/lnd/tlv"
	"google.golang.org/protobuf/proto"
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

// IncomingVTXOStore persists and reloads materialized descriptors. Reloading
// makes durable redelivery idempotent across the save-before-ack crash window.
type IncomingVTXOStore interface {
	VTXOSaver

	GetVTXO(ctx context.Context,
		outpoint wire.OutPoint) (*Descriptor, error)
}

// IncomingVTXOMsg wraps an IncomingVTXOEvent for the handler actor.
type IncomingVTXOMsg struct {
	actor.BaseMessage
	Event *arkrpc.IncomingVTXOEvent
}

// MessageType returns a human-readable message identifier.
func (m *IncomingVTXOMsg) MessageType() string {
	return fmt.Sprintf("IncomingVTXOMsg(event_id=%d)", m.Event.GetEventId())
}

const (
	incomingVTXOMsgTLVType   tlv.Type = 0
	incomingVTXORedriveDelay          = 30 * time.Second
)

// TLVType returns the durable-mailbox type identifier for an incoming event.
func (m *IncomingVTXOMsg) TLVType() tlv.Type {
	return incomingVTXOMsgTLVType
}

// Encode serializes the pushed protobuf for durable redelivery.
func (m *IncomingVTXOMsg) Encode(w io.Writer) error {
	if m.Event == nil {
		return fmt.Errorf("incoming VTXO event must be provided")
	}

	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(m.Event)
	if err != nil {
		return fmt.Errorf("marshal incoming VTXO event: %w", err)
	}

	_, err = w.Write(raw)

	return err
}

// Decode restores a pushed protobuf from the durable mailbox.
func (m *IncomingVTXOMsg) Decode(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	event := &arkrpc.IncomingVTXOEvent{}
	if err := proto.Unmarshal(raw, event); err != nil {
		return fmt.Errorf("unmarshal incoming VTXO event: %w", err)
	}
	m.Event = event

	return nil
}

// IncomingVTXOResp is the handler's response type.
type IncomingVTXOResp = any

const incomingVTXOServiceKeyName = "incoming-vtxo-handler"

// IncomingVTXOServiceKey returns the well-known service key for the
// incoming VTXO handler actor.
func IncomingVTXOServiceKey() actor.ServiceKey[
	*IncomingVTXOMsg, IncomingVTXOResp] {

	return actor.NewServiceKey[*IncomingVTXOMsg, IncomingVTXOResp](
		incomingVTXOServiceKeyName,
	)
}

// NewIncomingVTXOCodec builds the codec used by the durable incoming-event
// mailbox.
func NewIncomingVTXOCodec() *actor.MessageCodec {
	codec := actor.NewMessageCodec()
	codec.MustRegister(incomingVTXOMsgTLVType, func() actor.TLVMessage {
		return &IncomingVTXOMsg{}
	})

	return codec
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

	// BatchExpiry is derived from locally confirmed commitment
	// transactions and the sweep policy authenticated by Ancestry.
	BatchExpiry int32
}

// IncomingAncestryFetcher resolves the per-VTXO metadata the
// unilateral-exit unroll path needs to construct a CPFP child:
// ancestry slice plus the created-height anchor. Invoked
// synchronously inside IncomingVTXOHandler.Receive after the receive
// script is matched and the descriptor is built, so the persisted
// descriptor carries full lineage from the first save.
//
// The handler routes per-script signing via clientKey so the
// implementation can issue an indexer ListVTXOsByScripts query under the
// owner's proof-of-control and authenticate the returned expiry evidence.
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
	VTXOStore IncomingVTXOStore

	// VTXOManager is a tell-only reference to the VTXO manager
	// actor, used to notify it of newly materialized VTXOs.
	VTXOManager actor.TellOnlyRef[ManagerMsg]

	// AncestryFetcher resolves the authenticated expiry and round commit
	// tree fragments required to accept and unilaterally exit each incoming
	// VTXO. A nil fetcher rejects relevant events for durable retry.
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

// prepare validates an incoming event and builds its authenticated descriptor.
// It performs no persistence writes or manager notifications, so a durable
// caller can keep its slow proof I/O outside the acknowledgement transaction.
func (h *IncomingVTXOHandler) prepare(ctx context.Context,
	msg *IncomingVTXOMsg) fn.Result[*Descriptor] {

	evt := msg.Event
	if evt == nil {
		return fn.Ok[*Descriptor](nil)
	}

	// We only handle VTXO_CREATED events. Log unexpected types
	// so we notice if the server starts sending new event kinds.
	if evt.Type != arkrpc.VTXOEventType_VTXO_EVENT_TYPE_CREATED {
		h.log.DebugS(ctx, "Ignoring non-CREATED VTXO event",
			slog.Int("type", int(evt.Type)),
		)

		return fn.Ok[*Descriptor](nil)
	}

	op := evt.GetOutpoint()
	if op == nil || len(op.Txid) != 32 {
		h.log.WarnS(ctx, "IncomingVTXOEvent has invalid "+
			"or missing outpoint", nil)

		return fn.Ok[*Descriptor](nil)
	}

	pkScript := evt.GetPkScript()
	if len(pkScript) == 0 {
		h.log.WarnS(ctx, "IncomingVTXOEvent has empty "+
			"pkScript", nil)

		return fn.Ok[*Descriptor](nil)
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
		return fn.Ok[*Descriptor](nil)
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
			return fn.Ok[*Descriptor](nil)
		}

		h.log.WarnS(ctx, "Failed to look up incoming VTXO script",
			err,
			slog.String("outpoint", outpoint.String()),
		)

		return fn.Err[*Descriptor](
			fmt.Errorf("lookup owned receive script: %w", err),
		)
	}

	if rec.ClientKey.PubKey == nil {
		h.log.WarnS(ctx, "Owned receive script has nil "+
			"client pubkey", nil,
			slog.String("outpoint", outpoint.String()))

		return fn.Ok[*Descriptor](nil)
	}

	// Reject server-provided values that would overflow int64
	// when cast to btcutil.Amount.
	if evt.ValueSat > uint64(math.MaxInt64) ||
		evt.ValueSat > uint64(btcutil.MaxSatoshi) {

		h.log.WarnS(ctx, "Incoming VTXO value exceeds "+
			"maximum", nil,
			slog.String("outpoint", outpoint.String()),
			slog.Uint64("value_sat", evt.ValueSat))

		return fn.Ok[*Descriptor](nil)
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

		return fn.Ok[*Descriptor](nil)
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

		return fn.Ok[*Descriptor](nil)
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
		BatchExpiry:    0,
		RelativeExpiry: exitDelay,
		Status:         VTXOStatusLive,
	}

	// The indexer push is intentionally only a notification. Resolve the
	// ancestry and chain-authenticated expiry before the first write.
	// Returning an error keeps the durable event pending for retry instead
	// of accepting a new live VTXO with unauthenticated safety metadata.
	if h.cfg.AncestryFetcher == nil {
		h.log.WarnS(ctx, "Cannot authenticate incoming VTXO expiry",
			nil,
			slog.String("outpoint", outpoint.String()),
		)

		return fn.Err[*Descriptor](
			fmt.Errorf(
				"authenticate incoming VTXO %s: ancestry "+
					"fetcher not configured",
				outpoint.String(),
			),
		)
	}
	extras, err := h.cfg.AncestryFetcher(
		ctx, outpoint, pkScript, rec.ClientKey,
	)
	if err != nil {
		h.log.WarnS(ctx, "Failed to authenticate incoming VTXO expiry",
			err,
			slog.String("outpoint", outpoint.String()),
		)
		if errors.Is(err, ErrInvalidBatchExpiryEvidence) {
			h.emitReceived(ctx, "failed")
		}

		return fn.Err[*Descriptor](
			fmt.Errorf(
				"authenticate incoming VTXO %s: %w",
				outpoint.String(), err,
			),
		)
	}
	if extras.BatchExpiry <= 0 {
		err := fmt.Errorf("%w: derived batch expiry must be positive",
			ErrInvalidBatchExpiryEvidence)
		h.log.WarnS(ctx, "Failed to authenticate incoming VTXO expiry",
			err,
			slog.String("outpoint", outpoint.String()),
		)
		h.emitReceived(ctx, "failed")

		return fn.Err[*Descriptor](
			fmt.Errorf(
				"authenticate incoming VTXO %s: %w",
				outpoint.String(), err,
			),
		)
	}
	desc.Ancestry = extras.Ancestry
	desc.CreatedHeight = extras.CreatedHeight
	desc.BatchExpiry = extras.BatchExpiry

	return fn.Ok(desc)
}

// persist stores the authenticated descriptor, then reloads the canonical
// row. This closes the save-before-ack crash window without reviving a VTXO
// whose status changed before redelivery.
func (h *IncomingVTXOHandler) persist(ctx context.Context, desc *Descriptor) (
	*Descriptor, error) {

	if h.cfg.VTXOStore == nil {
		return desc, nil
	}

	saveErr := h.cfg.VTXOStore.SaveVTXO(ctx, desc)
	existing, loadErr := h.cfg.VTXOStore.GetVTXO(ctx, desc.Outpoint)
	if loadErr == nil && sameIncomingVTXO(existing, desc) {
		return existing, nil
	}
	if saveErr != nil {
		return nil, fmt.Errorf("save incoming VTXO %s: %w",
			desc.Outpoint.String(), saveErr)
	}
	if loadErr != nil {
		return nil, fmt.Errorf("reload incoming VTXO %s: %w",
			desc.Outpoint.String(), loadErr)
	}

	return nil, fmt.Errorf("reloaded incoming VTXO %s does not match event",
		desc.Outpoint.String())
}

// sameIncomingVTXO proves persistence found the row for this event. The
// immutable script, round, chain anchor, and
// authenticated expiry must all match before the duplicate is accepted.
func sameIncomingVTXO(existing, incoming *Descriptor) bool {
	if existing == nil || incoming == nil {
		return false
	}

	return existing.Outpoint == incoming.Outpoint &&
		existing.Amount == incoming.Amount &&
		bytes.Equal(existing.PolicyTemplate, incoming.PolicyTemplate) &&
		bytes.Equal(existing.PkScript, incoming.PkScript) &&
		existing.RoundID == incoming.RoundID &&
		existing.CommitmentTxID == incoming.CommitmentTxID &&
		existing.BatchExpiry == incoming.BatchExpiry &&
		existing.RelativeExpiry == incoming.RelativeExpiry &&
		existing.CreatedHeight == incoming.CreatedHeight &&
		len(existing.Ancestry) > 0
}

// notifyMaterialized publishes best-effort effects after persistence commits.
func (h *IncomingVTXOHandler) notifyMaterialized(ctx context.Context,
	desc *Descriptor) {

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
		slog.String("outpoint", desc.Outpoint.String()),
		slog.Int64("amount", int64(desc.Amount)),
		slog.String("round_id", desc.RoundID),
	)
}

// Receive processes an incoming event outside a durable mailbox. Production
// uses incomingVTXODurableBehavior; this direct entry point remains useful to
// focused tests and callers that own their own retry boundary.
func (h *IncomingVTXOHandler) Receive(ctx context.Context,
	msg *IncomingVTXOMsg) fn.Result[IncomingVTXOResp] {

	desc, err := h.prepare(ctx, msg).Unpack()
	if err != nil {
		return fn.Err[IncomingVTXOResp](err)
	}
	if desc == nil {
		return fn.Ok[IncomingVTXOResp](nil)
	}

	persisted, err := h.persist(ctx, desc)
	if err != nil {
		h.log.WarnS(ctx, "Failed to persist incoming VTXO",
			err,
			slog.String("outpoint", desc.Outpoint.String()),
		)
		h.emitReceived(ctx, "failed")

		return fn.Err[IncomingVTXOResp](err)
	}
	if persisted.Status == VTXOStatusLive {
		h.notifyMaterialized(ctx, persisted)
	}

	return fn.Ok[IncomingVTXOResp](nil)
}

// incomingVTXODurableBehavior keeps slow indexer and chain I/O outside the
// delivery-store writer transaction. Persistence and manager notification are
// replay-safe, then one short Commit consumes the durable event. Transient
// failures are postponed without spending the mailbox attempt budget.
type incomingVTXODurableBehavior struct {
	handler *IncomingVTXOHandler
}

func (b *incomingVTXODurableBehavior) Receive(ctx context.Context,
	msg *IncomingVTXOMsg,
	ax actor.Exec[struct{}]) fn.Result[IncomingVTXOResp] {

	desc, err := b.handler.prepare(ctx, msg).Unpack()
	if err != nil {
		if errors.Is(err, ErrInvalidBatchExpiryEvidence) {
			return fn.Err[IncomingVTXOResp](err)
		}

		return fn.Err[IncomingVTXOResp](
			fmt.Errorf(
				"%w: %w", err,
				actor.Postpone(incomingVTXORedriveDelay),
			),
		)
	}

	var persisted *Descriptor
	if desc != nil {
		persisted, err = b.handler.persist(ctx, desc)
		if err != nil {
			b.handler.log.WarnS(
				ctx,
				"Failed to persist incoming VTXO",
				err,
				slog.String("outpoint", desc.Outpoint.String()),
			)
			postpone := actor.Postpone(incomingVTXORedriveDelay)

			return fn.Err[IncomingVTXOResp](
				fmt.Errorf("%w: %w", err, postpone),
			)
		}

		if persisted.Status == VTXOStatusLive {
			b.handler.notifyMaterialized(ctx, persisted)
		}
	}

	err = ax.Commit(ctx, func(context.Context, struct{}) error {
		return nil
	})
	if err != nil {
		if errors.Is(err, actor.ErrLeaseLost) {
			return fn.Err[IncomingVTXOResp](err)
		}
		b.handler.log.WarnS(ctx, "Failed to acknowledge incoming VTXO",
			err,
		)

		return fn.Err[IncomingVTXOResp](
			fmt.Errorf(
				"%w: %w", err,
				actor.Postpone(incomingVTXORedriveDelay),
			),
		)
	}

	return fn.Ok[IncomingVTXOResp](nil)
}

// NewIncomingVTXODurableActor creates the crash-safe incoming-event consumer.
// The caller owns Start, Stop, and receptionist registration.
func NewIncomingVTXODurableActor(cfg IncomingVTXOHandlerConfig,
	store actor.DeliveryStore) (*actor.DurableActor[
	*IncomingVTXOMsg, IncomingVTXOResp], error) {

	if store == nil {
		return nil, fmt.Errorf("delivery store must be provided")
	}

	behavior := &incomingVTXODurableBehavior{
		handler: NewIncomingVTXOHandler(cfg),
	}
	durableCfg := actor.DefaultDurableTxActorConfig[
		*IncomingVTXOMsg, IncomingVTXOResp, struct{},
	](
		incomingVTXOServiceKeyName, behavior,
		func(context.Context, actor.DeliveryStore) struct{} {
			return struct{}{}
		},
		store, NewIncomingVTXOCodec(),
	)
	durableCfg.Log = cfg.Log
	durableCfg.TellRetryPolicy = func(err error, attempts int) (bool,
		time.Duration) {

		if errors.Is(err, ErrInvalidBatchExpiryEvidence) {
			return false, 0
		}

		return actor.DefaultTellRetryPolicy(err, attempts)
	}

	return actor.NewDurableActor(durableCfg).Unpack()
}
