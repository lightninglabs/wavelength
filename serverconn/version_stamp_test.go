package serverconn

import (
	"context"
	"testing"

	mailboxconn "github.com/lightninglabs/darepo-client/mailbox/conn"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// sentEnvelopes returns a snapshot of the envelopes delivered to the given
// recipient mailbox.
func sentEnvelopes(mb *inMemoryMailbox,
	recipient string) []*mailboxpb.Envelope {

	mb.mu.Lock()
	defer mb.mu.Unlock()

	out := make([]*mailboxpb.Envelope, len(mb.mailboxes[recipient]))
	copy(out, mb.mailboxes[recipient])

	return out
}

// TestStampEnvelopeOverwrites proves the shared stamping helper writes the
// bound version pair onto an envelope and overwrites any pre-existing
// caller-provided version values, so no send path can rely on a caller version.
func TestStampEnvelopeOverwrites(t *testing.T) {
	t.Parallel()

	cfg := ConnectorConfig{
		MailboxProtocolVersion: 1,
		ArkProtocolVersion:     2,
	}

	// A caller-provided envelope with bogus versions must be overwritten.
	env := &mailboxpb.Envelope{
		ProtocolVersion:    99,
		ArkProtocolVersion: 99,
	}
	cfg.stampEnvelope(env)
	require.Equal(t, uint32(1), env.ProtocolVersion)
	require.Equal(t, uint32(2), env.ArkProtocolVersion)

	// A nil envelope must not panic.
	cfg.stampEnvelope(nil)
}

// TestOutboundEnvelopesCarryBothVersions drives every client send path through
// the actor with a synthetic Ark v2 binding and proves each emitted envelope
// carries both the mailbox transport version and the Ark protocol version.
// Covers event, durable unary request, pre-built replay, and heartbeat
// envelopes. Synthetic v2 is configured only on this test connector, never in
// a production default.
func TestOutboundEnvelopesCarryBothVersions(t *testing.T) {
	t.Parallel()

	const (
		wantMailboxVersion = uint32(1)
		wantArkVersion     = uint32(2)
	)

	mb := newInMemoryMailbox()
	cfg := newTestConnectorConfig(mb, newMemCheckpointStore())

	// Bind a synthetic Ark v2 to prove stamping is not pinned to v1.
	cfg.ArkProtocolVersion = wantArkVersion
	cfg.MailboxProtocolVersion = wantMailboxVersion

	conn := NewServerConnectionActor(cfg)
	ctx := t.Context()

	// Event envelope (durable client FSM outbox message).
	res := conn.Receive(ctx, &SendClientEventRequest{
		Message: &testServerMessage{value: "event"},
	})
	require.NoError(t, res.Err())

	// Durable unary request envelope.
	unaryReq, err := NewSendUnaryRequest(
		mailboxrpc.ServiceMethod{
			Service: testEventService,
			Method:  testEventMethod,
		},
		wrapperspb.String("unary"), "corr-1",
	)
	require.NoError(t, err)
	require.NoError(t, conn.Receive(ctx, unaryReq).Err())

	// Pre-built (replay) envelope: deliberately constructed with zero
	// versions to prove the re-stamp path overwrites a persisted value.
	body, err := anypb.New(wrapperspb.String("replay"))
	require.NoError(t, err)
	prebuilt := &SendRPCRequest{
		Envelope: &mailboxpb.Envelope{
			ProtocolVersion:    0,
			ArkProtocolVersion: 0,
			Recipient:          cfg.RemoteMailboxID,
			Body:               body,
			Rpc: &mailboxpb.RpcMeta{
				Kind:    mailboxpb.RpcMeta_KIND_REQUEST,
				Service: testEventService,
				Method:  testEventMethod,
			},
		},
	}
	require.NoError(t, conn.Receive(ctx, prebuilt).Err())

	// Heartbeat envelope.
	conn.sendHeartbeat(ctx)

	// Every envelope delivered to the remote mailbox must carry both
	// versions.
	envs := sentEnvelopes(mb, cfg.RemoteMailboxID)
	require.Len(t, envs, 4)
	for i, env := range envs {
		require.Equalf(
			t, wantMailboxVersion, env.ProtocolVersion, "mailbox"+
				" version on envelope %d", i,
		)
		require.Equalf(
			t, wantArkVersion, env.ArkProtocolVersion, "ark "+
				"version on envelope %d", i,
		)
	}
}

// TestRuntimeStampEnvelopeResponse proves the exported Runtime.StampEnvelope
// entry point (used by the darepod mailbox response path) stamps a response
// envelope with the runtime-bound pair rather than any caller value.
func TestRuntimeStampEnvelopeResponse(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	cfg := newTestConnectorConfig(mb, newMemCheckpointStore())
	cfg.ArkProtocolVersion = 2

	rt, err := NewRuntime(cfg)
	require.NoError(t, err)

	responseEnv := &mailboxpb.Envelope{
		ProtocolVersion:    7,
		ArkProtocolVersion: 7,
		Rpc: &mailboxpb.RpcMeta{
			Kind: mailboxpb.RpcMeta_KIND_RESPONSE,
		},
	}
	rt.StampEnvelope(responseEnv)
	require.Equal(t, uint32(1), responseEnv.ProtocolVersion)
	require.Equal(t, uint32(2), responseEnv.ArkProtocolVersion)
}

// TestValidateInboundEnvelope covers the inbound validation matrix: a matching
// pair passes; a transport mismatch, a non-zero Ark mismatch, and a zero Ark
// version (no legacy fallback) all fail. There is no legacy mode.
func TestValidateInboundEnvelope(t *testing.T) {
	t.Parallel()

	const (
		codeMbx = mailboxconn.StatusMailboxVersionUnsupported
		codeArk = mailboxconn.StatusArkVersionMismatch
	)

	tests := []struct {
		name        string
		boundArk    uint32
		envMailbox  uint32
		envArk      uint32
		wantErr     bool
		wantErrCode string
	}{
		{
			name:       "matching v1",
			boundArk:   1,
			envMailbox: 1,
			envArk:     1,
		},
		{
			name:        "transport mismatch",
			boundArk:    1,
			envMailbox:  2,
			envArk:      1,
			wantErr:     true,
			wantErrCode: codeMbx,
		},
		{
			name:        "ark mismatch nonzero",
			boundArk:    1,
			envMailbox:  1,
			envArk:      2,
			wantErr:     true,
			wantErrCode: codeArk,
		},
		{
			name:        "ark zero is mismatch",
			boundArk:    1,
			envMailbox:  1,
			envArk:      0,
			wantErr:     true,
			wantErrCode: codeArk,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mb := newInMemoryMailbox()
			cfg := newTestConnectorConfig(
				mb, newMemCheckpointStore(),
			)
			cfg.ArkProtocolVersion = tc.boundArk
			cfg.MailboxProtocolVersion = 1

			conn := NewServerConnectionActor(cfg)
			env := &mailboxpb.Envelope{
				ProtocolVersion:    tc.envMailbox,
				ArkProtocolVersion: tc.envArk,
			}

			err := conn.validateInboundEnvelope(env)
			if !tc.wantErr {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			require.True(
				t, mailboxconn.IsPermanentVersionError(err),
			)

			var statusErr *mailboxconn.StatusError
			require.ErrorAs(t, err, &statusErr)
			require.Equal(t, tc.wantErrCode, statusErr.Code())
		})
	}
}

// TestDispatchBatchRejectsMismatchedEnvelope proves a mismatched inbound
// envelope is neither dispatched nor acknowledged: dispatchBatch returns an
// error, the dispatcher is never invoked, and the committed cursor does not
// advance past the rejected envelope.
func TestDispatchBatchRejectsMismatchedEnvelope(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	cfg := newTestConnectorConfig(mb, newMemCheckpointStore())
	cfg.ArkProtocolVersion = 1
	cfg.MailboxProtocolVersion = 1

	dispatched := false
	svcMethod := mailboxrpc.ServiceMethod{
		Service: testEventService,
		Method:  testEventMethod,
	}
	var disp EnvelopeDispatcher = func(context.Context,
		*mailboxpb.Envelope) error {

		dispatched = true

		return nil
	}
	cfg.Dispatchers = map[mailboxrpc.ServiceMethod]EnvelopeDispatcher{
		svcMethod: disp,
	}

	conn := NewServerConnectionActor(cfg)

	// An envelope bound for the dispatcher but carrying a mismatched Ark
	// version must be rejected before dispatch.
	env := &mailboxpb.Envelope{
		ProtocolVersion:    1,
		ArkProtocolVersion: 2,
		EventSeq:           5,
		Rpc: &mailboxpb.RpcMeta{
			Kind:    mailboxpb.RpcMeta_KIND_EVENT,
			Service: testEventService,
			Method:  testEventMethod,
		},
	}

	committed, err := conn.dispatchBatch(
		t.Context(), []*mailboxpb.Envelope{env}, 6,
	)
	require.Error(t, err)
	require.True(t, mailboxconn.IsPermanentVersionError(err))
	require.False(t, dispatched, "mismatched envelope was dispatched")

	// The committed cursor must not advance past the rejected envelope, so
	// the ack watermark cannot move and the envelope is preserved.
	require.Less(t, committed, uint64(5))
}
