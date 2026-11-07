package round

import (
	"errors"
	"time"

	"github.com/lightninglabs/wavelength/lib/batchschedule"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// encodeAdmissionBudget preserves the accepted attempt's fixed expiry.
func encodeAdmissionBudget(budget *admissionBudget) ([]byte, error) {
	if budget == nil {
		return nil, nil
	}
	id := budget.roundID[:]
	due, err := budget.deadline.ExpiresAt.MarshalBinary()
	if err != nil {
		return nil, err
	}
	var closed uint8
	if budget.deadline.Closed {
		closed = 1
	}

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &id),
		tlv.MakePrimitiveRecord(3, &due),
		tlv.MakePrimitiveRecord(5, &closed),
	)
}

// decodeAdmissionBudget restores an absolute expiry with a new monotonic bound.
func decodeAdmissionBudget(raw []byte,
	now time.Time) (*admissionBudget, error) {

	if len(raw) == 0 {
		return nil, nil
	}
	var id, due []byte
	var closed uint8
	if err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &id),
		tlv.MakePrimitiveRecord(3, &due),
		tlv.MakePrimitiveRecord(5, &closed),
	); err != nil {
		return nil, err
	}
	roundID, err := parseDurableRoundID(id)
	if err != nil {
		return nil, err
	}
	var deadline time.Time
	if err := deadline.UnmarshalBinary(due); err != nil {
		return nil, err
	}

	return &admissionBudget{roundID: roundID,
		deadline: AdmissionDeadline{
			ExpiresAt: deadline,
			Closed:    closed != 0,
		},
		cutoff: now.Add(deadline.Sub(now))}, nil
}

// encodeScheduledAttempt freezes the selected window and original join time.
func encodeScheduledAttempt(attempt *scheduledAttempt) ([]byte, error) {
	if attempt == nil {
		return nil, nil
	}
	id := attempt.selection.ScheduleID[:]
	opens, err := attempt.slot.Opens.MarshalBinary()
	if err != nil {
		return nil, err
	}
	cutoff, err := attempt.slot.Cutoff.MarshalBinary()
	if err != nil {
		return nil, err
	}
	wake, err := attempt.wake.MarshalBinary()
	if err != nil {
		return nil, err
	}
	offset, margin := uint64(attempt.clockOffset), uint64(attempt.margin)

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &id),
		tlv.MakePrimitiveRecord(3, &opens),
		tlv.MakePrimitiveRecord(5, &cutoff),
		tlv.MakePrimitiveRecord(7, &wake),
		tlv.MakePrimitiveRecord(9, &offset),
		tlv.MakePrimitiveRecord(11, &margin),
	)
}

// decodeScheduledAttempt never selects a new slot for an interrupted attempt.
func decodeScheduledAttempt(raw []byte) (*scheduledAttempt, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var id, opens, cutoff, wake []byte
	var offset, margin uint64
	if err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &id),
		tlv.MakePrimitiveRecord(3, &opens),
		tlv.MakePrimitiveRecord(5, &cutoff),
		tlv.MakePrimitiveRecord(7, &wake),
		tlv.MakePrimitiveRecord(9, &offset),
		tlv.MakePrimitiveRecord(11, &margin),
	); err != nil {
		return nil, err
	}
	identity, err := batchschedule.IDFromBytes(id)
	if err != nil {
		return nil, err
	}
	attempt := &scheduledAttempt{
		clockOffset: time.Duration(offset),
		margin:      time.Duration(margin),
	}
	if err := attempt.slot.Opens.UnmarshalBinary(opens); err != nil {
		return nil, err
	}
	if err := attempt.slot.Cutoff.UnmarshalBinary(cutoff); err != nil {
		return nil, err
	}
	if err := attempt.wake.UnmarshalBinary(wake); err != nil {
		return nil, err
	}
	attempt.selection = batchschedule.Selection{
		ScheduleID: identity,
		Cutoff:     attempt.slot.Cutoff,
	}
	if err := attempt.selection.Validate(); err != nil {
		return nil, err
	}

	return attempt, nil
}

// encodePublishedSchedule retains discovery without synthesizing future slots.
func encodePublishedSchedule(value fn.Option[batchschedule.Published]) ([]byte,
	error) {

	if value.IsNone() {
		return nil, nil
	}
	published := value.UnwrapOr(batchschedule.Published{})
	id := published.ID()
	var slots []batchschedule.Slot
	var after time.Time
	for {
		slot, err := published.Next(after)
		if errors.Is(err, batchschedule.ErrScheduleExhausted) {
			break
		}
		if err != nil {
			return nil, err
		}
		slots = append(slots, slot)
		after = slot.Cutoff
	}
	encoded, err := encodeDurableList(
		slots,
		func(slot batchschedule.Slot) ([]byte, error) {
			opens, err := slot.Opens.MarshalBinary()
			if err != nil {
				return nil, err
			}
			cutoff, err := slot.Cutoff.MarshalBinary()
			if err != nil {
				return nil, err
			}

			return encodeDurableFields(
				tlv.MakePrimitiveRecord(1, &opens),
				tlv.MakePrimitiveRecord(3, &cutoff),
			)
		},
	)
	if err != nil {
		return nil, err
	}
	timestamp, err := published.ServerTime().MarshalBinary()
	if err != nil {
		return nil, err
	}
	identity, offset := id[:], uint64(published.ClockOffset())

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &identity),
		tlv.MakePrimitiveRecord(3, &encoded),
		tlv.MakePrimitiveRecord(5, &timestamp),
		tlv.MakePrimitiveRecord(7, &offset),
	)
}

// decodePublishedSchedule validates the saved slots and operator clock
// estimate.
func decodePublishedSchedule(raw []byte) (fn.Option[batchschedule.Published],
	error) {

	none := fn.None[batchschedule.Published]()
	if len(raw) == 0 {
		return none, nil
	}
	var id, encoded, timestamp []byte
	var offset uint64
	if err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &id),
		tlv.MakePrimitiveRecord(3, &encoded),
		tlv.MakePrimitiveRecord(5, &timestamp),
		tlv.MakePrimitiveRecord(7, &offset),
	); err != nil {
		return none, err
	}
	identity, err := batchschedule.IDFromBytes(id)
	if err != nil {
		return none, err
	}
	slots, err := decodeDurableList(
		encoded,
		func(value []byte) (batchschedule.Slot, error) {
			var opens, cutoff []byte
			var slot batchschedule.Slot
			if err := decodeDurableFields(
				value, tlv.MakePrimitiveRecord(1, &opens),
				tlv.MakePrimitiveRecord(3, &cutoff),
			); err != nil {
				return slot, err
			}
			if err := slot.Opens.UnmarshalBinary(
				opens,
			); err != nil {
				return slot, err
			}
			if err := slot.Cutoff.UnmarshalBinary(
				cutoff,
			); err != nil {
				return slot, err
			}

			return slot, nil
		},
	)
	if err != nil {
		return none, err
	}
	published, err := batchschedule.NewPublished(identity, slots)
	if err != nil {
		return none, err
	}
	var serverTime time.Time
	if err := serverTime.UnmarshalBinary(timestamp); err != nil {
		return none, err
	}

	return fn.Some(
		published.WithServerTime(
			serverTime).WithClockOffset(time.Duration(offset)),
	), nil
}
