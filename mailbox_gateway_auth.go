package darepo

import "google.golang.org/grpc"

// NewGatewayAuthToken returns an unguessable per-process token used to mark
// requests that arrived through the local HTTP gateway.
func NewGatewayAuthToken() (string, error) {
	return newGatewayAuthToken()
}

// GatewayAuthUnaryClientInterceptor marks local gateway-originated gRPC calls.
func GatewayAuthUnaryClientInterceptor(
	token string) grpc.UnaryClientInterceptor {

	return gatewayAuthUnaryClientInterceptor(token)
}

// NewMailboxStreamInterceptor rejects streaming mailbox RPCs until
// stream-level identity checks are explicitly implemented.
func NewMailboxStreamInterceptor() grpc.StreamServerInterceptor {
	return newMailboxStreamInterceptor()
}
