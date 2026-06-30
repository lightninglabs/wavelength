package credit

// CreditEvent is the sealed inbound event interface that drives a credit
// operation FSM. The engine is poll-driven: a single drive event advances the
// current state by one step. The durable per-operation actor delivers it on
// resume and on each poll-timer expiry, and re-drives after every non-terminal,
// non-parked step until the operation parks on a poll or reaches a terminal
// state.
type CreditEvent interface {
	creditEventSealed()
}

// opDrive advances the current state by one step.
type opDrive struct{}

func (*opDrive) creditEventSealed() {}

// CreditOutMsg is the sealed outbox interface. A transition emits outbox
// directives that the durable per-operation actor executes through its Exec
// handle after the transition returns. This is how the FSM dictates persistence
// and cross-actor effects while the durable actor owns the lease-fenced
// Stage/Commit writes and the exactly-once ack: the transition decides WHAT to
// persist or signal, the actor decides HOW, preserving the
// persist-before-effect ordering (a stageRecord is flushed before the next
// state runs its effect).
type CreditOutMsg interface {
	creditOutMsgSealed()
}

// stageRecord directs the actor to durably checkpoint the current record via a
// Stage write before the next state runs its side effect. It is emitted by a
// state that recorded a server identifier (a top-up destination, a redeem
// destination) the next effect depends on, so a crash before the turn commits
// re-drives from the checkpointed state rather than re-deriving a fresh
// identifier the in-flight effect is no longer bound to.
type stageRecord struct{}

func (*stageRecord) creditOutMsgSealed() {}

// parkOp directs the actor to stop driving this turn and arm the reconciliation
// poll timer, so an awaiting state is re-driven after the configured backoff
// without a hot loop. It is emitted on every drive of an awaiting state that
// has not yet observed its terminal server or chain signal.
type parkOp struct{}

func (*parkOp) creditOutMsgSealed() {}

// triggerRedeem asks the registry to consider materializing available credits
// into a vTXO now that a receive has settled and pushed the balance over the
// auto-redeem watermark. The registry arbitrates the request against the
// no-pending-pay/redeem interlock and admits a redeem operation when warranted,
// so the receive FSM signals intent without owning the redeem decision. It
// folds the wallet-owned auto-redeem into the receive state machine, replacing
// the periodic background sweep with a causal, event-driven trigger.
type triggerRedeem struct {
	// AvailableSat is the earmark-adjusted available credit balance the
	// settled receive observed, the upper bound on what may be redeemed.
	AvailableSat uint64
}

func (*triggerRedeem) creditOutMsgSealed() {}
