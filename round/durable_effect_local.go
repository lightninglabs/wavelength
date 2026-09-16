package round

import (
	"fmt"
	"time"
)

// newDurableLocalEffect freezes the inputs to local actor notifications.
func newDurableLocalEffect(msg ClientOutMsg,
	now time.Time) (*durableClientEffect, error) {

	effect := &durableClientEffect{}
	var err error
	switch m := msg.(type) {
	case *RegisterConfirmationRequest:
		effect.Kind = clientEffectConfirmation
		effect.Payload, err = encodeDurableConfirmation(m)

	case *StartTimeoutReq:
		effect.Kind = clientEffectStartTimer
		deadline := m.deadline
		if deadline.IsZero() {
			deadline = now.Add(m.Duration)
		}
		effect.Payload, err = encodeDurableTimer(
			m.RoundKey, m.Phase, deadline,
		)

	case *CancelTimeoutReq:
		effect.Kind = clientEffectCancelTimer
		effect.Payload, err = encodeDurableTimer(
			m.RoundKey, m.Phase, time.Time{},
		)

	case *ReleaseForfeitReservation:
		effect.Kind = clientEffectRelease
		effect.Payload, err = encodeDurableList(
			m.Outpoints, encodeDurablePoint,
		)

	case *DropCustomForfeitReservation:
		effect.Kind = clientEffectDropCustom
		effect.Payload, err = encodeDurableList(
			m.Outpoints, encodeDurablePoint,
		)

	case *VTXOCreatedNotification:
		effect.Kind = clientEffectCreated
		effect.Payload, err = encodeDurableCreated(m)

	case *ForfeitRequestToVTXO:
		effect.Kind = clientEffectForfeitRequest
		effect.Payload, err = encodeDurableForfeitRequest(m)

	case *ForfeitConfirmedToVTXO:
		effect.Kind = clientEffectForfeitConfirmed
		effect.Payload, err = encodeDurableForfeitConfirmed(m)

	default:
		return nil, fmt.Errorf("unsupported client effect %T", msg)
	}
	if err != nil {
		return nil, err
	}

	return effect, nil
}

// localMessage reconstructs an effect using current delivery-time dependencies.
func (m *durableClientEffect) localMessage(now time.Time) (ClientOutMsg,
	error) {

	if err := m.validate(); err != nil {
		return nil, err
	}
	switch m.Kind {
	case clientEffectConfirmation:
		return decodeDurableConfirmation(m.Payload)

	case clientEffectStartTimer, clientEffectCancelTimer:
		return m.timerMessage(now)

	case clientEffectRelease, clientEffectDropCustom:
		points, err := decodeDurableList(
			m.Payload, parseDurableOutpoint,
		)
		if err != nil {
			return nil, err
		}
		if m.Kind == clientEffectRelease {
			return &ReleaseForfeitReservation{
				Outpoints: points,
			}, nil
		}

		return &DropCustomForfeitReservation{Outpoints: points}, nil

	case clientEffectCreated:
		return decodeDurableCreated(m.Payload)

	case clientEffectForfeitRequest:
		return decodeDurableForfeitRequest(m.Payload)

	case clientEffectForfeitConfirmed:
		return decodeDurableForfeitConfirmed(m.Payload)

	default:
		return nil, fmt.Errorf("not a local client effect")
	}
}
