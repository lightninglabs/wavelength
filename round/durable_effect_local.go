package round

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/tlv"
)

// newDurableLocalEffect freezes the inputs to local actor notifications.
func newDurableLocalEffect(msg ClientOutMsg,
	now time.Time) (*durableClientEffect, error) {

	effect := &durableClientEffect{}
	var err error
	switch m := msg.(type) {
	case *releaseBoardingEffect:
		effect.Kind = clientEffectBoardingRelease
		effect.Payload, err = encodeDurableList(
			m.outpoints, encodeDurablePoint,
		)

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
		points, pointErr := encodeDurableList(
			m.Outpoints, encodeDurablePoint,
		)
		if pointErr != nil {
			return nil, pointErr
		}
		id := []byte(m.RoundID)
		effect.Payload, err = encodeDurableFields(
			tlv.MakePrimitiveRecord(1, &points),
			tlv.MakePrimitiveRecord(3, &id),
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
	case clientEffectBoardingRelease:
		points, err := decodeDurableList(
			m.Payload, parseDurableOutpoint,
		)

		return &releaseBoardingEffect{outpoints: points}, err

	case clientEffectConfirmation:
		return decodeDurableConfirmation(m.Payload)

	case clientEffectStartTimer, clientEffectCancelTimer:
		return m.timerMessage(now)

	case clientEffectRelease:
		var raw []byte
		var id []byte
		if err := decodeDurableFields(
			m.Payload, tlv.MakePrimitiveRecord(1, &raw),
			tlv.MakePrimitiveRecord(3, &id),
		); err != nil {
			return nil, err
		}
		points, err := decodeDurableList(raw, parseDurableOutpoint)
		if err != nil {
			return nil, err
		}

		return &ReleaseForfeitReservation{
			Outpoints: points,
			RoundID:   string(id),
		}, nil

	case clientEffectDropCustom:
		points, err := decodeDurableList(
			m.Payload, parseDurableOutpoint,
		)
		if err != nil {
			return nil, err
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
