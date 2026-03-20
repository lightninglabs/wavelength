package harness

import (
	"context"
	"fmt"
	"testing"

	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
)

type recordingMailboxClient struct {
	sends []*mailboxpb.Envelope
	fail  error
}

func (m *recordingMailboxClient) Send(_ context.Context,
	in *mailboxpb.SendRequest, _ ...grpc.CallOption) (
	*mailboxpb.SendResponse, error) {

	if m.fail != nil {
		return nil, m.fail
	}

	m.sends = append(m.sends, in.Envelope)

	return &mailboxpb.SendResponse{}, nil
}

func (m *recordingMailboxClient) Pull(context.Context,
	*mailboxpb.PullRequest, ...grpc.CallOption) (
	*mailboxpb.PullResponse, error) {

	return &mailboxpb.PullResponse{}, nil
}

func (m *recordingMailboxClient) AckUpTo(context.Context,
	*mailboxpb.AckUpToRequest, ...grpc.CallOption) (
	*mailboxpb.AckUpToResponse, error) {

	return &mailboxpb.AckUpToResponse{}, nil
}

func testEnvelope(t *testing.T, typeURL string) *mailboxpb.Envelope {
	t.Helper()

	return &mailboxpb.Envelope{
		Body: &anypb.Any{TypeUrl: typeURL},
	}
}

// TestControlledMailboxClientBuffersPausedType verifies paused message types
// are buffered until the harness explicitly flushes them.
func TestControlledMailboxClientBuffersPausedType(t *testing.T) {
	t.Parallel()

	inner := &recordingMailboxClient{}
	client := NewControlledMailboxClient()
	client.SetInner(inner)
	client.PauseType("FinalizePackageRequest")

	_, err := client.Send(t.Context(), &mailboxpb.SendRequest{
		Envelope: testEnvelope(
			t,
			"type.googleapis.com/oorpb.FinalizePackageRequest",
		),
	})
	require.NoError(t, err)
	require.Empty(t, inner.sends)
	require.Equal(t, 1, client.PendingTypeCount("FinalizePackageRequest"))

	require.NoError(t, client.FlushAll())
	require.Len(t, inner.sends, 1)
	require.Equal(t,
		"type.googleapis.com/oorpb.FinalizePackageRequest",
		inner.sends[0].Body.TypeUrl,
	)
}

// TestControlledMailboxClientPassesThroughOtherTypes verifies non-paused
// message types still reach the wrapped mailbox client immediately.
func TestControlledMailboxClientPassesThroughOtherTypes(t *testing.T) {
	t.Parallel()

	inner := &recordingMailboxClient{}
	client := NewControlledMailboxClient()
	client.SetInner(inner)
	client.PauseType("FinalizePackageRequest")

	_, err := client.Send(t.Context(), &mailboxpb.SendRequest{
		Envelope: testEnvelope(
			t,
			"type.googleapis.com/oorpb.SubmitPackageRequest",
		),
	})
	require.NoError(t, err)
	require.Len(t, inner.sends, 1)
	require.Zero(t, client.PendingCount())
}

// TestControlledMailboxClientFlushAllRetainsFailedEnvelope verifies a failed
// flush does not drop the still-buffered message.
func TestControlledMailboxClientFlushAllRetainsFailedEnvelope(t *testing.T) {
	t.Parallel()

	inner := &recordingMailboxClient{
		fail: fmt.Errorf("operator unavailable"),
	}
	client := NewControlledMailboxClient()
	client.SetInner(inner)
	client.PauseType("FinalizePackageRequest")

	_, err := client.Send(t.Context(), &mailboxpb.SendRequest{
		Envelope: testEnvelope(
			t,
			"type.googleapis.com/oorpb.FinalizePackageRequest",
		),
	})
	require.NoError(t, err)
	require.Equal(t, 1, client.PendingTypeCount("FinalizePackageRequest"))

	err = client.FlushAll()
	require.ErrorContains(t, err, "operator unavailable")
	require.Equal(t, 1, client.PendingTypeCount("FinalizePackageRequest"))
	require.Empty(t, inner.sends)
}
