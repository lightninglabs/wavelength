package mailboxrpc

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestEncodeErrorHeaders_NilError verifies that EncodeErrorHeaders returns nil
// when given a nil error, so callers can safely pass the result without a nil
// check.
func TestEncodeErrorHeaders_NilError(t *testing.T) {
	t.Parallel()

	require.Nil(t, EncodeErrorHeaders(nil))
}

// TestEncodeDecodeRoundTrip verifies that a gRPC status error survives the
// encode → header → decode round-trip with code and message preserved.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		code codes.Code
		msg  string
	}{
		{
			name: "plain error becomes Unknown",
			err:  errors.New("something broke"),
			code: codes.Unknown,
			msg:  "something broke",
		},
		{
			name: "NotFound status preserved",
			err:  status.Error(codes.NotFound, "no such item"),
			code: codes.NotFound,
			msg:  "no such item",
		},
		{
			name: "PermissionDenied status preserved",
			err: status.Error(
				codes.PermissionDenied, "access denied",
			),
			code: codes.PermissionDenied,
			msg:  "access denied",
		},
		{
			name: "Internal status preserved",
			err:  status.Error(codes.Internal, "server fault"),
			code: codes.Internal,
			msg:  "server fault",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			headers := EncodeErrorHeaders(tc.err)
			require.NotNil(t, headers)
			require.Contains(t, headers, HeaderGRPCStatusB64)

			decoded := DecodeErrorHeaders(headers)
			require.Error(t, decoded)

			st, ok := status.FromError(decoded)
			require.True(t, ok)
			require.Equal(t, tc.code, st.Code())
			require.Equal(t, tc.msg, st.Message())
		})
	}
}

// TestDecodeErrorHeaders_NilAndEmpty verifies that DecodeErrorHeaders returns
// nil for nil maps, empty maps, and maps without the status header key.
func TestDecodeErrorHeaders_NilAndEmpty(t *testing.T) {
	t.Parallel()

	require.NoError(t, DecodeErrorHeaders(nil))
	require.NoError(t, DecodeErrorHeaders(map[string]string{}))
	require.NoError(
		t,
		DecodeErrorHeaders(
			map[string]string{
				"other-key": "value",
			},
		),
	)
}

// TestDecodeErrorHeaders_EmptyValue verifies that an empty string value for
// the status header key is treated as absent.
func TestDecodeErrorHeaders_EmptyValue(t *testing.T) {
	t.Parallel()

	err := DecodeErrorHeaders(map[string]string{
		HeaderGRPCStatusB64: "",
	})
	require.NoError(t, err)
}

// TestDecodeErrorHeaders_InvalidBase64 verifies that malformed base64 in the
// header produces a descriptive error rather than a panic.
func TestDecodeErrorHeaders_InvalidBase64(t *testing.T) {
	t.Parallel()

	err := DecodeErrorHeaders(map[string]string{
		HeaderGRPCStatusB64: "not-valid-base64!@#$",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "decode grpc status header")
}

// TestDecodeErrorHeaders_InvalidProto verifies that valid base64 containing
// garbage proto bytes produces a descriptive unmarshal error.
func TestDecodeErrorHeaders_InvalidProto(t *testing.T) {
	t.Parallel()

	// Valid base64 but not a valid protobuf Status message. Use a byte
	// sequence that base64-encodes cleanly but contains invalid proto
	// field tags.
	err := DecodeErrorHeaders(map[string]string{
		HeaderGRPCStatusB64: "////",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unmarshal grpc status proto")
}

// TestEncodeErrorHeaders_HeaderKeyPresent verifies the header map always uses
// the canonical HeaderGRPCStatusB64 key.
func TestEncodeErrorHeaders_HeaderKeyPresent(t *testing.T) {
	t.Parallel()

	headers := EncodeErrorHeaders(errors.New("test"))
	require.Len(t, headers, 1)

	_, ok := headers[HeaderGRPCStatusB64]
	require.True(t, ok)
}
