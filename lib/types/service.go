package types

import (
	"bytes"
	"fmt"
	"math"
	"time"

	"github.com/lightningnetwork/lnd/tlv"
)

// ServiceMode selects the explicitly requested round execution service.
// Legacy participation is represented by a nil ServiceRequest.
type ServiceMode uint8

const (
	// ServiceScheduled joins one published registration window.
	ServiceScheduled ServiceMode = 1

	// ServiceImmediate requests an independently authorized singleton
	// round.
	ServiceImmediate ServiceMode = 2
)

// ServiceRequest binds execution and fee authorization to a logical operation.
// A retry retains OperationID; the server assigns a fresh signing attempt.
// These fields are included in the canonical join-auth message.
type ServiceRequest struct {
	// OperationID is a client-generated, nonzero random operation identity.
	OperationID [32]byte

	// Mode selects scheduled or immediate execution.
	Mode ServiceMode

	// ScheduleVersion identifies the immutable timetable for scheduled
	// work. Immediate requests leave both schedule fields zero.
	ScheduleVersion uint64

	// SlotIndex identifies the requested slot within ScheduleVersion.
	SlotIndex uint64

	// ExpiresAtUnix is the absolute operation expiry in whole UTC seconds.
	ExpiresAtUnix uint64

	// FeeLimitSat is the maximum operator quote the caller will accept.
	// Zero authorizes only a zero-fee quote.
	FeeLimitSat uint64

	// AllowFallback authorizes a later immediate attempt after a scheduled
	// attempt has safely relinquished ownership. It never grants access to
	// the immediate service or authorizes two concurrent attempts.
	AllowFallback bool
}

// Validate checks the canonical shape without consulting time or policy.
// The operator separately checks expiry, capacity, and principal authorization.
func (s ServiceRequest) Validate() error {
	if s.OperationID == ([32]byte{}) {
		return fmt.Errorf("operation ID must be nonzero")
	}
	if s.ExpiresAtUnix == 0 || s.ExpiresAtUnix > 253402300799 {
		return fmt.Errorf("operation expiry outside 1970..9999")
	}
	if s.FeeLimitSat > math.MaxInt64 {
		return fmt.Errorf("operation fee limit exceeds amount range")
	}
	switch s.Mode {
	case ServiceScheduled:
		if s.ScheduleVersion == 0 {
			return fmt.Errorf("scheduled service requires a " +
				"version")
		}

	case ServiceImmediate:
		if s.ScheduleVersion != 0 || s.SlotIndex != 0 {
			return fmt.Errorf("immediate service cannot name a " +
				"slot")
		}

	default:
		return fmt.Errorf("unknown service mode %d", s.Mode)
	}

	return nil
}

// Expiry returns the absolute operation expiry. Call Validate before use.
func (s ServiceRequest) Expiry() time.Time {
	return time.Unix(int64(s.ExpiresAtUnix), 0).UTC()
}

// Encode serializes service authorization as an ordered TLV stream.
func (s ServiceRequest) Encode() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	mode := uint8(s.Mode)
	var fallback uint8
	if s.AllowFallback {
		fallback = 1
	}
	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(1, &s.OperationID),
		tlv.MakePrimitiveRecord(2, &mode),
		tlv.MakePrimitiveRecord(3, &s.ScheduleVersion),
		tlv.MakePrimitiveRecord(4, &s.SlotIndex),
		tlv.MakePrimitiveRecord(5, &s.ExpiresAtUnix),
		tlv.MakePrimitiveRecord(6, &s.FeeLimitSat),
		tlv.MakePrimitiveRecord(7, &fallback),
	)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// DecodeServiceRequest requires every authorization field and a canonical
// encoding, so persistence and signatures cannot interpret a request
// differently.
func DecodeServiceRequest(raw []byte) (*ServiceRequest, error) {
	var s ServiceRequest
	var mode, fallback uint8
	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(1, &s.OperationID),
		tlv.MakePrimitiveRecord(2, &mode),
		tlv.MakePrimitiveRecord(3, &s.ScheduleVersion),
		tlv.MakePrimitiveRecord(4, &s.SlotIndex),
		tlv.MakePrimitiveRecord(5, &s.ExpiresAtUnix),
		tlv.MakePrimitiveRecord(6, &s.FeeLimitSat),
		tlv.MakePrimitiveRecord(7, &fallback),
	)
	if err != nil {
		return nil, err
	}
	parsed, err := stream.DecodeWithParsedTypes(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for record := tlv.Type(1); record <= 7; record++ {
		if _, ok := parsed[record]; !ok {
			return nil, fmt.Errorf("missing service field %d",
				record)
		}
	}
	if fallback > 1 {
		return nil, fmt.Errorf("invalid fallback authorization")
	}
	s.Mode = ServiceMode(mode)
	s.AllowFallback = fallback == 1
	canonical, err := s.Encode()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, canonical) {
		return nil, fmt.Errorf("noncanonical service authorization")
	}

	return &s, nil
}
