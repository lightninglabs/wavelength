package round

import (
	"bytes"
	"context"
)

// deliverDurableEffect delivers frozen input without recapturing it in an
// outbox.
func (a *RoundClientActor) deliverDurableEffect(ctx context.Context,
	effect *durableClientEffect) error {

	if err := effect.validate(); err != nil {
		return err
	}
	if effect.Kind == clientEffectServer {
		request, err := effect.serverRequest()
		if err != nil {
			return err
		}

		return a.cfg.ServerConn.Tell(ctx, request)
	}
	if effect.Kind == clientEffectStartTimer ||
		effect.Kind == clientEffectCancelTimer {

		current, err := a.currentTimerEffect(effect)
		if err != nil {
			return err
		}
		if !current {
			return nil
		}
	}
	message, err := effect.localMessage(a.env.now())
	if err != nil {
		return err
	}

	return a.deliverOutbox(ctx, []ClientOutMsg{message})
}

// currentTimerEffect ignores effects superseded by a later committed schedule.
func (a *RoundClientActor) currentTimerEffect(effect *durableClientEffect) (
	bool, error) {

	key, phase, _, err := decodeDurableTimer(effect.Payload)
	if err != nil {
		return false, err
	}
	pending := a.pendingTimers[makeTimeoutID(key, phase)]
	if effect.Kind == clientEffectCancelTimer {
		return pending == nil, nil
	}

	return pending != nil &&
		bytes.Equal(pending.Payload, effect.Payload), nil
}

// currentTimerCallback prevents an older callback from expiring a new schedule.
func (a *RoundClientActor) currentTimerCallback(msg *TimeoutMsg) (bool, error) {
	if msg.deadline.IsZero() {
		return true, nil
	}
	pending := a.pendingTimers[msg.TimeoutID]
	if pending == nil {
		return false, nil
	}
	_, _, deadline, err := decodeDurableTimer(pending.Payload)
	if err != nil {
		return false, err
	}

	return deadline.Equal(msg.deadline), nil
}

// RoundReceivable permits delivery through the round actor's ordinary
// interface.
func (*durableClientEffect) RoundReceivable() {}
