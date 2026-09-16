package round

import (
	"bytes"
	"fmt"

	"github.com/lightninglabs/wavelength/timeout"
)

// captureDurableEffect updates timer ownership on the same turn as its effect.
func (a *RoundClientActor) captureDurableEffect(msg ClientOutMsg) (
	*durableClientEffect, error) {

	effect, err := newDurableClientEffect(msg, a.env.now())
	if err != nil {
		return nil, err
	}
	switch effect.Kind {
	case clientEffectStartTimer, clientEffectCancelTimer:
		key, phase, _, err := decodeDurableTimer(effect.Payload)
		if err != nil {
			return nil, err
		}
		id := makeTimeoutID(key, phase)
		if effect.Kind == clientEffectCancelTimer {
			delete(a.pendingTimers, id)

			return effect, nil
		}
		if a.pendingTimers == nil {
			a.pendingTimers = make(
				map[timeout.ID]*durableClientEffect,
			)
		}
		a.pendingTimers[id] = effect
	}

	return effect, nil
}

// encodeDurableTimers stores original start effects so restart never renews
// them.
func encodeDurableTimers(timers map[timeout.ID]*durableClientEffect) ([]byte,
	error) {

	return encodeDurableMap(timers,
		func(id timeout.ID) ([]byte, error) { return []byte(id), nil },
		func(effect *durableClientEffect) ([]byte, error) {
			if effect == nil {
				return nil, fmt.Errorf("missing timer effect")
			}
			var raw bytes.Buffer
			if err := effect.Encode(&raw); err != nil {
				return nil, err
			}

			return raw.Bytes(), nil
		},
	)
}

// decodeDurableTimers validates routing before any timeout can be rearmed.
func decodeDurableTimers(raw []byte) (map[timeout.ID]*durableClientEffect,
	error) {

	timers, err := decodeDurableMap(raw,
		func(key []byte) (timeout.ID, error) {
			return timeout.ID(key), nil
		},
		func(value []byte) (*durableClientEffect, error) {
			effect := &durableClientEffect{}
			if err := effect.Decode(
				bytes.NewReader(value),
			); err != nil {
				return nil, err
			}
			if effect.Kind != clientEffectStartTimer {
				return nil, fmt.Errorf("not a start timer " +
					"effect")
			}

			return effect, nil
		},
	)
	if err != nil {
		return nil, err
	}
	for id, effect := range timers {
		key, phase, _, err := decodeDurableTimer(effect.Payload)
		if err != nil {
			return nil, err
		}
		if id != makeTimeoutID(key, phase) {
			return nil, fmt.Errorf("timer routing key differs")
		}
	}

	return timers, nil
}
