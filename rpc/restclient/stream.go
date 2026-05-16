package restclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type streamChunk struct {
	Result json.RawMessage `json:"result"`
	Error  *GatewayError   `json:"error"`
}

// StreamClient adapts grpc-gateway's chunked JSON stream shape to the gRPC
// ServerStreamingClient interface. Browser/WASM callers should validate
// long-lived streams in their target runtime because Fetch implementations may
// buffer response bodies instead of exposing chunks incrementally.
type StreamClient[T any] struct {
	resp *http.Response
	dec  *json.Decoder
	name string
	new  func() *T
}

// NewStreamClient creates a typed gRPC-shaped client for one REST stream.
func NewStreamClient[T any](resp *http.Response, name string,
	newMsg func() *T) *StreamClient[T] {

	return &StreamClient[T]{
		resp: resp,
		dec:  json.NewDecoder(resp.Body),
		name: name,
		new:  newMsg,
	}
}

// Recv returns the next message from the REST stream.
func (c *StreamClient[T]) Recv() (*T, error) {
	for {
		var chunk streamChunk
		if err := c.dec.Decode(&chunk); err != nil {
			_ = c.resp.Body.Close()

			return nil, err
		}

		if chunk.Error != nil {
			code := codeFromHTTPStatus(c.resp.StatusCode)
			if len(chunk.Error.Code) > 0 {
				code = codeFromJSON(chunk.Error.Code, code)
			}

			_ = c.resp.Body.Close()

			return nil, status.Error(code, chunk.Error.Message)
		}
		if len(chunk.Result) == 0 {
			continue
		}

		msg := c.new()
		protoMsg, ok := any(msg).(proto.Message)
		if !ok {
			_ = c.resp.Body.Close()

			return nil, fmt.Errorf("stream message %T is not proto",
				msg)
		}
		if err := jsonUnmarshal.Unmarshal(
			chunk.Result, protoMsg,
		); err != nil {

			_ = c.resp.Body.Close()

			return nil, err
		}

		return msg, nil
	}
}

// Header returns response headers exposed as gRPC metadata.
func (c *StreamClient[T]) Header() (metadata.MD, error) {
	return metadata.MD{}, nil
}

// Trailer returns response trailers exposed as gRPC metadata.
func (c *StreamClient[T]) Trailer() metadata.MD {
	return metadata.MD{}
}

// CloseSend closes the underlying response body.
func (c *StreamClient[T]) CloseSend() error {
	return c.resp.Body.Close()
}

// Context returns the HTTP request context.
func (c *StreamClient[T]) Context() context.Context {
	return c.resp.Request.Context()
}

// SendMsg is unsupported for grpc-gateway server streams.
func (c *StreamClient[T]) SendMsg(_ any) error {
	return unsupportedSendMsg(c.name)
}

// RecvMsg receives into the supplied protobuf message.
func (c *StreamClient[T]) RecvMsg(m any) error {
	msg, ok := m.(proto.Message)
	if !ok {
		return fmt.Errorf("unexpected RecvMsg target %T", m)
	}

	resp, err := c.Recv()
	if err != nil {
		return err
	}
	respMsg, ok := any(resp).(proto.Message)
	if !ok {
		return fmt.Errorf("received stream message %T is not proto",
			resp)
	}
	proto.Reset(msg)
	proto.Merge(msg, respMsg)

	return nil
}
