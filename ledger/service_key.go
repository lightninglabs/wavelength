package ledger

import "github.com/lightninglabs/wavelength/baselib/actor"

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

// Sink is the type producer subsystems hold to forward accounting
// events to the ledger actor. It is a fire-and-forget reference
// because LedgerResp is always nil; callers Tell, never Ask.
type Sink = actor.TellOnlyRef[LedgerMsg]

// NewSink resolves the ledger service key against the supplied
// actor system and returns a Tell-only reference suitable for
// embedding in subsystem actor configs. Callers typically wrap
// the result in fn.Option so test harnesses can pass None when
// no actor system is available.
//
// The returned reference is a router built from
// ServiceKey.Ref(system); Tell on a system with no registered
// ledger actor returns an error which callers should log but not
// propagate -- accounting is a side observation, never a
// blocking pre-condition for the operations being recorded.
func NewSink(system *actor.ActorSystem) Sink {
	return NewServiceKey().Ref(system)
}
