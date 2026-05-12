package mailbox

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

// testEnvelope builds a minimal mailbox envelope for MemoryStore tests.
func testEnvelope(recipient, msgID, payload string) *Envelope {
	return &Envelope{
		Recipient: recipient,
		MsgId:     msgID,
		Body: &anypb.Any{
			TypeUrl: "type.googleapis.com/test.Payload",
			Value:   []byte(payload),
		},
	}
}

// TestMemoryStoreAppendOversizeDoesNotConsumeSequence verifies that rejected
// oversize envelopes do not advance the mailbox sequence counter.
func TestMemoryStoreAppendOversizeDoesNotConsumeSequence(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore(WithMaxEnvelopeBytes(64))

	ctx := t.Context()
	_, err := store.Append(
		ctx,
		testEnvelope(
			"alice", "msg-1", strings.Repeat("x", 256),
		),
	)
	require.Error(t, err)

	var tooLarge *ErrEnvelopeTooLarge
	require.ErrorAs(t, err, &tooLarge)

	seq, err := store.Append(ctx, testEnvelope("alice", "msg-2", "ok"))
	require.NoError(t, err)
	require.Equal(t, uint64(1), seq)

	envs, nextCursor, err := store.Pull(ctx, "alice", 1, 10)
	require.NoError(t, err)
	require.Len(t, envs, 1)
	require.Equal(t, uint64(1), envs[0].EventSeq)
	require.Equal(t, uint64(2), nextCursor)
}
