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

// TestDecodeErrorHeaders verifies that DecodeErrorHeaders treats nil, empty,
// missing-key, and empty-value maps as absent (no error), and returns a
// descriptive error for malformed base64 or garbage proto bytes.
func TestDecodeErrorHeaders(t *testing.T) {
	t.Parallel()

	hdr := func(v string) map[string]string {
		return map[string]string{HeaderGRPCStatusB64: v}
	}

	tests := []struct {
		name    string
		headers map[string]string
		wantErr string
	}{
		{
			name:    "nil map",
			headers: nil,
		},
		{
			name:    "empty map",
			headers: map[string]string{},
		},
		{
			name: "missing key",
			headers: map[string]string{
				"x": "v",
			},
		},
		{
			name:    "empty value",
			headers: hdr(""),
		},

		// Malformed base64 cannot be decoded at all.
		{
			name:    "invalid base64",
			headers: hdr("not-valid-base64!@#$"),
			wantErr: "decode grpc status header",
		},

		// Valid base64 that decodes to bytes which are not a valid
		// protobuf Status message.
		{
			name:    "invalid proto",
			headers: hdr("////"),
			wantErr: "unmarshal grpc status proto",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := DecodeErrorHeaders(tc.headers)
			if tc.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
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
