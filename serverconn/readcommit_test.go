package serverconn

import (
	"context"
	"fmt"
	"testing"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// errSendEdge is a MailboxServiceClient whose Send always fails, used to drive
// the connector's send-failure path. The embedded nil interface is never
// exercised because only Send is called.
type errSendEdge struct {
	mailboxpb.MailboxServiceClient
}

// Send always returns an error.
func (errSendEdge) Send(context.Context, *mailboxpb.SendRequest,
	...grpc.CallOption) (*mailboxpb.SendResponse, error) {

	return nil, fmt.Errorf("edge send failed")
}

// nonTxStore wraps a DeliveryStore but deliberately omits ExecTx, so it does
// not satisfy actor.TxAwareDeliveryStore. The Read/Commit construction path
// must reject it.
type nonTxStore struct {
	actor.DeliveryStore
}

// TestEgressCommitsOnSuccessfulSend verifies that a successful Edge.Send is
// followed by exactly one Commit -- the lease-fenced ack+dedup fold -- and that
// the envelope reaches the mailbox. This is the happy path of the Read/Commit
// migration: the send happens with no writer held, then a single short Commit
// consumes the message.
func TestEgressCommitsOnSuccessfulSend(t *testing.T) {
	t.Parallel()

	connector, mb, _ := newTestConnector(t, nil)

	ax := &fakeEgressExec{}
	req := &SendClientEventRequest{
		Message: &testServerMessage{
			value: "evt",
		},
	}

	result := connector.Receive(t.Context(), req, ax)
	require.NoError(t, result.Err())
	require.Equal(t, 1, ax.commits)

	mb.mu.Lock()
	defer mb.mu.Unlock()
	require.Len(t, mb.mailboxes["server-1"], 1)
}

// TestEgressDoesNotCommitOnSendError verifies that a failed Edge.Send returns
// an error WITHOUT committing, so the framework nacks and retries the message
// rather than consuming it. Preserving this retry-on-failure behavior is what
// makes the conversion behavior-equivalent to the old Classic path, which also
// rolled back (and thus retried) when the send failed.
func TestEgressDoesNotCommitOnSendError(t *testing.T) {
	t.Parallel()

	connector, _, _ := newTestConnector(t, nil)
	connector.cfg.Edge = errSendEdge{}

	ax := &fakeEgressExec{}
	req := &SendClientEventRequest{
		Message: &testServerMessage{
			value: "evt",
		},
	}

	result := connector.Receive(t.Context(), req, ax)
	require.Error(t, result.Err())
	require.Zero(t, ax.commits)
}

// TestEgressSurfacesLeaseLost verifies that when the Commit fence reports the
// lease was reclaimed mid-send, Receive surfaces ErrLeaseLost so the framework
// retry path takes over. The duplicate send a reclaiming worker already emitted
// is absorbed by server-side MsgId/IdempotencyKey dedup.
func TestEgressSurfacesLeaseLost(t *testing.T) {
	t.Parallel()

	connector, _, _ := newTestConnector(t, nil)

	ax := &fakeEgressExec{commitErr: actor.ErrLeaseLost}
	req := &SendClientEventRequest{
		Message: &testServerMessage{
			value: "evt",
		},
	}

	result := connector.Receive(t.Context(), req, ax)
	require.ErrorIs(t, result.Err(), actor.ErrLeaseLost)
	require.Equal(t, 1, ax.commits)
}

// TestNewRuntimeRejectsNonTxStore verifies the Read/Commit construction guard:
// pairing the TxBehavior connector with a store that is not transaction-aware
// fails fast with ErrTxBehaviorNeedsTxStore rather than panicking at first
// dispatch.
func TestNewRuntimeRejectsNonTxStore(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()
	cfg := newTestConnectorConfig(mb, store)
	cfg.Store = nonTxStore{DeliveryStore: store}

	_, err := NewRuntime(cfg)
	require.ErrorIs(t, err, actor.ErrTxBehaviorNeedsTxStore)
}
