package oor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	libtypes "github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
)

var (
	// ErrIncomingRecipientNotOwned signals that a recipient output does not
	// belong to this wallet and should be ignored by incoming
	// materialization.
	ErrIncomingRecipientNotOwned = errors.New("incoming recipient not " +
		"owned by wallet")
)

// IsIncomingRecipientNotOwned reports whether an error means the recipient
// output is not owned by this wallet.
//
// Callers use this to safely skip non-owned recipients while continuing
// materialization for owned outputs in the same incoming transfer.
func IsIncomingRecipientNotOwned(err error) bool {
	return errors.Is(err, ErrIncomingRecipientNotOwned)
}

// IncomingClientKeyResolver resolves the local client key for a recipient
// output being materialized from an incoming transfer.
type IncomingClientKeyResolver func(ctx context.Context,
	recipient ArkRecipientOutput) (keychain.KeyDescriptor, error)

// IncomingMetadataResolver resolves authoritative lineage and expiry metadata
// for a recipient output being materialized from an incoming transfer.
type IncomingMetadataResolver func(ctx context.Context, sessionID SessionID,
	recipient ArkRecipientOutput, ark *psbt.Packet,
	finalCheckpoints []*psbt.Packet) (IncomingVTXOMetadata, error)

// IncomingMetadataRecipientFilter filters an incoming Ark package down to the
// recipient outputs controlled by the local wallet.
type IncomingMetadataRecipientFilter interface {
	// FilterIncomingMetadataRecipients returns only locally owned
	// recipients from the provided incoming Ark package recipients.
	FilterIncomingMetadataRecipients(ctx context.Context,
		recipients []ArkRecipientOutput) ([]ArkRecipientOutput, error)
}

// SpendCompleter enqueues OOR spend completion through the VTXO manager so
// each VTXO actor transitions to SpentState via its own FSM. Implementations
// must return only after the manager has either durably completed the spend or
// reported an error so the OOR actor does not checkpoint Completed ahead of
// local VTXO persistence.
type SpendCompleter func(ctx context.Context,
	outpoints []wire.OutPoint) error

// SpendReleaser releases the spend reservation on input VTXOs through
// the VTXO manager, returning each from SpendingState to LiveState.
// Invoked when an outgoing session fails terminally before the point of
// no return (the server never locked the inputs), so the funds become
// spendable again without waiting for a restart sweep.
type SpendReleaser func(ctx context.Context,
	outpoints []wire.OutPoint) error

// IncomingVTXONotifier is called after incoming VTXOs are durably
// materialized, allowing callers to spawn/manage VTXO actors for expiry and
// spend monitoring.
type IncomingVTXONotifier func(ctx context.Context,
	vtxos []*vtxo.Descriptor) error

// LocalPersistenceOutboxHandler implements the persistence-related outbox
// requests emitted by the OOR FSM.
//
// This exists as a small convenience for callers: an application can wrap its
// transport/signing outbox implementation with this handler to get consistent,
// restart-safe local VTXO state updates.
//
// Incoming transfer materialization requires wallet-specific key and lineage
// resolution. Those remain explicit callouts via resolver callbacks on this
// handler.
type LocalPersistenceOutboxHandler struct {
	// Next is the delegate handler that executes non-persistence outbox
	// requests.
	Next OutboxHandler

	// Store is the VTXO store to update.
	Store vtxo.VTXOStore

	// BatchRegistrar registers authenticated lineage before synchronous
	// callers materialize incoming VTXOs. Durable actors register outside
	// their commit transaction before invoking this handler.
	BatchRegistrar BatchRegistrar

	// PackageStore persists finalized OOR package artifacts and local
	// outpoint bindings.
	PackageStore PackagePersistence

	// OperatorKey is the fallback operator key used to reconstruct incoming
	// VTXO tapscripts when older indexer metadata omits the per-VTXO key.
	OperatorKey *btcec.PublicKey

	// ExitDelay is the unilateral CSV delay for incoming VTXO descriptors.
	ExitDelay uint32

	// ResolveIncomingClientKey resolves the local client key for each
	// incoming recipient output.
	ResolveIncomingClientKey IncomingClientKeyResolver

	// ResolveIncomingMetadata resolves authoritative lineage/expiry
	// metadata for each incoming recipient output.
	ResolveIncomingMetadata IncomingMetadataResolver

	// NotifyIncomingVTXOs is invoked after incoming VTXOs are persisted.
	// Production wiring should use this to notify the VTXO manager so newly
	// received OOR VTXOs are actively monitored when the handler is used
	// outside the OOR durable actor.
	NotifyIncomingVTXOs IncomingVTXONotifier
}

// Handle executes one outbox request and emits follow-up FSM events.
//
// Persistence-related requests are handled locally, while unknown requests are
// delegated to Next when configured.
func (h *LocalPersistenceOutboxHandler) Handle(ctx context.Context,
	sessionID SessionID, outbox OutboxEvent) ([]Event, error) {

	_ = sessionID

	if h == nil {
		return nil, fmt.Errorf("handler must be provided")
	}

	switch msg := outbox.(type) {
	case *QueryIncomingMetadataRequest:
		return h.handleQueryIncomingMetadata(ctx, msg)

	case *MaterializeIncomingVTXOsRequest:
		return h.handleMaterializeIncoming(ctx, msg)

	case *SendIncomingAckRequest:
		return h.handleIncomingAck(ctx, sessionID, msg)

	default:
		if h.Next == nil {
			return nil, nil
		}

		return h.Next.Handle(ctx, sessionID, outbox)
	}
}

// handleMaterializeIncoming persists recipient VTXOs for an incoming transfer
// before the receive FSM acknowledges the transfer to the server.
func (h *LocalPersistenceOutboxHandler) handleMaterializeIncoming(
	ctx context.Context, msg *MaterializeIncomingVTXOsRequest) ([]Event,
	error) {

	err := h.validateMaterializeIncoming(ctx, msg)
	if err != nil {
		return nil, err
	}
	if !hasActorDBTx(ctx) {
		err := RegisterIncomingBatchEvidence(
			ctx, h.BatchRegistrar, msg.SessionID,
			msg.MetadataMatches,
		)
		if err != nil {
			return nil, err
		}
	}

	return h.materializeIncoming(ctx, msg, !hasActorDBTx(ctx))
}

// validateMaterializeIncoming verifies the dependencies needed to materialize
// an incoming transfer.
func (h *LocalPersistenceOutboxHandler) validateMaterializeIncoming(
	ctx context.Context, msg *MaterializeIncomingVTXOsRequest) error {

	if msg == nil {
		return fmt.Errorf("materialize request must be provided")
	}

	if h.Store == nil {
		return fmt.Errorf("vtxo store must be provided")
	}

	if h.ResolveIncomingClientKey == nil {
		return fmt.Errorf("incoming client key resolver must be " +
			"provided")
	}

	if h.NotifyIncomingVTXOs == nil && !hasActorDBTx(ctx) {
		return fmt.Errorf("incoming VTXO notifier must be provided")
	}

	if len(msg.Recipients) == 0 {
		return fmt.Errorf("incoming recipients must be provided")
	}

	if h.PackageStore != nil {
		root := packageArtifactForValidation(
			msg.SessionID, msg.ArkPSBT, msg.FinalCheckpointPSBTs,
		)
		err := validateIncomingPackageGraph(
			root, msg.AncestorPackages,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// handleQueryIncomingMetadata resolves authoritative metadata for owned
// recipients when the OOR actor is running without the durable serverconn
// request/response path.
func (h *LocalPersistenceOutboxHandler) handleQueryIncomingMetadata(
	ctx context.Context, msg *QueryIncomingMetadataRequest) ([]Event,
	error) {

	if msg == nil {
		return nil, fmt.Errorf("incoming metadata query must be " +
			"provided")
	}

	if h.ResolveIncomingClientKey == nil {
		return nil, fmt.Errorf("incoming client key resolver must be " +
			"provided")
	}

	if h.ResolveIncomingMetadata == nil {
		return nil, fmt.Errorf("incoming metadata resolver must be " +
			"provided")
	}

	matches := make([]IncomingMetadataMatch, 0, len(msg.Recipients))
	for i := range msg.Recipients {
		recipient := msg.Recipients[i]

		_, err := h.ResolveIncomingClientKey(ctx, recipient)
		if err != nil {
			if IsIncomingRecipientNotOwned(err) {
				continue
			}

			return nil, err
		}

		metadata, err := h.ResolveIncomingMetadata(
			ctx, msg.SessionID, recipient, msg.ArkPSBT,
			msg.FinalCheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}

		matches = append(matches, IncomingMetadataMatch{
			OutputIndex: recipient.OutputIndex,
			Metadata:    metadata,
		})
	}

	return []Event{&IncomingMetadataResolvedEvent{
		Matches: matches,
	}}, nil
}

// FilterIncomingMetadataRecipients returns only the incoming recipients owned
// by the local wallet. Durable metadata queries use this before asking the
// server/indexer to prove script ownership, because mixed OOR packages can
// contain recipient outputs belonging to other clients.
func (h *LocalPersistenceOutboxHandler) FilterIncomingMetadataRecipients(
	ctx context.Context, recipients []ArkRecipientOutput) (
	[]ArkRecipientOutput, error) {

	if h == nil {
		return nil, fmt.Errorf("handler must be provided")
	}

	if h.ResolveIncomingClientKey == nil {
		return nil, fmt.Errorf("incoming client key resolver must be " +
			"provided")
	}

	owned := make([]ArkRecipientOutput, 0, len(recipients))
	for i := range recipients {
		recipient := recipients[i]

		_, err := h.ResolveIncomingClientKey(ctx, recipient)
		if err != nil {
			if IsIncomingRecipientNotOwned(err) {
				continue
			}

			return nil, err
		}

		owned = append(owned, recipient)
	}

	return owned, nil
}

// materializeIncoming persists recipient VTXOs for an incoming transfer and
// optionally notifies the VTXO manager directly when the caller is not
// resuming the durable actor with a follow-up event.
func (h *LocalPersistenceOutboxHandler) materializeIncoming(ctx context.Context,
	msg *MaterializeIncomingVTXOsRequest, notifyIncoming bool) ([]Event,
	error) {

	logger(ctx).InfoS(ctx, "Materializing incoming VTXOs",
		slog.String("session_id", msg.SessionID.String()),
		slog.Int("num_recipients", len(msg.Recipients)),
	)

	ownedRecipients := 0
	materializedVTXOs := make([]*vtxo.Descriptor, 0, len(msg.Recipients))
	materializedOutpoints := make([]wire.OutPoint, 0, len(msg.Recipients))
	sessionIDHash := chainhash.Hash(msg.SessionID)
	metadataByOutput := make(
		map[uint32]IncomingVTXOMetadata, len(msg.MetadataMatches),
	)

	for i := range msg.MetadataMatches {
		match := msg.MetadataMatches[i]
		metadataByOutput[match.OutputIndex] = match.Metadata
	}

	if h.PackageStore != nil {
		err := h.persistIncomingAncestorPackages(
			ctx, msg.AncestorPackages,
		)
		if err != nil {
			return nil, err
		}

		err = h.PackageStore.UpsertPackage(
			ctx, PackageDirectionIncoming, sessionIDHash,
			msg.ArkPSBT, msg.FinalCheckpointPSBTs,
		)
		if err != nil {
			isDirectionConflict := errors.Is(
				err, libtypes.ErrOORPackageDirectionConflict,
			)
			if isDirectionConflict {
				logger(ctx).DebugS(
					ctx, "Reusing outgoing package for "+
						"incoming materialization",
					slog.String(
						"session_id",
						msg.SessionID.String(),
					),
				)
			} else {
				return nil, err
			}
		}
	}

	for i := range msg.Recipients {
		recipient := msg.Recipients[i]
		clientKey, err := h.ResolveIncomingClientKey(ctx, recipient)
		if err != nil {
			if IsIncomingRecipientNotOwned(err) {
				continue
			}

			return nil, err
		}

		ownedRecipients++

		metadata, ok := metadataByOutput[recipient.OutputIndex]

		// Legacy synchronous path for callers outside the durable
		// actor transaction (e.g. systest, non-actor consumers).
		// Production durable actors pre-resolve metadata via the
		// async QueryIncomingMetadataRequest path, so this branch
		// is never reached inside an actor DB tx.
		if !ok && h.ResolveIncomingMetadata != nil &&
			!hasActorDBTx(ctx) {

			metadata, err = h.ResolveIncomingMetadata(
				ctx, msg.SessionID, recipient, msg.ArkPSBT,
				msg.FinalCheckpointPSBTs,
			)
			if err != nil {
				return nil, err
			}

			ok = true
		}
		if !ok {
			return nil, NewRetryableOutboxError(
				fmt.Errorf("incoming metadata missing for "+
					"wallet-owned output %d",
					recipient.OutputIndex),
				defaultRetryDelay,
			)
		}

		logger(ctx).DebugS(ctx, "Resolved incoming metadata",
			slog.String("session_id", msg.SessionID.String()),
			slog.Int("output_index", int(recipient.OutputIndex)),
			slog.String("round_id", metadata.RoundID),
			slog.Int("ancestry_paths", len(metadata.Ancestry)),
			slog.Int("chain_depth", metadata.ChainDepth),
		)

		operatorKey := metadata.OperatorKey
		if operatorKey == nil {
			operatorKey = h.OperatorKey
		}
		if operatorKey == nil {
			return nil, fmt.Errorf("operator key missing for "+
				"incoming output %d", recipient.OutputIndex)
		}

		desc, err := BuildIncomingVTXODescriptor(msg.ArkPSBT,
			IncomingVTXOConfig{
				OutputIndex: recipient.OutputIndex,
				ClientKey:   clientKey,
				OperatorKey: operatorKey,
				ExitDelay:   h.ExitDelay,
				PolicyTemplate: recipient.
					VTXOPolicyTemplate,
				Metadata: metadata,
			},
		)
		if err != nil {
			return nil, err
		}

		err = h.Store.SaveVTXO(ctx, desc)
		if err != nil {
			// SaveVTXO may fail for duplicates on retry/restart
			// paths. Treat that case as idempotent only if the
			// already-persisted descriptor matches the
			// materialized recipient output.
			existing, getErr := h.Store.GetVTXO(ctx, desc.Outpoint)
			if getErr != nil || existing == nil {
				return nil, err
			}

			if existing.Amount != desc.Amount {
				return nil, err
			}

			if !bytes.Equal(existing.PkScript, desc.PkScript) {
				return nil, err
			}

			desc = existing
		}

		materializedVTXOs = append(materializedVTXOs, desc)
		materializedOutpoints = append(
			materializedOutpoints, desc.Outpoint,
		)

		if h.PackageStore != nil {
			err := h.PackageStore.UpsertBinding(
				ctx, desc.Outpoint, sessionIDHash,
				recipient.OutputIndex,
				PackageLinkKindCreatedOutput,
			)
			if err != nil {
				return nil, err
			}
		}
	}

	if ownedRecipients == 0 {
		return nil, fmt.Errorf("incoming transfer contains no " +
			"wallet-owned recipients")
	}

	logger(ctx).InfoS(ctx, "Incoming VTXOs materialized",
		slog.String("session_id", msg.SessionID.String()),
		slog.Int("owned_recipients", ownedRecipients),
		slog.Int("materialized_vtxos", len(materializedVTXOs)),
	)

	// When the durable actor will receive an
	// IncomingHandledEvent follow-up, defer notification to
	// that actor path so the manager only sees the
	// materialization once.
	if notifyIncoming {
		// Notify the VTXO manager so newly received OOR VTXOs are
		// actively monitored for expiry and spend.
		err := h.NotifyIncomingVTXOs(
			ctx, materializedVTXOs,
		)
		if err != nil {
			return nil, err
		}
	}

	return []Event{&IncomingHandledEvent{
		MaterializedVTXOs:     materializedVTXOs,
		MaterializedOutpoints: materializedOutpoints,
	}}, nil
}

// persistIncomingAncestorPackages stores chained OOR artifacts before the
// target package so unroll resolution can walk from the recipient package into
// prior OOR hops even when the intermediate VTXOs are not wallet-owned.
func (h *LocalPersistenceOutboxHandler) persistIncomingAncestorPackages(
	ctx context.Context, ancestors []PackageArtifact) error {

	if h == nil || h.PackageStore == nil {
		return nil
	}

	for i := range ancestors {
		ancestor := ancestors[i]
		sessionHash := chainhash.Hash(ancestor.SessionID)

		err := h.PackageStore.UpsertPackage(
			ctx, PackageDirectionIncoming, sessionHash,
			ancestor.ArkPSBT, ancestor.FinalCheckpointPSBTs,
		)
		if err == nil {
			continue
		}

		if errors.Is(err, libtypes.ErrOORPackageDirectionConflict) {
			logger(ctx).DebugS(
				ctx, "Reusing existing ancestor package",
				slog.String(
					"session_id",
					ancestor.SessionID.String(),
				),
			)

			continue
		}

		return fmt.Errorf("persist ancestor package %s: %w",
			ancestor.SessionID.String(), err)
	}

	return nil
}

// hasActorDBTx reports whether the current context is already scoped to a
// durable actor database transaction.
func hasActorDBTx(ctx context.Context) bool {
	_, ok := actor.TxFromContext(ctx)

	return ok
}

// handleIncomingAck forwards the ack request to the transport boundary (if
// configured) and emits IncomingAckSentEvent on success.
func (h *LocalPersistenceOutboxHandler) handleIncomingAck(ctx context.Context,
	sessionID SessionID, msg *SendIncomingAckRequest) ([]Event, error) {

	if msg == nil {
		return nil, fmt.Errorf("incoming ack request must be provided")
	}

	if h.Next == nil {
		return []Event{&IncomingAckSentEvent{}}, nil
	}

	followUps, err := h.Next.Handle(ctx, sessionID, msg)
	if err != nil {
		return nil, err
	}

	followUps = append(followUps, &IncomingAckSentEvent{})

	return followUps, nil
}

var _ OutboxHandler = (*LocalPersistenceOutboxHandler)(nil)
var _ IncomingMetadataRecipientFilter = (*LocalPersistenceOutboxHandler)(nil)
