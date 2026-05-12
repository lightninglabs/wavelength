package mailboxrpc

import (
	"encoding/base64"
	"fmt"

	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	// HeaderGRPCStatusB64 is the envelope header key that carries a
	// base64-encoded google.rpc.Status protobuf when an RPC fails on the
	// server side.
	//
	// When present on a KIND_RESPONSE envelope, the receiver should decode
	// the status and surface it as a gRPC error rather than unmarshaling
	// the body.
	HeaderGRPCStatusB64 = "mailboxrpc.grpc_status_b64"
)

// EncodeErrorHeaders serializes err as a gRPC status and returns envelope
// headers that carry it in base64-encoded protobuf form.
//
// Returns nil when err is nil so callers can safely pass the result to
// envelope construction without a nil check.
func EncodeErrorHeaders(err error) map[string]string {
	if err == nil {
		return nil
	}

	// Preserve any existing gRPC status code. For plain errors that
	// don't carry a gRPC status, FromError returns Unknown with the
	// original error message.
	st, _ := status.FromError(err)

	blob, marshalErr := proto.Marshal(st.Proto())
	if marshalErr != nil {
		// Fall back to a minimal Internal status when marshaling itself
		// fails, so callers always get a usable error header.
		fallback := status.New(
			codes.Internal, "failed to marshal error status",
		)
		blob, _ = proto.Marshal(fallback.Proto())
	}

	return map[string]string{
		HeaderGRPCStatusB64: base64.StdEncoding.EncodeToString(blob),
	}
}

// DecodeErrorHeaders returns the gRPC error encoded in headers, or nil if no
// error header is present.
//
// Callers should check the returned error before attempting to unmarshal the
// response body — a non-nil error means the RPC failed on the server.
func DecodeErrorHeaders(headers map[string]string) error {
	if len(headers) == 0 {
		return nil
	}

	b64, ok := headers[HeaderGRPCStatusB64]
	if !ok || b64 == "" {
		return nil
	}

	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("decode grpc status header: %w", err)
	}

	var pb spb.Status
	if err := proto.Unmarshal(blob, &pb); err != nil {
		return fmt.Errorf("unmarshal grpc status proto: %w", err)
	}

	return status.FromProto(&pb).Err()
}
