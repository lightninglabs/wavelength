package mailboxrpc_test

import (
	"context"
	"errors"
	"testing"

	"github.com/lightninglabs/wavelength/arkrpc"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"google.golang.org/protobuf/proto"
)

// TestServeMuxDispatch verifies a handler is invoked after unmarshalling.
func TestServeMuxDispatch(t *testing.T) {
	t.Parallel()

	mux := mailboxrpc.NewServeMux()
	mux.Handle(arkrpcServiceName, "GetInfo", func() proto.Message {
		return new(arkrpc.GetInfoRequest)
	}, func(_ context.Context, msg proto.Message) (proto.Message, error) {
		_, ok := msg.(*arkrpc.GetInfoRequest)
		if !ok {
			t.Fatalf("unexpected request type: %T", msg)
		}

		return &arkrpc.GetInfoResponse{
			Version: "test",
			Pubkey: []byte{
				1,
				2,
				3,
			},
			Network:     "regtest",
			BlockHeight: 7,
		}, nil
	})

	reqBytes, err := proto.Marshal(&arkrpc.GetInfoRequest{})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	respMsg, err := mux.ServeRPC(
		t.Context(), arkrpcServiceName, "GetInfo", reqBytes,
	)
	if err != nil {
		t.Fatalf("ServeRPC: %v", err)
	}

	resp, ok := respMsg.(*arkrpc.GetInfoResponse)
	if !ok {
		t.Fatalf("unexpected response type: %T", respMsg)
	}

	if resp.GetVersion() != "test" {
		t.Fatalf("unexpected version: %q", resp.GetVersion())
	}
}

// TestServeMuxMissingHandler verifies unknown routes fail with ErrNoHandler.
func TestServeMuxMissingHandler(t *testing.T) {
	t.Parallel()

	mux := mailboxrpc.NewServeMux()

	_, err := mux.ServeRPC(
		t.Context(), arkrpcServiceName, "Missing", nil,
	)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, mailboxrpc.ErrNoHandler) {
		t.Fatalf("expected ErrNoHandler, got %v", err)
	}
}

const arkrpcServiceName = "arkrpc.ArkService"
