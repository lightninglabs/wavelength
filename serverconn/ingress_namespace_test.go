package serverconn

import (
	"context"
	"testing"

	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/stretchr/testify/require"
)

// recordingQuarantineStore records the lane selected for startup recovery.
type recordingQuarantineStore struct {
	*memCheckpointStore
	listedLanes []string
}

// newRecordingQuarantineStore creates an empty recovery-listing test store.
func newRecordingQuarantineStore() *recordingQuarantineStore {
	return &recordingQuarantineStore{
		memCheckpointStore: newMemCheckpointStore(),
	}
}

// QuarantineIngress satisfies the ingress evidence store contract.
func (s *recordingQuarantineStore) QuarantineIngress(context.Context,
	mailboxconn.IngressQuarantine) error {

	return nil
}

// ListIngressQuarantine records the recovery namespace selected by ingress.
func (s *recordingQuarantineStore) ListIngressQuarantine(_ context.Context,
	lane string) ([]mailboxconn.IngressQuarantine, error) {

	s.listedLanes = append(s.listedLanes, lane)

	return nil, nil
}

// NoteIngressQuarantineAttempt satisfies the ingress evidence store contract.
func (s *recordingQuarantineStore) NoteIngressQuarantineAttempt(context.Context,
	string) error {

	return nil
}

// DeleteIngressQuarantine satisfies the ingress evidence store contract.
func (s *recordingQuarantineStore) DeleteIngressQuarantine(context.Context,
	string) error {

	return nil
}

// scopedReceiptID derives an occurrence receipt through the connector's
// configured ingress-evidence namespace.
func scopedReceiptID(t *testing.T, conn *ServerConnectionActor,
	env *mailboxpb.Envelope) string {

	t.Helper()
	ctx := context.WithValue(
		t.Context(), ingressScopeKey{}, conn.ingressEvidenceScope(),
	)
	ctx, err := withEventReceipt(ctx, env)
	require.NoError(t, err)
	id, ok := mailboxconn.IngressReceiptFromContext(ctx)
	require.True(t, ok)

	return id
}

// TestIngressEvidenceUsesReplyMailboxNamespace proves independent reply
// streams cannot alias receipts, quarantine limits, or recovery listings when
// their authenticated local and remote identities are identical.
func TestIngressEvidenceUsesReplyMailboxNamespace(t *testing.T) {
	t.Parallel()

	env := &mailboxpb.Envelope{
		MsgId:  "delivery-v1:12345678-1234-4234-8234-123456789abc",
		Sender: "shared-remote",
		Rpc: &mailboxpb.RpcMeta{
			Kind:    mailboxpb.RpcMeta_KIND_EVENT,
			Service: "test.Service",
			Method:  "Event",
		},
	}
	store := newRecordingQuarantineStore()
	newConnector := func(reply string) *ServerConnectionActor {
		cfg := DefaultConnectorConfig()
		cfg.LocalMailboxID = "shared-local"
		cfg.RemoteMailboxID = "shared-remote"
		cfg.ReplyMailboxID = reply
		cfg.Store = store

		return NewServerConnectionActor(cfg)
	}

	first := newConnector("reply-one")
	second := newConnector("reply-two")
	firstLane := first.quarantineLane()
	secondLane := second.quarantineLane()

	require.NotEqual(
		t, scopedReceiptID(t, first, env),
		scopedReceiptID(t, second, env),
	)
	require.NotEqual(t, firstLane, secondLane)

	first.retryQuarantinedIngress(t.Context(), store)
	second.retryQuarantinedIngress(t.Context(), store)
	require.Equal(t, []string{firstLane, secondLane}, store.listedLanes)
}

// TestValidateInboundEnvelopeReplyMailbox rejects explicitly misaddressed
// envelopes while preserving compatibility with legacy envelopes that omitted
// the recipient field.
func TestValidateInboundEnvelopeReplyMailbox(t *testing.T) {
	t.Parallel()

	cfg := DefaultConnectorConfig()
	cfg.LocalMailboxID = "shared-local"
	cfg.RemoteMailboxID = "shared-remote"
	cfg.ReplyMailboxID = "reply-one"
	cfg.ArkProtocolVersion = 1
	conn := NewServerConnectionActor(cfg)
	env := &mailboxpb.Envelope{
		ProtocolVersion:    cfg.MailboxProtocolVersion,
		ArkProtocolVersion: cfg.ArkProtocolVersion,
		Recipient:          "reply-two",
	}

	err := conn.validateInboundEnvelope(env)
	require.ErrorContains(t, err, "does not match reply mailbox")

	env.Recipient = cfg.ReplyMailboxID
	require.NoError(t, conn.validateInboundEnvelope(env))

	env.Recipient = ""
	require.NoError(t, conn.validateInboundEnvelope(env))
}
