//go:build swapruntime

// Package swapclientserver hosts the optional daemon-side swap client
// subserver.
//
// The package is built only with the swapruntime tag. It translates
// swapclientrpc control-plane calls into sdk/swaps operations, owns the
// daemon-local worker registry, and resumes persisted pending swaps when the
// daemon starts. It deliberately does not implement swap FSM transitions,
// mailbox receive-event handling, or swap server protocol behavior; those
// responsibilities stay in sdk/swaps and swapdk-server.
package swapclientserver
