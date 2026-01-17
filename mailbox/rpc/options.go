package mailboxrpc

// RPCOptions configures a single RPC-over-mailbox request.
type RPCOptions struct {
	// IdempotencyKey overrides the derived idempotency key.
	//
	// This must be stable per semantic operation (not per send attempt).
	IdempotencyKey string

	// CorrelationID overrides the default correlation id.
	//
	// CorrelationID is used to match responses to requests. When unset,
	// callers typically use the idempotency key as the correlation id.
	CorrelationID string

	// Headers are copied into the request envelope headers.
	Headers map[string]string
}
