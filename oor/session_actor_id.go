package oor

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// SessionActorIDPrefix is the deterministic prefix for a per-session OOR actor
// mailbox id. The registry derives one actor id per session so that a session's
// durable mailbox and control-plane row are addressable without a side lookup.
const SessionActorIDPrefix = "oor-session-"

// ActorIDForSession returns the deterministic durable-mailbox id for the
// per-session OOR actor owning sessionID. It is stable across restarts so the
// registry can re-address a restored session's mailbox, and is the same id
// stored on the session's oor_session_registry row.
func ActorIDForSession(sessionID SessionID) string {
	return SessionActorIDPrefix + chainhash.Hash(sessionID).String()
}
