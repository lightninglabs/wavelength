package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// ActorFamilyCreditOp labels the durable mailbox family owned by a
	// per-operation credit actor.
	ActorFamilyCreditOp = "credit-op"

	// ActorFamilyIncomingVTXO labels the durable incoming-VTXO handler
	// mailbox.
	ActorFamilyIncomingVTXO = "incoming-vtxo"

	// actorFamilyOther buckets a mailbox family the metrics actor does not
	// recognize. It keeps an unexpected value from becoming its own
	// time series.
	actorFamilyOther = "other"
)

const (
	// PostponeCauseMissing means the awaited object does not exist yet
	// (for example an indexer scan that finished without the outpoint it
	// was asked for).
	PostponeCauseMissing = "missing"

	// PostponeCauseChainUnconfirmed means a commitment transaction the
	// evidence depends on is not confirmed yet.
	PostponeCauseChainUnconfirmed = "chain-unconfirmed"

	// PostponeCauseIndexerError means the indexer lookup itself failed.
	PostponeCauseIndexerError = "indexer-error"

	// PostponeCauseServerError means the operator's credit/pay surface
	// could not be read.
	PostponeCauseServerError = "server-error"

	// PostponeCauseOther buckets a cause the metrics actor does not
	// recognize.
	PostponeCauseOther = "other"
)

// normalizeActorFamily maps an unrecognized mailbox family to a fixed bucket.
// Label values must come from a closed set: a caller-supplied identifier (a
// per-operation actor id, say) would otherwise create one time series per
// operation.
func normalizeActorFamily(family string) string {
	switch family {
	case ActorFamilyCreditOp, ActorFamilyIncomingVTXO:
		return family

	default:
		return actorFamilyOther
	}
}

// normalizePostponeCause maps an unrecognized postpone cause to a fixed
// bucket, for the same cardinality reason as normalizeActorFamily.
func normalizePostponeCause(cause string) string {
	switch cause {
	case PostponeCauseMissing, PostponeCauseChainUnconfirmed,
		PostponeCauseIndexerError, PostponeCauseServerError:
		return cause

	default:
		return PostponeCauseOther
	}
}

// EmitDeliveryPostpone reports one postpone decision, or one expired-horizon
// decision, to the metrics actor. Every durable behavior that postpones a
// delivery shares this one path so the counters mean the same thing wherever
// they are emitted.
//
// It is best-effort observation: an unconfigured sink does nothing and a Tell
// failure is logged at debug, so instrumentation never changes how a delivery
// is handled. The cause is a coarse PostponeCause bucket, never a
// caller-supplied identifier, which is why the metrics actor normalizes it
// again on the way in.
func EmitDeliveryPostpone(ctx context.Context, sink fn.Option[Sink],
	log btclog.Logger, cause string, expired bool) {

	sink.WhenSome(func(s Sink) {
		msg := &DeliveryPostponeMsg{
			Cause:   cause,
			Expired: expired,
		}

		if err := s.TryTell(ctx, msg); err != nil {
			log.DebugS(ctx, "Failed to emit postpone metric",
				err,
				slog.String("cause", cause),
			)
		}
	})
}

// WithDeadLetterCounter wraps a Tell retry policy so every delivery the policy
// gives up on is counted as a dead letter for the supplied actor family.
//
// A policy that declines to retry is exactly the durable actor's dead-letter
// decision: the runtime moves the mailbox row to the dead-letter table and
// deletes it in the same step. Counting from the policy keeps the
// instrumentation at the call sites that own a metrics sink instead of teaching
// the actor framework about metrics.
//
// The counter is a lower bound on dead letters, not an exact count. The
// runtime also dead-letters when the mailbox attempt ceiling is reached even
// though the policy still wanted to retry, and when a message cannot be
// decoded at all; neither case passes through the policy. Both are outside
// the path a retry policy is meant to describe, and for the money-moving
// actors wired here the policy stops the delivery first.
//
// A nil policy falls back to actor.DefaultTellRetryPolicy, and a missing sink
// returns the policy untouched, so an unconfigured daemon pays nothing.
func WithDeadLetterCounter(base actor.TellRetryPolicy, sink fn.Option[Sink],
	family string) actor.TellRetryPolicy {

	if base == nil {
		base = actor.DefaultTellRetryPolicy
	}

	if sink.IsNone() {
		return base
	}

	family = normalizeActorFamily(family)

	return func(err error, attempts int) (bool, time.Duration) {
		retry, delay := base(err, attempts)
		if retry {
			return retry, delay
		}

		// The policy runs inside a failed delivery turn, so this is
		// observation only: drop the sample rather than waiting on a
		// busy metrics mailbox, and never let it affect the decision.
		sink.WhenSome(func(s Sink) {
			_ = s.TryTell(
				context.Background(), &ActorDeadLetterMsg{
					Actor: family,
				},
			)
		})

		return retry, delay
	}
}
