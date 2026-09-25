package roundpb

import (
	"github.com/lightninglabs/wavelength/lib/batchschedule"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// SelectionFromProto converts a join's slot selection from its wire form. A
// nil message means the join requests event-driven timing and yields None.
func SelectionFromProto(p *BatchSlotSelection) (
	fn.Option[batchschedule.Selection], error) {

	none := fn.None[batchschedule.Selection]()
	if p == nil {
		return none, nil
	}

	id, err := batchschedule.IDFromBytes(p.ScheduleId)
	if err != nil {
		return none, err
	}

	selection, err := batchschedule.SelectionFromUnix(id, p.CutoffUnix)
	if err != nil {
		return none, err
	}

	return fn.Some(selection), nil
}

// SelectionToProto converts a slot selection to its wire form. None yields a
// nil message so that legacy joins carry no slot at all.
func SelectionToProto(
	selection fn.Option[batchschedule.Selection]) *BatchSlotSelection {

	return fn.MapOptionZ(
		selection,
		func(s batchschedule.Selection) *BatchSlotSelection {
			return &BatchSlotSelection{
				ScheduleId: append(
					[]byte(nil), s.ScheduleID[:]...,
				),
				CutoffUnix: s.CutoffUnix(),
			}
		},
	)
}
