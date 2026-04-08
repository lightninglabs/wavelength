package oor

// Package oor implements client-side coordination for out-of-round (OOR) Ark
// transfers.
//
// The main goal is to let a client transfer VTXOs to one or more recipients
// without waiting for a normal round, while still preserving:
// - deterministic transaction construction for safe retries; and
// - crash-safe "resume" semantics for mobile clients.
//
// This package is built around a protofsm-based state machine. All I/O and
// external side effects are modeled as explicit outbox requests that the caller
// executes (via RPC, in-process adaptors, or other mechanisms) and then feeds
// back as events.
//
// For outgoing transfers, the critical point-of-no-return is when the server
// has co-signed the checkpoint transaction(s). After that, the client must be
// able to resume and obtain byte-identical co-signed PSBTs to safely finalize.
