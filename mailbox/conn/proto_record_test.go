package conn

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// TestWrappedProto_RoundTrip verifies that a proto message survives TLV
// encode → decode and retains its field values.
func TestWrappedProto_RoundTrip(t *testing.T) {
	t.Parallel()

	original := &wrapperspb.StringValue{Value: "hello-proto"}

	type testTLV = tlv.TlvType1

	encRecord := tlv.NewRecordT[testTLV](
		WrappedProto[*wrapperspb.StringValue]{
			Val: original,
		},
	)

	var buf bytes.Buffer
	stream, err := tlv.NewStream(encRecord.Record())
	require.NoError(t, err)
	require.NoError(t, stream.Encode(&buf))

	// Decode into a fresh WrappedProto with a pre-allocated zero
	// value for the concrete proto type.
	decRecord := tlv.ZeroRecordT[testTLV,
		WrappedProto[*wrapperspb.StringValue],
	]()
	decRecord.Val.Val = &wrapperspb.StringValue{}

	decStream, err := tlv.NewStream(decRecord.Record())
	require.NoError(t, err)
	require.NoError(t, decStream.Decode(&buf))

	require.Equal(t, "hello-proto", decRecord.Val.Val.Value)
}

// TestWrappedProto_EmptyPayload verifies that encoding an empty proto
// produces a decodable payload that round-trips cleanly.
func TestWrappedProto_EmptyPayload(t *testing.T) {
	t.Parallel()

	type testTLV = tlv.TlvType2

	original := &wrapperspb.StringValue{}
	encRecord := tlv.NewRecordT[testTLV](
		WrappedProto[*wrapperspb.StringValue]{
			Val: original,
		},
	)

	var buf bytes.Buffer
	stream, err := tlv.NewStream(encRecord.Record())
	require.NoError(t, err)
	require.NoError(t, stream.Encode(&buf))

	decRecord := tlv.ZeroRecordT[testTLV,
		WrappedProto[*wrapperspb.StringValue],
	]()
	decRecord.Val.Val = &wrapperspb.StringValue{}

	decStream, err := tlv.NewStream(decRecord.Record())
	require.NoError(t, err)
	require.NoError(t, decStream.Decode(&buf))

	require.Equal(t, "", decRecord.Val.Val.Value)
}

// TestWrappedProto_NilValue verifies that isNil correctly detects a nil
// proto value inside the generic wrapper.
func TestWrappedProto_NilValue(t *testing.T) {
	t.Parallel()

	wp := WrappedProto[*wrapperspb.StringValue]{}
	require.True(t, isNil(wp.Val))

	wp.Val = &wrapperspb.StringValue{Value: "set"}
	require.False(t, isNil(wp.Val))
}

// TestWrappedProto_NilEncodes verifies that a nil proto value produces
// a valid (empty) TLV encoding without errors.
func TestWrappedProto_NilEncodes(t *testing.T) {
	t.Parallel()

	type testTLV = tlv.TlvType4

	encRecord := tlv.NewRecordT[testTLV](
		WrappedProto[*wrapperspb.StringValue]{},
	)

	var buf bytes.Buffer
	stream, err := tlv.NewStream(encRecord.Record())
	require.NoError(t, err)
	require.NoError(t, stream.Encode(&buf))

	// Should produce a valid but minimal TLV encoding.
	require.Greater(t, buf.Len(), 0)
}

// TestWrappedProto_DecodeOverwritesPreviousState verifies that decoding
// into a WrappedProto that already holds data replaces the old content.
func TestWrappedProto_DecodeOverwritesPreviousState(t *testing.T) {
	t.Parallel()

	type testTLV = tlv.TlvType3

	// Encode "new-value".
	encRecord := tlv.NewRecordT[testTLV](
		WrappedProto[*wrapperspb.StringValue]{
			Val: &wrapperspb.StringValue{Value: "new-value"},
		},
	)

	var buf bytes.Buffer
	stream, err := tlv.NewStream(encRecord.Record())
	require.NoError(t, err)
	require.NoError(t, stream.Encode(&buf))

	// Pre-populate the decode target with stale data.
	decRecord := tlv.ZeroRecordT[testTLV,
		WrappedProto[*wrapperspb.StringValue],
	]()
	decRecord.Val.Val = &wrapperspb.StringValue{Value: "stale"}

	decStream, err := tlv.NewStream(decRecord.Record())
	require.NoError(t, err)
	require.NoError(t, decStream.Decode(&buf))

	require.Equal(t, "new-value", decRecord.Val.Val.Value)
}
