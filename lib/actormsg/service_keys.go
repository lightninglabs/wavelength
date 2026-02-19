package actormsg

import (
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
)

// VTXOActorServiceKey returns the service key for looking up a VTXO actor by
// its outpoint. Both round and vtxo packages use this function to ensure
// consistent service key types for actor registration and lookup.
//
// The service key uses VTXOActorMsg for the message type and VTXOActorResp for
// the response type. This enables proper type checking by the receptionist
// during actor registration and lookup.
func VTXOActorServiceKey(outpoint wire.OutPoint) actor.ServiceKey[
	VTXOActorMsg, VTXOActorResp,
] {

	return actor.NewServiceKey[VTXOActorMsg, VTXOActorResp](
		fmt.Sprintf("vtxo.%s", outpoint.String()),
	)
}

// RoundActorServiceKeyName is the well-known service key name for the round
// actor. Both the round package (for registration) and wallet package (for
// lookup) use this constant to ensure they reference the same actor.
const RoundActorServiceKeyName = "round-client"

// RoundActorServiceKey returns the service key for looking up the round actor.
// This uses RoundReceivable for the message type so the wallet can look up the
// round actor without importing the round package (avoiding import cycles).
//
// The round package must register the round actor with this key for lookup to
// work. Use RoundActorServiceKeyName to construct the actor ID for Spawn.
func RoundActorServiceKey() actor.ServiceKey[RoundReceivable, RoundActorResp] {
	return actor.NewServiceKey[RoundReceivable, RoundActorResp](
		RoundActorServiceKeyName,
	)
}

// ChainResolverServiceKeyName is the well-known service key name for the
// chain resolver coordinator actor.
const ChainResolverServiceKeyName = "chain-resolver"

// ChainResolverMsg is the message type marker for the chain resolver actor.
// The concrete chainresolver.ChainResolverMsg interface embeds this to
// enable cross-package service key usage without import cycles.
type ChainResolverMsg interface {
	actor.Message
	ChainResolverMsg()
}

// ChainResolverResp is the response type marker for the chain resolver actor.
type ChainResolverResp interface {
	ChainResolverResp()
}

// ChainResolverServiceKey returns the service key for looking up the chain
// resolver coordinator actor.
func ChainResolverServiceKey() actor.ServiceKey[
	ChainResolverMsg, ChainResolverResp,
] {

	return actor.NewServiceKey[ChainResolverMsg, ChainResolverResp](
		ChainResolverServiceKeyName,
	)
}
