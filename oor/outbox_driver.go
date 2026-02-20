package oor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
	clientoor "github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// InProcessOutboxDriver is a reusable outbox handler intended for unit tests
// and early in-process harnesses.
//
// It provides a minimal implementation of the OOR session outbox boundary:
//
//   - Locking, signing, and finalize steps are stubbed as unconditional success
//     events so the FSM can advance.
//   - Submit/finalize validation uses the shared darepo-client lib/tx/oor
//     primitives so tests exercise real v0 package rules.
//
// The purpose of this driver is to keep tests honest without requiring wallet,
// database, or VTXO store integrations yet.
type InProcessOutboxDriver struct {
	mu sync.Mutex

	seen []string

	locker vtxo.Locker

	store vtxo.Store

	sessionStore SessionStore

	recipientEvents RecipientEventStore

	coSigner CheckpointCoSigner

	operatorKey keychain.KeyDescriptor

	sessionExpiry time.Duration
}

// DriverCfg configures the in-process outbox driver.
type DriverCfg struct {
	// Locker applies input locks during submit handling.
	Locker vtxo.Locker

	// Store marks inputs in-flight/spent during finalize.
	Store vtxo.Store

	// SessionStore persists point-of-no-return/finalized session snapshots.
	SessionStore SessionStore

	// RecipientEvents persists recipient-notification cursors and payloads.
	RecipientEvents RecipientEventStore

	// CoSigner customizes checkpoint co-signing strategy for tests.
	CoSigner CheckpointCoSigner

	// OperatorSigner is used by the default co-signer when CoSigner is nil.
	OperatorSigner input.Signer

	// OperatorKey is the operator key descriptor used for co-signing.
	OperatorKey keychain.KeyDescriptor

	// SessionExpiry is the optional lock/session lease duration.
	SessionExpiry time.Duration
}

// CheckpointCoSigner defines how operator signatures are attached to
// checkpoint PSBTs.
type CheckpointCoSigner interface {
	// CoSignCheckpoints adds operator signature material to the checkpoint
	// PSBTs.
	CoSignCheckpoints(operatorKey keychain.KeyDescriptor,
		descs []VTXOSigningDescriptor, checkpoints []*psbt.Packet) error
}

// NoopCoSigner leaves checkpoint PSBTs unchanged.
//
// Tests can inject this signer when they want to exercise FSM and durability
// behavior without coupling to signature plumbing.
type NoopCoSigner struct{}

// CoSignCheckpoints is a no-op implementation for tests.
func (NoopCoSigner) CoSignCheckpoints(_ keychain.KeyDescriptor,
	_ []VTXOSigningDescriptor, _ []*psbt.Packet) error {

	return nil
}

// psbtCoSigner signs checkpoint PSBTs using a configured operator signer.
type psbtCoSigner struct {
	signer input.Signer
}

// CoSignCheckpoints signs checkpoint PSBTs using the package helper.
func (c *psbtCoSigner) CoSignCheckpoints(operatorKey keychain.KeyDescriptor,
	descs []VTXOSigningDescriptor, checkpoints []*psbt.Packet) error {

	return CoSignCheckpointPSBTs(c.signer, operatorKey, descs, checkpoints)
}

// defaultOutboxSessionExpiry is the lease duration used when a locker supports
// expiries.
const defaultOutboxSessionExpiry = 30 * time.Minute

// NewDriver creates a new in-process outbox driver.
func NewDriver(cfg DriverCfg) *InProcessOutboxDriver {
	sessionExpiry := cfg.SessionExpiry
	if sessionExpiry == 0 {
		sessionExpiry = defaultOutboxSessionExpiry
	}

	coSigner := cfg.CoSigner
	switch {
	case coSigner != nil:
		// Use the explicitly provided strategy.

	case cfg.OperatorSigner != nil:
		coSigner = &psbtCoSigner{
			signer: cfg.OperatorSigner,
		}

	default:
		// Default to a no-op strategy when no signer is provided so
		// tests can focus on FSM/durability behavior.
		coSigner = NoopCoSigner{}
	}

	return &InProcessOutboxDriver{
		seen:            make([]string, 0),
		locker:          cfg.Locker,
		store:           cfg.Store,
		sessionStore:    cfg.SessionStore,
		recipientEvents: cfg.RecipientEvents,
		coSigner:        coSigner,
		operatorKey:     cfg.OperatorKey,
		sessionExpiry:   sessionExpiry,
	}
}

// SeenOutboxTypes returns a snapshot of outbox types observed so far.
func (d *InProcessOutboxDriver) SeenOutboxTypes() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]string, len(d.seen))
	copy(out, d.seen)

	return out
}

// Handle executes the outbox request and returns follow-up events.
func (d *InProcessOutboxDriver) Handle(ctx context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

	_ = ctx

	// Track the outbox types we executed so tests can assert that specific
	// transitions emitted the expected side effects in the expected order.
	d.mu.Lock()
	d.seen = append(d.seen, outbox.OutboxType())
	d.mu.Unlock()

	switch msg := outbox.(type) {
	case *LockInputsReq:
		return d.handleLockInputs(ctx, sessionID, msg)

	case *ValidateSubmitReq:
		return d.handleValidateSubmit(msg)

	case *CoSignReq:
		return d.handleCoSign(ctx, sessionID, msg)

	case *ValidateFinalizeReq:
		return d.handleValidateFinalize(msg)

	case *FinalizeReq:
		return d.handleFinalize(ctx, sessionID, msg)

	case *NotifyRecipientsReq:
		return d.handleNotifyRecipients(ctx, sessionID, msg)

	case *UnlockInputsReq:
		return d.handleUnlockInputs(ctx, sessionID, msg)

	default:
		return nil, fmt.Errorf(
			"unsupported outbox event type: %T", outbox,
		)
	}
}

var _ OutboxHandler = (*InProcessOutboxDriver)(nil)

// handleLockInputs applies a lock request and emits either a success or
// failure inbox event.
func (d *InProcessOutboxDriver) handleLockInputs(ctx context.Context,
	sessionID SessionID, msg *LockInputsReq) ([]Event, error) {

	if d.locker != nil {
		owner := vtxo.OORLockOwner(sessionID.String())
		if leaseLocker, ok := d.locker.(vtxo.LeaseLocker); ok {
			expiresAt := time.Now().Add(d.sessionExpiry)
			err := leaseLocker.LockManyWithExpiry(
				ctx, msg.Inputs, owner, expiresAt,
			)
			if err != nil {
				return []Event{
					&InputsLockFailedEvent{
						Reason: err.Error(),
					},
				}, nil
			}
		} else {
			err := d.locker.LockMany(ctx, msg.Inputs, owner)
			if err != nil {
				return []Event{
					&InputsLockFailedEvent{
						Reason: err.Error(),
					},
				}, nil
			}
		}
	}

	return []Event{
		&InputsLockSucceededEvent{},
	}, nil
}

// handleValidateSubmit validates the submit package and returns an inbox event
// indicating success/failure.
func (d *InProcessOutboxDriver) handleValidateSubmit(
	msg *ValidateSubmitReq) ([]Event, error) {

	validated, err := oorlib.ValidateSubmitPackage(
		msg.ArkPSBT, msg.CheckpointPSBTs,
	)
	if err != nil {
		return []Event{
			&SubmitFailedEvent{
				Reason: err.Error(),
			},
		}, nil
	}

	return []Event{
		&SubmitValidatedEvent{
			ArkTxid: validated.ArkTxid,
		},
	}, nil
}

// handleCoSign persists point-of-no-return state, optionally co-signs the
// checkpoint PSBTs, and returns an inbox event for the FSM.
func (d *InProcessOutboxDriver) handleCoSign(ctx context.Context,
	sessionID SessionID, msg *CoSignReq) ([]Event, error) {

	_ = ctx

	err := d.coSigner.CoSignCheckpoints(
		d.operatorKey, msg.VTXOSigningDescriptors, msg.CheckpointPSBTs,
	)
	if err != nil {
		return []Event{
			&SignFailedEvent{
				Reason: err.Error(),
			},
		}, nil
	}

	owner := vtxo.OORLockOwner(sessionID.String())

	// When both stores are configured, require one atomic path for
	// persisting CoSigned state and marking inputs in-flight.
	// Separate writes leave a crash window between operations.
	if d.sessionStore != nil && d.store != nil {
		atomicStore, ok := d.sessionStore.(CoSignedAtomicStore)
		if !ok {
			return []Event{
				&SignFailedEvent{
					Reason: "session store must " +
						"implement " +
						"CoSignedAtomicStore " +
						"when VTXO store is configured",
				},
			}, nil
		}

		err := atomicStore.UpsertCoSignedAndMarkInFlight(
			ctx, sessionID, msg.Inputs, msg.ArkPSBT,
			msg.CheckpointPSBTs,
			time.Now().Add(d.sessionExpiry),
			owner,
		)
		if err != nil {
			return []Event{
				&SignFailedEvent{
					Reason: err.Error(),
				},
			}, nil
		}

		return []Event{
			&OperatorSignedEvent{},
		}, nil
	}

	if d.sessionStore != nil {
		err := d.sessionStore.UpsertCoSigned(ctx, sessionID,
			msg.Inputs, msg.ArkPSBT,
			msg.CheckpointPSBTs,
			time.Now().Add(d.sessionExpiry),
		)
		if err != nil {
			return []Event{
				&SignFailedEvent{
					Reason: err.Error(),
				},
			}, nil
		}
	}

	if d.store != nil {
		err := d.store.MarkInFlight(ctx, msg.Inputs, owner)
		if err != nil {
			return []Event{
				&SignFailedEvent{
					Reason: err.Error(),
				},
			}, nil
		}
	}

	return []Event{
		&OperatorSignedEvent{},
	}, nil
}

// handleValidateFinalize validates the finalize package and returns an inbox
// event indicating success/failure.
func (d *InProcessOutboxDriver) handleValidateFinalize(
	msg *ValidateFinalizeReq) ([]Event, error) {

	ark := msg.ArkPSBT
	if ark == nil {
		return nil, fmt.Errorf("ark psbt must be provided")
	}

	err := oorlib.ValidateFinalizePackage(
		ark, msg.FinalCheckpointPSBTs,
	)
	if err != nil {
		return []Event{
			&FinalizeFailedEvent{
				Reason: err.Error(),
			},
		}, nil
	}

	return []Event{
		&FinalizeValidatedEvent{},
	}, nil
}

// handleFinalize applies the finalized transfer to the VTXO set and persists
// the session's terminal state.
func (d *InProcessOutboxDriver) handleFinalize(ctx context.Context,
	sessionID SessionID, msg *FinalizeReq) ([]Event, error) {

	if len(msg.FinalCheckpointPSBTs) == 0 {
		return nil, fmt.Errorf("final checkpoints must be provided")
	}

	err := d.finalizeVTXOSet(ctx, msg)
	if err != nil {
		return nil, err
	}

	if d.sessionStore != nil {
		err := d.sessionStore.ApplyFinalize(
			ctx, sessionID, msg.FinalCheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}
	}

	return []Event{
		&FinalizeSucceededEvent{},
	}, nil
}

// finalizeVTXOSet marks inputs spent and materializes Ark tx outputs as new
// VTXOs in the in-memory store (v0 behavior for tests).
func (d *InProcessOutboxDriver) finalizeVTXOSet(ctx context.Context,
	msg *FinalizeReq) error {

	if d.store == nil {
		return nil
	}

	ark := msg.ArkPSBT
	if ark == nil || ark.UnsignedTx == nil {
		return fmt.Errorf("ark psbt must be provided")
	}

	// NOTE: MarkSpent and Create are separate calls here because the
	// in-process driver uses an in-memory store for tests. The production
	// DB path applies VTXO set mutations atomically within the session
	// store's finalize transaction.
	err := d.store.MarkSpent(ctx, msg.Inputs)
	if err != nil {
		return err
	}

	tx := ark.UnsignedTx
	arkTxid := tx.TxHash()

	outs := tx.TxOut
	if len(outs) == 0 {
		return fmt.Errorf("ark tx must have outputs")
	}

	// Materialize the non-anchor Ark outputs into the in-memory VTXO set.
	//
	// This is test-only behavior. It allows subsequent OOR sessions and
	// unit tests to treat recipient outputs as spendable VTXOs without
	// needing full rounds machinery or a durable VTXO set yet.
	//
	// We intentionally skip the last output, which is the P2A anchor.
	for i := 0; i < len(outs)-1; i++ {
		out := outs[i]
		err := d.store.Create(ctx, &vtxo.Record{
			Outpoint: wire.OutPoint{
				Hash:  arkTxid,
				Index: uint32(i),
			},
			Value:    out.Value,
			PkScript: out.PkScript,
			Status:   vtxo.StatusLive,
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// handleNotifyRecipients appends durable recipient events for the finalized
// Ark transaction and returns an FSM event indicating success or failure.
func (d *InProcessOutboxDriver) handleNotifyRecipients(ctx context.Context,
	sessionID SessionID,
	msg *NotifyRecipientsReq) ([]Event, error) {

	if d.recipientEvents == nil {
		// Mark the session as fully notified even when there is no
		// recipient event store, completing the awaiting_notify →
		// finalized DB transition.
		if d.sessionStore != nil {
			err := d.sessionStore.MarkNotified(ctx, sessionID)
			if err != nil {
				return []Event{
					&NotifyRecipientsFailedEvent{
						Reason: err.Error(),
					},
				}, nil
			}
		}

		return []Event{
			&NotifyRecipientsSucceededEvent{},
		}, nil
	}

	ark := msg.ArkPSBT
	if ark == nil {
		return []Event{
			&NotifyRecipientsFailedEvent{
				Reason: "ark psbt must be provided",
			},
		}, nil
	}

	recipients, err := clientoor.ExtractArkRecipients(ark)
	if err != nil {
		return []Event{
			&NotifyRecipientsFailedEvent{
				Reason: err.Error(),
			},
		}, nil
	}

	err = d.recipientEvents.AppendRecipientEvents(
		ctx, sessionID, ark, recipients,
	)
	if err != nil {
		return []Event{
			&NotifyRecipientsFailedEvent{
				Reason: err.Error(),
			},
		}, nil
	}

	// Mark the session as fully notified, completing the
	// awaiting_notify → finalized DB transition.
	if d.sessionStore != nil {
		err := d.sessionStore.MarkNotified(ctx, sessionID)
		if err != nil {
			return []Event{
				&NotifyRecipientsFailedEvent{
					Reason: err.Error(),
				},
			}, nil
		}
	}

	return []Event{
		&NotifyRecipientsSucceededEvent{},
	}, nil
}

// handleUnlockInputs unlocks inputs if a locker is configured. This is only
// used before point-of-no-return.
func (d *InProcessOutboxDriver) handleUnlockInputs(ctx context.Context,
	sessionID SessionID,
	msg *UnlockInputsReq) ([]Event, error) {

	if d.locker != nil {
		owner := vtxo.OORLockOwner(sessionID.String())
		err := d.locker.UnlockMany(ctx, msg.Inputs, owner)
		if err != nil {
			return nil, err
		}
	}

	return nil, nil
}
