// Package indexer implements the operator-side mailbox indexer service.
//
// The indexer is intended to be a wallet-scoped query/notification facade
// transported over MailboxRPC. It is explicitly not designed to be a public
// explorer: all queries are authenticated (via proof-of-control) and routed
// over a durable mailbox transport.
package indexer
