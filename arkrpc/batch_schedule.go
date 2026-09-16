package arkrpc

import (
	"bytes"
	"fmt"
	"math"
	"time"

	"github.com/lightninglabs/wavelength/lib/batchschedule"
)

// ParseBatchSchedule validates advertised timing before a client uses it.
// Absence preserves legacy event-driven registration.
func ParseBatchSchedule(p *BatchSchedule) (*batchschedule.Schedule, error) {
	if p == nil {
		return nil, nil
	}
	if p.Version != 1 {
		return nil, fmt.Errorf("unsupported schedule version %d",
			p.Version)
	}
	if p.IntervalSeconds <= 0 ||
		p.IntervalSeconds > math.MaxInt64/int64(time.Second) ||
		p.RegistrationWindowSeconds <= 0 ||
		p.RegistrationWindowSeconds > math.MaxInt64/int64(time.Second) {
		return nil, fmt.Errorf("invalid schedule durations")
	}
	s, err := batchschedule.New(
		time.Unix(p.AnchorUnix, 0),
		time.Duration(p.IntervalSeconds)*time.Second,
		time.Duration(p.RegistrationWindowSeconds)*time.Second,
	)
	if err != nil {
		return nil, err
	}
	id := s.ID()
	if !bytes.Equal(id[:], p.ScheduleId) {
		return nil, fmt.Errorf("schedule identifier does not match " +
			"timing")
	}

	if _, err := s.At(time.Unix(p.NextCutoffUnix, 0)); err != nil {
		return nil, fmt.Errorf("invalid advertised cutoff: %w", err)
	}

	return s, nil
}

// BatchScheduleToProto publishes an already validated schedule and the next
// admission opportunity chosen by the operator's persisted slot cursor.
func BatchScheduleToProto(s *batchschedule.Schedule,
	now, cutoff time.Time) *BatchSchedule {

	if s == nil {
		return nil
	}
	id := s.ID()

	return &BatchSchedule{
		Version: 1, AnchorUnix: s.Anchor().Unix(),
		IntervalSeconds:           int64(s.Interval() / time.Second),
		RegistrationWindowSeconds: int64(s.Window() / time.Second),
		ScheduleId:                id[:], ServerTimeUnix: now.Unix(),
		NextCutoffUnix: cutoff.Unix(),
	}
}
