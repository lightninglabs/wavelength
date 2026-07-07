package actormsg

import (
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
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
		fmt.Sprintf(
			"vtxo.%s", outpoint.String(),
		),
	)
}

// VTXOManagerServiceKeyName is the well-known service key name for the VTXO
// manager actor. Both the vtxo package (for registration) and wallet package
// (for lookup) use this constant to ensure they reference the same actor.
const VTXOManagerServiceKeyName = "vtxo-manager"

// VTXOManagerServiceKey returns the service key for looking up the VTXO
// manager actor. This uses VTXOManagerMsg for the message type so the
// wallet can send admission requests without importing the vtxo package.
func VTXOManagerServiceKey() actor.ServiceKey[
	VTXOManagerMsg, VTXOManagerResp,
] {

	return actor.NewServiceKey[VTXOManagerMsg, VTXOManagerResp](
		VTXOManagerServiceKeyName,
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
