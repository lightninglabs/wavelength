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
