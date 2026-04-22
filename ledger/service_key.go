package ledger

import "github.com/lightninglabs/darepo-client/baselib/actor"

// ServiceKeyName is the actor system service key name for the
// ledger accounting actor.
const ServiceKeyName = "ledger-accounting"

// NewServiceKey returns the service key used to register the
// ledger actor with the actor system.
func NewServiceKey() actor.ServiceKey[LedgerMsg, LedgerResp] {
	return actor.NewServiceKey[LedgerMsg, LedgerResp](
		ServiceKeyName,
	)
}
