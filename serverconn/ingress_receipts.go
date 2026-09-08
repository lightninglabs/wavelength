package serverconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
)

// ingressReceiptPruneInterval bounds SQLite write-lock pressure while keeping
// physical cleanup active. Logical expiry is enforced during admission.
const ingressReceiptPruneInterval = time.Hour

// ingressScopeKey separates each configured server/client connection's event
// identities. Only the transactional ingress path installs this scope.
type ingressScopeKey struct{}

// ingressScope fixes local trust-domain routing independently of peer fields.
type ingressScope struct{ local, remote string }

// withEventReceipt attaches a producer-persisted delivery occurrence ID to
// asynchronous events. Legacy body-derived IDs and operation idempotency keys
// cannot distinguish a replay from a fresh identical response, so they retain
// their existing delivery behavior. Ordinary RPC responses are also unchanged.
func withEventReceipt(ctx context.Context,
	env *mailboxpb.Envelope) (context.Context, error) {

	if _, ok := mailboxconn.IngressReceiptFromContext(ctx); ok {
		return ctx, nil
	}
	scope, ok := ctx.Value(ingressScopeKey{}).(ingressScope)
	if !ok || env.GetRpc().GetKind() != mailboxpb.RpcMeta_KIND_EVENT ||
		!mailboxconn.IsDeliveryOccurrenceID(env.MsgId) {
		return ctx, nil
	}
	fields := []string{
		"ingress-event/v1", scope.local, scope.remote, env.Sender,
		env.Rpc.Service, env.Rpc.Method, env.MsgId,
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return ctx, err
	}
	digest := sha256.Sum256(encoded)

	return mailboxconn.WithIngressReceipt(
		ctx, hex.EncodeToString(digest[:]),
	), nil
}

// ingressReceiptPruner is the optional persistence capability used by ingress
// to reclaim expired receipts on both active and empty long-poll cycles.
type ingressReceiptPruner interface {
	// PruneIngressReceipts removes at most limit expired receipt rows.
	PruneIngressReceipts(context.Context, int) (int64, error)
}

// pruneIngressReceiptsIfDue performs one bounded cleanup when the schedule is
// due and returns the next deadline. Callers may invoke it on every loop
// iteration without opening extra write transactions.
func (a *ServerConnectionActor) pruneIngressReceiptsIfDue(ctx context.Context,
	now, next time.Time) time.Time {

	if now.Before(next) {
		return next
	}
	a.pruneIngressReceipts(ctx)

	return now.Add(ingressReceiptPruneInterval)
}

// pruneIngressReceipts bounds background cleanup without delaying dispatch on
// a failed maintenance operation. Admission itself enforces the expiry cutoff.
func (a *ServerConnectionActor) pruneIngressReceipts(ctx context.Context) {
	if pruner, ok := a.cfg.Store.(ingressReceiptPruner); ok {
		if _, err := pruner.PruneIngressReceipts(
			ctx, 512,
		); err != nil &&
			!isIngressShutdownErr(ctx, err) {

			a.log.WarnS(ctx, "Ingress receipt cleanup failed", err)
		}
	}
}
