package serverconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"google.golang.org/protobuf/proto"
)

// quarantineStore is the persistence contract shared with delivery storage.
type quarantineStore = mailboxconn.IngressQuarantineStore

// poisonEnvelopeError distinguishes deterministic wire adaptation failures
// from transient consumer/store failures, which must retain normal redelivery.
type poisonEnvelopeError struct{ err error }

// Error describes the failing route and conversion.
func (e *poisonEnvelopeError) Error() string { return e.err.Error() }

// Unwrap preserves the original parser error for diagnosis.
func (e *poisonEnvelopeError) Unwrap() error { return e.err }

// ingressQuarantineKey exposes only the active fold's evidence store.
type ingressQuarantineKey struct{}

// quarantineLane identifies the configured peer and reply mailbox without
// relying on untrusted envelope routing fields.
func (a *ServerConnectionActor) quarantineLane() string {
	ingressScope := a.ingressEvidenceScope()
	scope := fmt.Appendf(
		nil, "%d:%s%s", len(ingressScope.reply), ingressScope.reply,
		ingressScope.remote,
	)
	digest := sha256.Sum256(scope)

	return hex.EncodeToString(digest[:])
}

// quarantinePoison preserves exact rejected bytes in the checkpoint
// transaction. Full storage or missing persistence fails closed and leaves the
// remote event unacknowledged. Consumer errors are never converted into
// quarantine success.
func (a *ServerConnectionActor) quarantinePoison(ctx context.Context,
	env *mailboxpb.Envelope, dispatchErr error) error {

	var poison *poisonEnvelopeError
	if !errors.As(dispatchErr, &poison) {
		if !errors.Is(
			dispatchErr, mailboxconn.ErrIngressReceiptMismatch,
		) {
			return dispatchErr
		}
		poison = &poisonEnvelopeError{err: dispatchErr}
	}
	store, ok :=
		ctx.Value(ingressQuarantineKey{}).(quarantineStore)
	if !ok {
		return dispatchErr
	}
	encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(env)
	if err != nil {
		return err
	}
	lane := a.quarantineLane()
	digest := sha256.Sum256(append([]byte(lane+":"), encoded...))
	id := "quarantine-v1:" + hex.EncodeToString(digest[:])
	err = store.QuarantineIngress(
		ctx, mailboxconn.IngressQuarantine{
			ID:       id,
			Lane:     lane,
			Envelope: encoded,
			Reason:   poison.Error(),
		},
	)
	if err != nil {
		return err
	}
	a.log.InfoS(ctx, "Ingress event preserved for recovery",
		poison,
		slog.String("quarantine_id", id),
		slog.Uint64("event_seq", env.EventSeq),
	)

	return nil
}

// retryQuarantinedIngress attempts each retained envelope once per process
// start. This bounds retries of deterministic poison while permitting recovery
// after an adapter upgrade. Evidence is removed only in a durable inbox commit;
// an in-memory handoff leaves it available for the next explicit restart.
func (a *ServerConnectionActor) retryQuarantinedIngress(ctx context.Context,
	txStore actor.TxAwareDeliveryStore) {

	store, ok := a.cfg.Store.(quarantineStore)
	if !ok {
		return
	}
	entries, err := store.ListIngressQuarantine(ctx, a.quarantineLane())
	if err != nil {
		a.log.WarnS(ctx, "Cannot load ingress quarantine", err)

		return
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if err := store.NoteIngressQuarantineAttempt(
			ctx, entry.ID,
		); err != nil {

			a.log.WarnS(
				ctx,
				"Cannot record ingress recovery attempt",
				err,
			)

			return
		}
		err = txStore.ExecTx(
			ctx, false,
			func(txCtx context.Context,
				tx actor.DeliveryStore) error {

				evidence, ok := tx.(quarantineStore)
				if !ok {
					return fmt.Errorf("transaction has " +
						"no ingress evidence store")
				}
				env := &mailboxpb.Envelope{}
				if err := proto.Unmarshal(
					entry.Envelope, env,
				); err != nil {
					return err
				}
				if err := a.validateInboundEnvelope(
					env,
				); err != nil {
					return err
				}
				if env.Rpc == nil || a.isNonTxRequest(env) ||
					a.resolvesToNonTxDispatcher(env) {
					return fmt.Errorf("quarantined route " +
						"has no durable recovery " +
						"handoff")
				}
				route := mailboxrpc.ServiceMethod{
					Service: env.Rpc.Service,
					Method:  env.Rpc.Method,
				}
				dispatcher, ok := a.cfg.Dispatchers[route]
				if !ok {
					return fmt.Errorf("quarantined route " +
						"is unavailable")
				}
				txCtx = context.WithValue(
					txCtx, ingressScopeKey{},
					a.ingressEvidenceScope(),
				)
				txCtx, err := withEventReceipt(txCtx, env)
				if err != nil {
					return err
				}
				_, ok = mailboxconn.IngressReceiptFromContext(
					txCtx,
				)
				if !ok {
					txCtx = mailboxconn.WithIngressReceipt(
						txCtx, entry.ID,
					)
				}
				if err := dispatcher(txCtx, env); err != nil {
					return err
				}
				if !mailboxconn.IngressReceiptHandedOff(txCtx) {
					return fmt.Errorf("recovery has no " +
						"durable consumer handoff; " +
						"evidence retained")
				}

				return evidence.DeleteIngressQuarantine(
					txCtx, entry.ID,
				)
			},
		)
		if err != nil {
			a.log.WarnS(ctx, "Ingress recovery remains pending",
				err,
				slog.String("quarantine_id", entry.ID),
			)

			continue
		}
		a.log.InfoS(ctx, "Ingress quarantine recovered",
			slog.String("quarantine_id", entry.ID),
		)
	}
}
