package serverconn

import (
	"testing"

	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/stretchr/testify/require"
)

// responseEnvelope builds a KIND_RESPONSE envelope with the given correlation
// ID and event_seq for split-partition tests.
func responseEnvelope(corrID string, seq uint64) *mailboxpb.Envelope {
	return &mailboxpb.Envelope{
		EventSeq: seq,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_RESPONSE,
			CorrelationId: corrID,
		},
	}
}

// eventEnvelope builds a KIND_EVENT envelope with the given event_seq for
// split-partition tests.
func eventEnvelope(seq uint64) *mailboxpb.Envelope {
	return &mailboxpb.Envelope{
		EventSeq: seq,
		Rpc: &mailboxpb.RpcMeta{
			Kind: mailboxpb.RpcMeta_KIND_EVENT,
		},
	}
}

// TestSplitIngressEnvelopesWaiterRouting verifies that splitIngressEnvelopes
// keeps the fast pre-transaction path only for responses with a live waiter,
// while a waiterless response folds into the durable bucket alongside events.
// This is the no-reorder/single-commit contract: a durable-fallback response
// must commit in the same transaction as the cursor, not ahead of it.
func TestSplitIngressEnvelopesWaiterRouting(t *testing.T) {
	t.Parallel()

	// Only "corr-live" has a registered in-memory waiter.
	hasWaiter := func(id CorrelationID) bool {
		return id == CorrelationID("corr-live")
	}

	envelopes := []*mailboxpb.Envelope{
		responseEnvelope("corr-live", 1),
		responseEnvelope("corr-gone", 2),
		eventEnvelope(3),
		responseEnvelope("", 4),
	}

	responses, durables := splitIngressEnvelopes(envelopes, hasWaiter)

	// Only the waiter-backed response takes the pre-transaction path.
	require.Len(t, responses, 1)
	require.Equal(t, uint64(1), responses[0].EventSeq)

	// The waiterless response, the event, and the correlation-less
	// response all fold into the durable transaction in event_seq order.
	require.Len(t, durables, 3)
	require.Equal(t, uint64(2), durables[0].EventSeq)
	require.Equal(t, uint64(3), durables[1].EventSeq)
	require.Equal(t, uint64(4), durables[2].EventSeq)
}

// TestSplitIngressEnvelopesNoWaiters verifies that with no live waiters every
// envelope folds into the durable transaction, so nothing dispatches ahead of
// the cursor commit on a crash-replay batch where all waiters are gone.
func TestSplitIngressEnvelopesNoWaiters(t *testing.T) {
	t.Parallel()

	hasWaiter := func(CorrelationID) bool { return false }

	envelopes := []*mailboxpb.Envelope{
		responseEnvelope("corr-a", 1),
		responseEnvelope("corr-b", 2),
		eventEnvelope(3),
	}

	responses, durables := splitIngressEnvelopes(envelopes, hasWaiter)

	require.Empty(t, responses)
	require.Len(t, durables, 3)
}

// TestDeliverWaiterResponsesDefersVanishedWaiters covers the TOCTOU guard in
// the pre-transaction response path. splitIngressEnvelopes routes a response
// to the fast bucket on a split-time waiter peek, but the waiter can vanish
// (RPC deadline cancel or TTL prune) before delivery runs. deliverWaiter
// Responses must deliver only to a still-live waiter and return every other
// response as a straggler — to fold into the durable transaction — rather than
// dispatch it durably outside the cursor fold.
func TestDeliverWaiterResponsesDefersVanishedWaiters(t *testing.T) {
	t.Parallel()

	actor, _, _ := newTestConnector(t, nil)

	// corr-live keeps a live waiter; corr-gone models a waiter that
	// vanished after the split peek (never registered here), and the empty
	// correlation ID can never match a waiter.
	actor.RegisterWaiter(CorrelationID("corr-live"))

	responses := []*mailboxpb.Envelope{
		responseEnvelope("corr-live", 1),
		responseEnvelope("corr-gone", 2),
		responseEnvelope("", 3),
	}

	stragglers := actor.deliverWaiterResponses(responses)

	// The live-waiter response is delivered in memory and excluded; only
	// the vanished-waiter and correlation-less responses fold back into the
	// durable batch, preserved in event_seq order.
	require.Len(t, stragglers, 2)
	require.Equal(t, uint64(2), stragglers[0].EventSeq)
	require.Equal(t, uint64(3), stragglers[1].EventSeq)
}

// TestMergeEnvelopesByEventSeq verifies the straggler fold preserves global
// event_seq order when deferred responses merge back into the durable
// partition, and that empty inputs are handled.
func TestMergeEnvelopesByEventSeq(t *testing.T) {
	t.Parallel()

	durables := []*mailboxpb.Envelope{
		eventEnvelope(1),
		eventEnvelope(3),
		eventEnvelope(5),
	}
	stragglers := []*mailboxpb.Envelope{
		responseEnvelope("a", 2),
		responseEnvelope("b", 4),
	}

	merged := mergeEnvelopesByEventSeq(durables, stragglers)
	require.Len(t, merged, 5)
	for i, env := range merged {
		require.Equal(t, uint64(i+1), env.EventSeq)
	}

	// Empty straggler set returns the durable partition unchanged.
	require.Equal(
		t, durables, mergeEnvelopesByEventSeq(durables, nil),
	)

	// Empty durable partition returns the stragglers unchanged.
	require.Equal(
		t, stragglers, mergeEnvelopesByEventSeq(nil, stragglers),
	)
}
