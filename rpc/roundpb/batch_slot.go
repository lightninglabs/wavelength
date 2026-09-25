package roundpb

import "github.com/lightninglabs/wavelength/lib/batchschedule"

// SelectionFromProto converts a join's slot selection from its wire form. A
// nil message means the join requests event-driven timing and yields nil.
func SelectionFromProto(p *BatchSlotSelection) (*batchschedule.Selection,
	error) {

	if p == nil {
		return nil, nil
	}

	id, err := batchschedule.IDFromBytes(p.ScheduleId)
	if err != nil {
		return nil, err
	}

	return batchschedule.SelectionFromUnix(id, p.CutoffUnix)
}

// SelectionToProto converts a slot selection to its wire form. A nil
// selection yields nil so that legacy joins carry no slot at all.
func SelectionToProto(s *batchschedule.Selection) *BatchSlotSelection {
	if s == nil {
		return nil
	}

	return &BatchSlotSelection{
		ScheduleId: append([]byte(nil), s.ScheduleID[:]...),
		CutoffUnix: s.CutoffUnix(),
	}
}
