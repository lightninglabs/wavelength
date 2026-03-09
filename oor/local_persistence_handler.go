package oor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
)

var (
	// ErrIncomingRecipientNotOwned signals that a recipient output does not
	// belong to this wallet and should be ignored by incoming
	// materialization.
	ErrIncomingRecipientNotOwned = errors.New(
		"incoming recipient not owned by wallet",
	)
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

	// PackageStore persists finalized OOR package artifacts and local
	// outpoint bindings.
	PackageStore PackagePersistence

	// OperatorKey is the operator key used to reconstruct incoming VTXO
	// tapscripts.
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
	// received OOR VTXOs are actively monitored.
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
	case *MarkInputsSpentRequest:
		return h.handleMarkInputsSpent(ctx, msg)

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

// handleMarkInputsSpent updates local VTXO state for outgoing transfer inputs.
func (h *LocalPersistenceOutboxHandler) handleMarkInputsSpent(
	ctx context.Context, msg *MarkInputsSpentRequest) ([]Event, error) {

	if h.Store == nil {
		return nil, fmt.Errorf("vtxo store must be provided")
	}

	if len(msg.Outpoints) == 0 {
		return nil, fmt.Errorf("outpoints must be provided")
	}

	log.InfoS(ctx, "Marking VTXO inputs as spent",
		slog.Int("num_outpoints", len(msg.Outpoints)))

	for i := range msg.Outpoints {
		err := h.Store.UpdateVTXOStatus(
			ctx, msg.Outpoints[i], vtxo.VTXOStatusSpent,
		)
		if err != nil {
			return nil, err
		}

		log.DebugS(ctx, "Marked VTXO input spent",
			slog.String("outpoint", msg.Outpoints[i].String()))
	}

	return []Event{&InputsMarkedSpentEvent{}}, nil
}

// handleMaterializeIncoming persists recipient VTXOs for an incoming transfer
// before the receive FSM acknowledges the transfer to the server.
func (h *LocalPersistenceOutboxHandler) handleMaterializeIncoming(
	ctx context.Context,
	msg *MaterializeIncomingVTXOsRequest) ([]Event, error) {

	if h.Store == nil {
		return nil, fmt.Errorf("vtxo store must be provided")
	}

	if h.OperatorKey == nil {
		return nil, fmt.Errorf("operator key must be provided")
	}

	if h.ResolveIncomingClientKey == nil {
		return nil, fmt.Errorf(
			"incoming client key resolver must be provided",
		)
	}

	if h.ResolveIncomingMetadata == nil {
		return nil, fmt.Errorf(
			"incoming metadata resolver must be provided",
		)
	}

	if h.NotifyIncomingVTXOs == nil {
		return nil, fmt.Errorf(
			"incoming VTXO notifier must be provided",
		)
	}

	if len(msg.Recipients) == 0 {
		return nil, fmt.Errorf("incoming recipients must be provided")
	}

	log.InfoS(ctx, "Materializing incoming VTXOs",
		slog.String("session_id", msg.SessionID.String()),
		slog.Int("num_recipients", len(msg.Recipients)))

	ownedRecipients := 0
	materializedVTXOs := make([]*vtxo.Descriptor, 0, len(msg.Recipients))
	sessionIDHash := chainhash.Hash(msg.SessionID)

	if h.PackageStore != nil {
		err := h.PackageStore.UpsertPackage(ctx,
			PackageDirectionIncoming, sessionIDHash,
			msg.ArkPSBT, msg.FinalCheckpointPSBTs,
		)
		if err != nil {
			return nil, err
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

		metadata, err := h.ResolveIncomingMetadata(
			ctx, msg.SessionID, recipient, msg.ArkPSBT,
			msg.FinalCheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}

		desc, err := BuildIncomingVTXODescriptor(msg.ArkPSBT,
			IncomingVTXOConfig{
				OutputIndex: recipient.OutputIndex,
				ClientKey:   clientKey,
				OperatorKey: h.OperatorKey,
				ExitDelay:   h.ExitDelay,
				Metadata:    metadata,
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

	log.InfoS(ctx, "Incoming VTXOs materialized",
		slog.String("session_id", msg.SessionID.String()),
		slog.Int("owned_recipients", ownedRecipients),
		slog.Int("materialized_vtxos", len(materializedVTXOs)))

	err := h.NotifyIncomingVTXOs(ctx, materializedVTXOs)
	if err != nil {
		return nil, err
	}

	return []Event{&IncomingHandledEvent{}}, nil
}

// handleIncomingAck forwards the ack request to the transport boundary (if
// configured) and emits IncomingAckSentEvent on success.
func (h *LocalPersistenceOutboxHandler) handleIncomingAck(
	ctx context.Context, sessionID SessionID,
	msg *SendIncomingAckRequest) ([]Event, error) {

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
