package oor

import (
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
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

// SessionServiceKey returns the per-session service key under which a live
// session's durable actor is registered. The ingress fast path resolves this
// key to tell session-addressed server pushes straight into the child's
// durable mailbox, skipping the registry hop; a miss (the session has no live
// actor yet, or it was reaped) falls back to the registry, which owns
// admission.
func SessionServiceKey(
	sessionID SessionID) actor.ServiceKey[OORDurableMsg, ActorResp] {

	return actor.NewServiceKey[OORDurableMsg, ActorResp](
		ActorIDForSession(sessionID),
	)
}
