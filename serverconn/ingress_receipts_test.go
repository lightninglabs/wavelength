package serverconn

import (
	"context"
	"testing"

	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestIngressReceiptScopeAndCompatibility keeps occurrence receipts confined to
// opted-in events and separates configured peers from peer-controlled metadata.
func TestIngressReceiptScopeAndCompatibility(t *testing.T) {
	const occurrence = "delivery-v1:12345678-1234-4234-8234-123456789abc"
	env := &mailboxpb.Envelope{
		MsgId:  occurrence,
		Sender: "sender",
		Rpc: &mailboxpb.RpcMeta{
			Kind:    mailboxpb.RpcMeta_KIND_EVENT,
			Service: "service",
			Method:  "method",
		},
	}
	scope := ingressScope{local: "local", remote: "remote"}
	receipt := func(scope ingressScope, env *mailboxpb.Envelope) string {
		ctx := context.WithValue(
			context.Background(), ingressScopeKey{}, scope,
		)
		ctx, err := withEventReceipt(ctx, env)
		require.NoError(t, err)
		id, _ := mailboxconn.IngressReceiptFromContext(ctx)

		return id
	}
	original := receipt(scope, env)
	require.NotEmpty(t, original)
	for _, changed := range []ingressScope{
		{
			local:  "other",
			remote: "remote",
		},
		{
			local:  "local",
			remote: "other",
		},
	} {
		require.NotEqual(t, original, receipt(changed, env))
	}
	copyEnv, ok := proto.Clone(env).(*mailboxpb.Envelope)
	require.True(t, ok)
	copyEnv.EventSeq = 999
	copyEnv.IdempotencyKey = "fresh-transport-metadata"
	require.Equal(t, original, receipt(scope, copyEnv))
	copyEnv.Rpc.Kind = mailboxpb.RpcMeta_KIND_RESPONSE
	require.Empty(t, receipt(scope, copyEnv))
	copyEnv.Rpc.Kind = mailboxpb.RpcMeta_KIND_REQUEST
	require.Empty(t, receipt(scope, copyEnv))
	copyEnv.Rpc.Kind = mailboxpb.RpcMeta_KIND_EVENT
	for _, id := range []string{
		"", "body-derived",
		"DELIVERY-V1:12345678-1234-4234-8234-123456789abc",
		"delivery-v1:12345678123442348234123456789abc",
		"delivery-v1:12345678-1234-4234-8234-123456789ABC",
	} {
		copyEnv.MsgId = id
		require.Empty(t, receipt(scope, copyEnv), id)
	}
	ctx, err := withEventReceipt(context.Background(), env)
	require.NoError(t, err)
	_, ok = mailboxconn.IngressReceiptFromContext(ctx)
	require.False(t, ok)
}
