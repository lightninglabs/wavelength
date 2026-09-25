package roundpb

import (
	"fmt"

	"github.com/lightninglabs/wavelength/lib/batchschedule"
)

// BatchSlotFromProto checks the slot's bounds before decoding its identity.
func BatchSlotFromProto(p *BatchSlotSelection) (*batchschedule.Selection, error) {
	if p == nil {
		return nil, nil
	}
	if len(p.ScheduleId) != 32 {
		return nil, fmt.Errorf("schedule identifier must be 32 bytes")
	}
	s := &batchschedule.Selection{CutoffUnix: p.CutoffUnix}
	copy(s.ScheduleID[:], p.ScheduleId)
	if err := s.Validate(); err != nil {
		return nil, err
	}

	return s, nil
}

// BatchSlotToProto preserves absence for legacy joins.
func BatchSlotToProto(s *batchschedule.Selection) *BatchSlotSelection {
	if s == nil {
		return nil
	}

	return &BatchSlotSelection{
		ScheduleId: append([]byte(nil), s.ScheduleID[:]...),
		CutoffUnix: s.CutoffUnix,
	}
}
