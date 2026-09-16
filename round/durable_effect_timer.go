package round

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/tlv"
)

// encodeDurableTimer preserves the absolute due time across delayed delivery.
func encodeDurableTimer(key RoundKeyStr, phase TimeoutPhase,
	deadline time.Time) ([]byte, error) {

	keyBytes, phaseBytes := []byte(key), []byte(phase)
	due, err := deadline.UTC().MarshalBinary()
	if err != nil {
		return nil, err
	}

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &keyBytes),
		tlv.MakePrimitiveRecord(3, &phaseBytes),
		tlv.MakePrimitiveRecord(5, &due),
	)
}

// decodeDurableTimer returns the recorded wall-clock deadline without renewal.
func decodeDurableTimer(raw []byte) (RoundKeyStr, TimeoutPhase, time.Time,
	error) {

	var key, phase, due []byte
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &key),
		tlv.MakePrimitiveRecord(3, &phase),
		tlv.MakePrimitiveRecord(5, &due),
	)
	if err != nil {
		return "", "", time.Time{}, err
	}
	var deadline time.Time
	if err := deadline.UnmarshalBinary(due); err != nil {
		return "", "", time.Time{}, err
	}

	return RoundKeyStr(key), TimeoutPhase(phase), deadline, nil
}

// timerMessage reconstructs a timer using only the remaining duration.
func (m *durableClientEffect) timerMessage(now time.Time) (ClientOutMsg,
	error) {

	key, phase, deadline, err := decodeDurableTimer(m.Payload)
	if err != nil {
		return nil, err
	}
	switch m.Kind {
	case clientEffectStartTimer:
		return &StartTimeoutReq{
			deadline: deadline,
			RoundKey: key, Phase: phase, Duration: max(
				0, deadline.Sub(now),
			),
		}, nil

	case clientEffectCancelTimer:
		return &CancelTimeoutReq{RoundKey: key, Phase: phase}, nil

	default:
		return nil, fmt.Errorf("not a timer effect")
	}
}
