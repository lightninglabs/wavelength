package oor

import "github.com/lightninglabs/darepo-client/baselib/actor"

const (
	// DefaultActorServiceKeyName is the default OOR actor service-key name
	// used for actor-system lookup.
	DefaultActorServiceKeyName = "oor-service"
)

// ActorServiceKey returns the actor-system key for the outgoing OOR actor.
//
// When actorID is empty, the default service-key name is used.
func ActorServiceKey(actorID string) actor.ServiceKey[ActorMsg, ActorResp] {
	if actorID == "" {
		actorID = DefaultActorServiceKeyName
	}

	return actor.NewServiceKey[ActorMsg, ActorResp](actorID)
}
