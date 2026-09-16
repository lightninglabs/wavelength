package roundpb

import (
	"bytes"
	"fmt"

	"github.com/lightninglabs/wavelength/lib/types"
)

// ServiceRequestToProto preserves explicit authorization, including zero fees.
func ServiceRequestToProto(s *types.ServiceRequest) (*ServiceRequest, error) {
	if s == nil {
		return nil, nil
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}

	return &ServiceRequest{
		OperationId:     bytes.Clone(s.OperationID[:]),
		Mode:            ServiceMode(s.Mode),
		ScheduleVersion: s.ScheduleVersion,
		SlotIndex:       s.SlotIndex,
		ExpiresAtUnix:   s.ExpiresAtUnix,
		FeeLimitSat:     s.FeeLimitSat,
		AllowFallback:   s.AllowFallback,
	}, nil
}

// ServiceRequestFromProto validates before narrowing enums or copying IDs.
func ServiceRequestFromProto(s *ServiceRequest) (*types.ServiceRequest, error) {
	if s == nil {
		return nil, nil
	}
	if len(s.OperationId) != 32 {
		return nil, fmt.Errorf("operation ID must be 32 bytes")
	}
	if s.Mode != ServiceMode_SERVICE_SCHEDULED &&
		s.Mode != ServiceMode_SERVICE_IMMEDIATE {
		return nil, fmt.Errorf("unknown service mode %d", s.Mode)
	}
	result := &types.ServiceRequest{
		OperationID:     [32]byte(s.OperationId),
		Mode:            types.ServiceMode(s.Mode),
		ScheduleVersion: s.ScheduleVersion,
		SlotIndex:       s.SlotIndex,
		ExpiresAtUnix:   s.ExpiresAtUnix,
		FeeLimitSat:     s.FeeLimitSat,
		AllowFallback:   s.AllowFallback,
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}

	return result, nil
}
