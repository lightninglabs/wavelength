// Package mailboxrpc defines the runtime interfaces used by generated
// RPC-over-mailbox stubs.
//
// The mailbox transport provides durability across unreliable networks via a
// small edge surface (Send/Pull/Ack*). This package intentionally does not
// include a transport implementation; it provides only the narrow contracts
// needed by generated code so both clients and servers can depend on it.
package mailboxrpc
