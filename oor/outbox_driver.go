package oor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
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

	log btclog.Logger

	seen []string

	locker vtxo.Locker

	store vtxo.Store

	sessionStore SessionStore

	recipientEvents RecipientEventStore

	recipientNotifier RecipientNotifier

	coSigner       CheckpointCoSigner
	operatorSigner input.Signer

	operatorKey keychain.KeyDescriptor

	sessionExpiry time.Duration

	operatorPolicy SubmitOutputPolicy
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

	// RecipientNotifier receives best-effort notifications for each
	// finalized recipient output. The notifier is called after durable
	// recipient events are persisted, bridging OOR into the indexer
	// event stream for connected clients. May be nil.
	RecipientNotifier RecipientNotifier

	// Logger is used for outbox driver logging. When nil,
	// btclog.Disabled is used.
	Logger btclog.Logger

	// CoSigner customizes checkpoint co-signing strategy for tests.
	CoSigner CheckpointCoSigner

	// OperatorSigner is used by the default co-signer when CoSigner is nil.
	OperatorSigner input.Signer

	// OperatorKey is the operator key descriptor used for co-signing.
	OperatorKey keychain.KeyDescriptor

	// SessionExpiry is the optional lock/session lease duration.
	SessionExpiry time.Duration

	// OperatorPolicy provides optional submit output policy constraints.
	OperatorPolicy SubmitOutputPolicy
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

	log := cfg.Logger
	if log == nil {
		log = btclog.Disabled
	}

	return &InProcessOutboxDriver{
		log:               log,
		seen:              make([]string, 0),
		locker:            cfg.Locker,
		store:             cfg.Store,
		sessionStore:      cfg.SessionStore,
		recipientEvents:   cfg.RecipientEvents,
		recipientNotifier: cfg.RecipientNotifier,
		coSigner:          coSigner,
		operatorSigner:    cfg.OperatorSigner,
		operatorKey:       cfg.OperatorKey,
		sessionExpiry:     sessionExpiry,
		operatorPolicy:    cfg.OperatorPolicy,
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
		return d.handleValidateSubmit(ctx, msg)

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
				d.log.DebugS(ctx, "Input lock failed",
					btclog.Hex("session_id", sessionID[:]),
					slog.Int("num_inputs", len(msg.Inputs)),
					slog.String("reason", err.Error()))

				return []Event{
					&InputsLockFailedEvent{
						Reason: err.Error(),
					},
				}, nil
			}
		} else {
			err := d.locker.LockMany(ctx, msg.Inputs, owner)
			if err != nil {
				d.log.DebugS(ctx, "Input lock failed",
					btclog.Hex("session_id", sessionID[:]),
					slog.Int("num_inputs", len(msg.Inputs)),
					slog.String("reason", err.Error()))

				return []Event{
					&InputsLockFailedEvent{
						Reason: err.Error(),
					},
				}, nil
			}
		}
	}

	d.log.DebugS(ctx, "Inputs locked",
		btclog.Hex("session_id", sessionID[:]),
		slog.Int("num_inputs", len(msg.Inputs)))

	return []Event{
		&InputsLockSucceededEvent{},
	}, nil
}

// handleValidateSubmit validates the submit package and returns an inbox event
// indicating success/failure.
//
// Submit validation is structural at this stage. The Ark PSBT carries the
// client-selected collaborative owner leaf, so full script-valid spends are
// only expected after finalize.
func (d *InProcessOutboxDriver) handleValidateSubmit(ctx context.Context,
	msg *ValidateSubmitReq) ([]Event, error) {

	validated, err := oorlib.ValidateSubmitPackage(
		msg.ArkPSBT, msg.CheckpointPSBTs,
	)
	if err != nil {
		d.log.DebugS(ctx, "Submit validation failed",
			slog.Int("num_checkpoints", len(msg.CheckpointPSBTs)),
			slog.String("reason", err.Error()))

		return []Event{
			&SubmitFailedEvent{
				Reason: err.Error(),
			},
		}, nil
	}

	if d.store != nil {
		err := validateSubmitRebuildAndPolicy(
			ctx, msg.ArkPSBT, msg.CheckpointPSBTs,
			msg.VTXOSigningDescriptors,
			msg.CheckpointPolicy, d.store,
			d.operatorPolicy,
		)
		if err != nil {
			d.log.DebugS(ctx, "Submit rebuild/policy check failed",
				slog.String("ark_txid", validated.ArkTxid.String()),
				slog.String("reason", err.Error()))

			return []Event{
				&SubmitFailedEvent{
					Reason: err.Error(),
				},
			}, nil
		}
	}

	d.log.InfoS(ctx, "Submit package validated",
		slog.String("ark_txid", validated.ArkTxid.String()),
		slog.Int("num_checkpoints", len(msg.CheckpointPSBTs)))

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

	err := d.coSigner.CoSignCheckpoints(
		d.operatorKey, msg.VTXOSigningDescriptors, msg.CheckpointPSBTs,
	)
	if err != nil {
		d.log.WarnS(ctx, "Co-sign checkpoints failed", err,
			btclog.Hex("session_id", sessionID[:]))

		return []Event{
			&SignFailedEvent{
				Reason: err.Error(),
			},
		}, nil
	}

	d.log.DebugS(ctx, "Checkpoints co-signed",
		btclog.Hex("session_id", sessionID[:]),
		slog.Int("num_checkpoints", len(msg.CheckpointPSBTs)))

	if d.operatorSigner != nil && msg.ArkPSBT != nil {
		arkSigned, err := CoSignArkPSBT(
			d.operatorSigner, d.operatorKey, msg.ArkPSBT,
		)
		if err != nil {
			d.log.WarnS(ctx, "Co-sign Ark PSBT failed", err,
				btclog.Hex("session_id", sessionID[:]))

			return []Event{
				&SignFailedEvent{
					Reason: fmt.Sprintf(
						"co-sign ark psbt: %v", err,
					),
				},
			}, nil
		}

		if arkSigned {
			_, err = oorlib.ValidateSubmitPackageSigned(
				msg.ArkPSBT, msg.CheckpointPSBTs,
			)
			if err != nil {
				d.log.WarnS(ctx, "Co-signed Ark PSBT invalid",
					err, btclog.Hex("session_id", sessionID[:]))

				reason := fmt.Sprintf(
					"co-signed ark PSBT invalid: %v", err,
				)

				return []Event{
					&SignFailedEvent{
						Reason: reason,
					},
				}, nil
			}
		}
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

	// When an operator key is configured, enforce finalize signature
	// correctness against the co-signed checkpoint set before the
	// FSM advances to spent-state side effects.
	if d.operatorKey.PubKey != nil {
		err = validateFinalizeCheckpointSignatures(
			d.operatorKey.PubKey,
			msg.CoSignedCheckpointPSBTs,
			msg.FinalCheckpointPSBTs,
		)
		if err != nil {
			return []Event{
				&FinalizeFailedEvent{
					Reason: err.Error(),
				},
			}, nil
		}
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

	var outputRecords []*vtxo.Record
	if d.store != nil {
		var err error
		outputRecords, err = d.materializedOutputRecords(ctx, msg)
		if err != nil {
			return nil, err
		}
	}

	switch {
	case d.store != nil && d.sessionStore != nil:
		atomicStore, ok := d.sessionStore.(FinalizeAtomicStore)
		if !ok {
			return nil, fmt.Errorf("session store must implement " +
				"FinalizeAtomicStore when store is configured")
		}

		err := atomicStore.ApplyFinalizeAndMaterialize(
			ctx, sessionID, msg.Inputs, msg.FinalCheckpointPSBTs,
			outputRecords,
		)
		if err != nil {
			return nil, err
		}

	case d.store != nil:
		err := d.finalizeVTXOSet(ctx, msg.Inputs, outputRecords)
		if err != nil {
			return nil, err
		}

	case d.sessionStore != nil:
		err := d.sessionStore.ApplyFinalize(
			ctx, sessionID, msg.FinalCheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}
	}

	d.log.InfoS(ctx, "Session finalized",
		btclog.Hex("session_id", sessionID[:]),
		slog.Int("num_checkpoints", len(msg.FinalCheckpointPSBTs)),
		slog.Int("num_inputs", len(msg.Inputs)))

	return []Event{
		&FinalizeSucceededEvent{
			FinalCheckpointPSBTs: msg.FinalCheckpointPSBTs,
		},
	}, nil
}

// materializedOutputRecords builds the recipient VTXO records implied by a
// finalized Ark package. Any metadata lookup/validation failures happen before
// mutations so the atomic DB path can fail fast.
func (d *InProcessOutboxDriver) materializedOutputRecords(ctx context.Context,
	msg *FinalizeReq) ([]*vtxo.Record, error) {

	ark := msg.ArkPSBT
	if ark == nil || ark.UnsignedTx == nil {
		return nil, fmt.Errorf("ark psbt must be provided")
	}

	tx := ark.UnsignedTx
	arkTxid := tx.TxHash()

	recipients := msg.Recipients
	if len(recipients) == 0 {
		outs := tx.TxOut
		if len(outs) == 0 {
			return nil, fmt.Errorf("ark tx must have outputs")
		}

		recipients = make([]oorlib.RecipientOutput, 0, len(outs)-1)
		for i := 0; i < len(outs)-1; i++ {
			recipients = append(recipients, oorlib.RecipientOutput{
				PkScript: outs[i].PkScript,
				Value:    btcutil.Amount(outs[i].Value),
			})
		}
	}

	// Materialize the non-anchor Ark outputs into the in-memory VTXO set.
	records := make([]*vtxo.Record, 0, len(recipients))
	for i := 0; i < len(recipients); i++ {
		recipient := recipients[i]
		record := &vtxo.Record{
			Outpoint: wire.OutPoint{
				Hash:  arkTxid,
				Index: uint32(i),
			},
			Value:          int64(recipient.Value),
			PolicyTemplate: recipient.VTXOPolicyTemplate,
			PkScript:       recipient.PkScript,
			Status:         vtxo.StatusLive,
		}
		records = append(records, record)
	}

	return records, nil
}

// finalizeVTXOSet marks inputs spent and materializes Ark tx outputs as new
// VTXOs in the in-memory store (v0 behavior for tests).
func (d *InProcessOutboxDriver) finalizeVTXOSet(ctx context.Context,
	inputs []wire.OutPoint, outputRecords []*vtxo.Record) error {

	if d.store == nil {
		return nil
	}

	// NOTE: MarkSpent and Create are separate calls here because the
	// in-process driver uses an in-memory store for tests. The production
	// DB path applies VTXO set mutations atomically via
	// FinalizeAtomicStore.
	err := d.store.MarkSpent(ctx, inputs)
	if err != nil {
		return err
	}

	for _, record := range outputRecords {
		err := d.store.Create(ctx, record)
		if err != nil {
			return err
		}
	}

	return nil
}

// handleNotifyRecipients appends durable recipient events for the finalized
// Ark transaction, pushes transfer notifications via clientconn, and returns
// an FSM event indicating success or failure.
func (d *InProcessOutboxDriver) handleNotifyRecipients(ctx context.Context,
	sessionID SessionID,
	msg *NotifyRecipientsReq) ([]Event, error) {

	ark := msg.ArkPSBT

	// Extract recipients from the Ark PSBT when available. The
	// recipient list is used for both RecipientEventStore persistence
	// and the clientconn push path.
	var recipients []clientoor.ArkRecipientOutput
	if ark != nil {
		var err error
		recipients, err = clientoor.ExtractArkRecipients(ark)
		if err != nil {
			return []Event{
				&NotifyRecipientsFailedEvent{
					Reason: err.Error(),
				},
			}, nil
		}
	}

	// Persist recipient events to the polling store when configured.
	if d.recipientEvents != nil {
		if ark == nil {
			return []Event{
				&NotifyRecipientsFailedEvent{
					Reason: "ark psbt must be provided",
				},
			}, nil
		}

		err := d.recipientEvents.AppendRecipientEvents(
			ctx, sessionID, ark, recipients,
		)
		if err != nil {
			return []Event{
				&NotifyRecipientsFailedEvent{
					Reason: err.Error(),
				},
			}, nil
		}
	}

	// Best-effort push each recipient through the indexer event
	// bridge so connected clients receive real-time notification
	// without polling. Failures are logged internally by the
	// notifier and do not block finalization.
	if d.recipientNotifier != nil {
		for _, r := range recipients {
			d.recipientNotifier.NotifyRecipientEvent(
				ctx, sessionID, r,
			)
		}
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

	d.log.InfoS(ctx, "Recipients notified",
		btclog.Hex("session_id", sessionID[:]),
		slog.Int("num_recipients", len(recipients)))

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

		d.log.DebugS(ctx, "Inputs unlocked",
			btclog.Hex("session_id", sessionID[:]),
			slog.Int("num_inputs", len(msg.Inputs)))
	}

	return nil, nil
}
