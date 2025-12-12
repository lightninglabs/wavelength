package rounds

import (
	"github.com/lightninglabs/darepo-client/baselib/actor"
)

// ActorMsg is the sealed interface for all messages that can be sent to the
// server round Actor.
type ActorMsg interface {
	actor.Message

	// actorMsgSealed marks this interface as sealed, preventing external
	// implementations.
	actorMsgSealed()
}

// ActorResp is the sealed interface for all response messages from a server
// rounds Actor.
type ActorResp interface {
	actor.Message

	// actorRespSealed marks this interface as sealed, preventing external
	// implementations.
	actorRespSealed()
}
