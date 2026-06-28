package mailboxpb

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestEnvelopeOldShapeDecodesZero proves that an Envelope encoded without the
// new ark_protocol_version field decodes under the new generated code with
// that field left at its zero value, while the existing mailbox transport
// protocol_version is preserved. This is the additive-compatibility guarantee
// that lets the updated code decode envelopes produced before the Ark
// protocol version field existed.
func TestEnvelopeOldShapeDecodesZero(t *testing.T) {
	t.Parallel()

	// Populate only fields that existed before ark_protocol_version was
	// added.
	oldShape := &Envelope{
		ProtocolVersion: uint32(1),
		MsgId:           "msg-1",
		IdempotencyKey:  "idem-1",
		Sender:          "alice",
		Recipient:       "bob",
		EventSeq:        7,
	}

	raw, err := proto.Marshal(oldShape)
	require.NoError(t, err)

	decoded := &Envelope{}
	require.NoError(t, proto.Unmarshal(raw, decoded))

	// The mailbox transport version round-trips unchanged.
	require.Equal(t, uint32(1), decoded.ProtocolVersion)
	require.Equal(t, "msg-1", decoded.MsgId)
	require.Equal(t, uint64(7), decoded.EventSeq)

	// The newly added Ark protocol version must be zero after decoding an
	// old-shape envelope.
	require.Zero(t, decoded.ArkProtocolVersion)
}

// TestStatusOldShapeDecodesZero proves that a Status encoded with only the
// pre-versioning fields decodes under the new code with the newly added
// supported-version lists empty.
func TestStatusOldShapeDecodesZero(t *testing.T) {
	t.Parallel()

	oldShape := &Status{
		Ok:      false,
		Code:    "SOME_CODE",
		Message: "human readable",
	}

	raw, err := proto.Marshal(oldShape)
	require.NoError(t, err)

	decoded := &Status{}
	require.NoError(t, proto.Unmarshal(raw, decoded))

	require.Equal(t, "SOME_CODE", decoded.Code)
	require.Equal(t, "human readable", decoded.Message)

	require.Empty(t, decoded.SupportedMailboxVersions)
	require.Empty(t, decoded.SupportedArkVersions)
}

// TestEnvelopeVersionFieldsRoundTrip proves that both version axes on the
// Envelope are independently preserved across a marshal/unmarshal cycle.
func TestEnvelopeVersionFieldsRoundTrip(t *testing.T) {
	t.Parallel()

	env := &Envelope{
		ProtocolVersion:    uint32(1),
		ArkProtocolVersion: 2,
		MsgId:              "msg-2",
	}

	raw, err := proto.Marshal(env)
	require.NoError(t, err)

	decoded := &Envelope{}
	require.NoError(t, proto.Unmarshal(raw, decoded))

	require.Equal(t, uint32(1), decoded.ProtocolVersion)
	require.Equal(t, uint32(2), decoded.ArkProtocolVersion)
	require.Equal(t, "msg-2", decoded.MsgId)
}

// TestStatusVersionFieldsRoundTrip proves that the new supported-version lists
// on Status are preserved across a marshal/unmarshal cycle so callers can
// surface actionable guidance from a permanent version error.
func TestStatusVersionFieldsRoundTrip(t *testing.T) {
	t.Parallel()

	status := &Status{
		Ok:      false,
		Code:    "ARK_VERSION_UNSUPPORTED",
		Message: "unsupported",
		SupportedMailboxVersions: []uint32{
			uint32(1),
		},
		SupportedArkVersions: []uint32{
			1,
			2,
		},
	}

	raw, err := proto.Marshal(status)
	require.NoError(t, err)

	decoded := &Status{}
	require.NoError(t, proto.Unmarshal(raw, decoded))

	require.Equal(t, "ARK_VERSION_UNSUPPORTED", decoded.Code)
	require.Equal(
		t, []uint32{uint32(1)}, decoded.SupportedMailboxVersions,
	)
	require.Equal(t, []uint32{1, 2}, decoded.SupportedArkVersions)
}
