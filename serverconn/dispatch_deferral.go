package serverconn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/metrics"
)

// ErrDispatchDeferred marks an inbound envelope that could not be handed to its
// target actor because the target refused it for want of room: a full
// in-memory mailbox, or a durable mailbox whose backlog crossed its hard
// watermark. It is not a dispatch failure: the envelope is intact,
// unacknowledged, and the ingress loop re-pulls it after a backoff, so the
// cursor never passes an event that was not delivered.
//
// The error exists so the ingress loop can tell backpressure apart from a real
// dispatch error. Both back off, but only backpressure is expected to clear on
// its own, and only backpressure means a local actor — not the operator — is
// the thing that stopped making progress.
var ErrDispatchDeferred = errors.New("serverconn: ingress dispatch deferred")

// deferredDispatchError carries the identity of a deferred delivery so the
// ingress loop can name the target that stopped draining. Everything in it is
// known at the dispatcher, and nothing else on the ingress path can reconstruct
// it: by the time the error reaches the loop, the envelope has been consumed.
type deferredDispatchError struct {
	// service and method are the envelope's route.
	service string
	method  string

	// target is the ID of the actor reference whose mailbox was full.
	target string

	// eventSeq is the sequence number of the undelivered envelope, which is
	// also the cursor the next pull resumes from.
	eventSeq uint64

	// err is the underlying delivery error, always wrapping either
	// actor.ErrMailboxFull (bounded in-memory target) or
	// actor.ErrMailboxSaturated (durable target past its hard watermark).
	err error
}

// Error implements the error interface.
func (e *deferredDispatchError) Error() string {
	return fmt.Sprintf("deferred %s/%s event_seq=%d to actor %q: %v",
		e.service, e.method, e.eventSeq, e.target, e.err)
}

// Unwrap exposes both the sentinel and the underlying mailbox error, so
// errors.Is matches ErrDispatchDeferred for the loop's classification and
// the concrete cause (actor.ErrMailboxFull or actor.ErrMailboxSaturated)
// for anything that cares which bound turned the envelope away.
func (e *deferredDispatchError) Unwrap() []error {
	return []error{ErrDispatchDeferred, e.err}
}

// deferDispatch builds the deferral error for an envelope that a full mailbox
// turned away.
func deferDispatch(service, method, target string, eventSeq uint64,
	err error) *deferredDispatchError {

	return &deferredDispatchError{
		service:  service,
		method:   method,
		target:   target,
		eventSeq: eventSeq,
		err:      err,
	}
}

// deliverToActor hands an adapted message to its target actor without ever
// parking the ingress goroutine. A bounded in-memory target is sent to with
// TryTell and a full mailbox comes back as a deferral; a durable target keeps
// the blocking Tell, which is bounded by its own write and has to stay inside
// the caller's transaction to commit atomically with the cursor. A durable
// target with backlog watermarks configured can refuse that Tell with
// ErrMailboxSaturated, which classifies as the same deferral: the backlog is
// expected to drain on its own, and stalling the cursor at the undelivered
// envelope is exactly the backpressure the watermark asks for.
//
// This is the fix for the wedge that made a deployed client go deaf: the round
// client and the incoming-VTXO handler are registered with the default
// fixed-capacity mailbox, and the ingress loop used to deliver to them with a
// blocking Tell that had no deadline, from inside the folded write transaction.
// A target that stopped draining therefore parked the process's only mailbox
// puller forever, with the database's single writer held, and nothing logged.
//
// A TryTell delivery is the one thing here that outlives a rolled-back
// transaction, so it is recorded on the way out and suppressed on the way back
// in: the fold's store may run its body again after a retryable error, and
// without this the target would receive the same event once per attempt with
// nothing logged either. See deliveredOutsideTx.
func deliverToActor[M actor.Message, R any](ctx context.Context,
	ref actor.ActorRef[M, R], msg M, service, method string,
	eventSeq uint64) error {

	replayed := deliveredOutsideTxFrom(ctx)
	if replayed.seen(eventSeq) {
		return nil
	}

	outsideTx, err := actor.TellWithoutParking[M](ctx, ref, msg).Unpack()
	if errors.Is(err, actor.ErrMailboxFull) ||
		errors.Is(err, actor.ErrMailboxSaturated) {
		return deferDispatch(service, method, ref.ID(), eventSeq, err)
	}
	if err != nil {
		return err
	}

	if outsideTx {
		replayed.mark(eventSeq)
	}

	return nil
}

// deferralLogInterval bounds how often an open backpressure episode re-states
// itself in the log. One line per re-pull is the storm that would bury the
// signal, but one line for the whole episode is how a permanent wedge ends up
// with a single record that can age out of retention before anyone looks — the
// exact shape of the two incidents this path was added for. Re-stating on an
// interval keeps a persistent episode continuously visible while staying two
// orders of magnitude below the redrive rate.
const deferralLogInterval = 30 * time.Second

// deferralEpisode is the ingress loop's own state for one backpressure episode,
// carried on that single goroutine's stack because it is the only place that
// sees every dispatch outcome in order. It exists to throttle the log without
// losing the episode's shape: when it started, how many redrives it has taken,
// and when it ended.
type deferralEpisode struct {
	// open is true between the first deferral of an episode and the next
	// fully successful dispatch.
	open bool

	// deferrals counts the redrives this episode has turned away, including
	// the first.
	deferrals int

	// startedAt is when the episode's first deferral was seen, so the
	// closing line can state how long the target was backed up.
	startedAt time.Time

	// lastLoggedAt is when this episode last wrote a line, which is what
	// deferralLogInterval throttles against.
	lastLoggedAt time.Time
}

// noteDispatchDeferral reports whether err is a deferred dispatch, returning
// the deferral when it is and nil when it is not. A deferral is always counted;
// it is logged on the first one of an episode and then at most once per
// deferralLogInterval, because a target that has stopped draining defers again
// on every re-pull. clearDispatchDeferral closes the episode.
//
// The level splits by how long the episode has lasted. A brief deferral is
// ordinary backpressure that the deferred counter already captures, so the
// episode's opening line is a warning, per the repo rule that error level is
// for internal bugs rather than external triggers. An episode still open a
// full deferralLogInterval later means the target has stopped draining, which
// is exactly the stuck-receive-loop bug class this path exists to expose, so
// the interval re-logs escalate to error level. Before this path existed the
// only log here was a Trace inside the mailbox, which is why two production
// incidents produced no evidence at all.
func (a *ServerConnectionActor) noteDispatchDeferral(ctx context.Context,
	err error, episode *deferralEpisode) *deferredDispatchError {

	var deferral *deferredDispatchError
	if !errors.As(err, &deferral) {
		return nil
	}

	// The counter counts redrives turned away, not envelopes: the loop
	// meets the full mailbox once per re-pull and stops there, so the
	// envelopes queued behind the first are never attempted and cannot be
	// counted.
	metrics.ServerConnIngressDeferredTotal.WithLabelValues(
		deferral.service, deferral.method,
	).Inc()

	now := time.Now()

	first := !episode.open
	if first {
		episode.open = true
		episode.deferrals = 0
		episode.startedAt = now
	}

	episode.deferrals++

	if !first && now.Sub(episode.lastLoggedAt) < deferralLogInterval {
		return deferral
	}

	episode.lastLoggedAt = now

	logFn := a.log.WarnS
	if !first {
		logFn = a.log.ErrorS
	}
	logFn(
		ctx, "Ingress dispatch deferred: target mailbox full, "+
			"re-pulling after backoff", err,
		slog.String("mailbox_id", a.cfg.LocalMailboxID),
		slog.String("service", deferral.service),
		slog.String("method", deferral.method),
		slog.String("target_actor", deferral.target),
		slog.Uint64("event_seq", deferral.eventSeq),
		slog.Int("episode_deferrals", episode.deferrals),
		slog.String("episode_age", now.Sub(episode.startedAt).String()),
	)

	return deferral
}

// clearDispatchDeferral closes an open backpressure episode, so the next one is
// logged again and an operator reading the log can bound how long the target
// was backed up.
func (a *ServerConnectionActor) clearDispatchDeferral(ctx context.Context,
	episode *deferralEpisode) {

	if !episode.open {
		return
	}

	deferrals := episode.deferrals
	age := time.Since(episode.startedAt)

	*episode = deferralEpisode{}

	a.log.InfoS(ctx, "Ingress dispatch backpressure cleared",
		slog.String("mailbox_id", a.cfg.LocalMailboxID),
		slog.Int("episode_deferrals", deferrals),
		slog.String("episode_duration", age.String()),
	)
}
