package conn

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// TestWrappedProtoRoundTrip verifies that a proto message survives TLV
// encode → decode and retains its field values across input shapes. The
// "stale decode target" row also exercises decoding into a WrappedProto
// that already holds data, replacing the old content.
func TestWrappedProtoRoundTrip(t *testing.T) {
	t.Parallel()

	type testTLV = tlv.TlvType1

	tests := []struct {
		name     string
		encVal   string
		decStale string
		want     string
	}{
		{
			name:   "non-empty payload",
			encVal: "hello-proto",
			want:   "hello-proto",
		},
		{
			name:   "empty payload",
			encVal: "",
			want:   "",
		},
		{
			name:     "overwrites stale decode target",
			encVal:   "new-value",
			decStale: "stale",
			want:     "new-value",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			encRecord := tlv.NewRecordT[testTLV](
				WrappedProto[*wrapperspb.StringValue]{
					Val: &wrapperspb.StringValue{
						Value: tc.encVal,
					},
				},
			)

			var buf bytes.Buffer
			stream, err := tlv.NewStream(encRecord.Record())
			require.NoError(t, err)
			require.NoError(t, stream.Encode(&buf))

			// Decode into a WrappedProto pre-populated with the
			// (possibly stale) concrete proto value.
			decRecord := tlv.ZeroRecordT[testTLV,
				WrappedProto[*wrapperspb.StringValue],
			]()
			decRecord.Val.Val = &wrapperspb.StringValue{
				Value: tc.decStale,
			}

			decStream, err := tlv.NewStream(decRecord.Record())
			require.NoError(t, err)
			require.NoError(t, decStream.Decode(&buf))

			require.Equal(t, tc.want, decRecord.Val.Val.Value)
		})
	}
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
