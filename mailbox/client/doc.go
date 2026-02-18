// Package mailboxclient provides a concrete implementation of the
// mailboxrpc.RPCClient interface backed by the mailbox edge gRPC API
// (mailboxpb.MailboxService).
//
// The RPC client in this package focuses on:
//   - encoding requests into mailboxpb.Envelope values,
//   - receiving correlated responses by long-polling Pull, and
//   - preventing response loss under cursor-based acking by caching pulled
//     responses by correlation id before advancing the remote cursor.
//
// This package is intentionally small and self-contained so it can be used in
// both production code and tests.
//
// For crash safety, the Client can be configured with a Store implementation
// that persists:
//   - the remote Pull cursor, and
//   - response payloads keyed by correlation id.
//
// This is sufficient to avoid response loss when using cursor-based acking,
// even across restarts, as long as callers reuse correlation ids when retrying.
//
// This package does not attempt to implement the full “local durability ↔
// remote mailbox” connector described in the spec (for example, integrating a
// transactional outbox with a local FSM store). That integration belongs at a
// higher layer that has access to the application's durability boundaries.
package mailboxclient
