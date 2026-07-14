package txconfirm

import "github.com/lightninglabs/wavelength/baselib/actor"

// ServiceKeyName is the actor system service key name for the shared
// transaction-confirmation broadcaster actor.
const ServiceKeyName = "txconfirm"

// NewServiceKey returns the service key used to register the
// TxBroadcasterActor with the actor system.
//
// Producer subsystems (the unroll registry, the boarding-sweep flow inside
// the wallet actor, etc.) construct the same service key via this helper
// to look up the broadcaster ref at runtime, so registrar and consumers
// always agree on the canonical name.
func NewServiceKey() actor.ServiceKey[Msg, Resp] {
	return actor.NewServiceKey[Msg, Resp](ServiceKeyName)
}

// LookupRef resolves the txconfirm service key against the supplied actor
// system context and returns an actor ref usable for Ask/Tell. Producers
// that hold only the actor system at construction time (e.g. wallet.NewArk)
// use this helper to defer broadcaster resolution until the broadcaster
// actor has been registered. The returned ref is a router built from the
// receptionist; calls before registration return ErrNoSuchActor on first
// use rather than panicking.
func LookupRef(system actor.SystemContext) actor.ActorRef[Msg, Resp] {
	return NewServiceKey().Ref(system)
}
