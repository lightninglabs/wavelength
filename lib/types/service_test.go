package types

import (
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestServiceAuthorization binds every execution constraint into join auth.
func TestServiceAuthorization(t *testing.T) {
	service := ServiceRequest{
		OperationID: [32]byte{
			1,
		}, Mode: ServiceScheduled,
		ScheduleVersion: 2, SlotIndex: 3, ExpiresAtUnix: 1_900_000_000,
		FeeLimitSat: 1000, AllowFallback: true,
	}
	req := &JoinRoundRequest{Identifier: testJoinAuthPubKey(t)}
	legacy, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	req.Service = &service
	signed, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	require.NotEqual(t, legacy, signed)
	decoded, err := DecodeJoinRoundAuthMessage(signed)
	require.NoError(t, err)
	require.Equal(t, service, *decoded.Service)
	for _, mutate := range []func(*ServiceRequest){
		func(s *ServiceRequest) {
			s.OperationID[0]++
		},
		func(s *ServiceRequest) {
			s.ScheduleVersion++
		},
		func(s *ServiceRequest) {
			s.SlotIndex++
		},
		func(s *ServiceRequest) {
			s.ExpiresAtUnix++
		},
		func(s *ServiceRequest) {
			s.FeeLimitSat++
		},
		func(s *ServiceRequest) {
			s.AllowFallback = false
		},
		func(s *ServiceRequest) {
			s.Mode = ServiceImmediate
			s.ScheduleVersion = 0
			s.SlotIndex = 0
		},
	} {
		candidate := service
		mutate(&candidate)
		req.Service = &candidate
		changed, err := JoinRoundAuthMessage(req)
		require.NoError(t, err)
		require.NotEqual(t, signed, changed)
	}
	req.Service = nil
	replayed, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	require.Equal(t, legacy, replayed)
	decoded, err = DecodeJoinRoundAuthMessage(legacy)
	require.NoError(t, err)
	require.Nil(t, decoded.Service)
}

// TestServiceEncodingProperty checks deterministic canonical round trips.
func TestServiceEncodingProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		service := ServiceRequest{
			OperationID: [32]byte{
				1,
			}, Mode: ServiceScheduled,
			ScheduleVersion: rapid.Uint64Min(1).Draw(t, "version"),
			SlotIndex:       rapid.Uint64().Draw(t, "slot"),
			ExpiresAtUnix: rapid.Uint64Range(1, 253402300799).Draw(
				t, "expiry",
			),
			FeeLimitSat: rapid.Uint64Range(0, 100_000_000).Draw(
				t, "fee",
			),
			AllowFallback: rapid.Bool().Draw(t, "fallback"),
		}
		if rapid.Bool().Draw(t, "immediate") {
			service.Mode = ServiceImmediate
			service.ScheduleVersion = 0
			service.SlotIndex = 0
		}
		encoded, err := service.Encode()
		require.NoError(t, err)
		decoded, err := DecodeServiceRequest(encoded)
		require.NoError(t, err)
		require.Equal(t, service, *decoded)
		// Removing a required field must not grant default authority.
		_, err = DecodeServiceRequest(encoded[:len(encoded)-3])
		require.Error(t, err)
	})
}

// TestOperationMessageExcludesAttemptIdentity keeps fresh authentication keys
// out of stable ownership while still binding the requested spend and outputs.
func TestOperationMessageExcludesAttemptIdentity(t *testing.T) {
	request := &JoinRoundRequest{Identifier: testJoinAuthPubKey(t)}
	firstAuth, err := JoinRoundAuthMessage(request)
	require.NoError(t, err)
	stable, err := JoinRoundOperationMessage(request)
	require.NoError(t, err)
	request.Identifier = testJoinAuthPubKey(t)
	request.Service = &ServiceRequest{
		OperationID: [32]byte{
			1,
		}, Mode: ServiceImmediate, ExpiresAtUnix: 2_000_000_000,
	}
	secondAuth, err := JoinRoundAuthMessage(request)
	require.NoError(t, err)
	require.NotEqual(t, firstAuth, secondAuth)
	retried, err := JoinRoundOperationMessage(request)
	require.NoError(t, err)
	require.Equal(t, stable, retried)
	request.ForfeitReqs = []*ForfeitRequest{
		{
			VTXOOutpoint: &wire.OutPoint{
				Index: 1,
			},
		},
	}
	changed, err := JoinRoundOperationMessage(request)
	require.NoError(t, err)
	require.NotEqual(t, stable, changed)
}
