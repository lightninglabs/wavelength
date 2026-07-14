package waveclicommands

import (
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestParseOORFilters verifies user-facing OOR filter names map to proto.
func TestParseOORFilters(t *testing.T) {
	t.Parallel()

	direction, err := parseOORDirectionFilter("incoming")
	require.NoError(t, err)
	require.Equal(
		t, waverpc.OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING,
		direction,
	)

	status, err := parseOORStatusFilter("failed")
	require.NoError(t, err)
	require.Equal(
		t, waverpc.OORSessionStatus_OOR_SESSION_STATUS_FAILED, status,
	)

	_, err = parseOORDirectionFilter("sideways")
	require.ErrorContains(t, err, "unknown OOR direction")

	_, err = parseOORStatusFilter("mystery")
	require.ErrorContains(t, err, "unknown OOR status")
}

// TestParseRoundStateFilter verifies user-facing round state names map to
// proto.
func TestParseRoundStateFilter(t *testing.T) {
	t.Parallel()

	state, err := parseRoundStateFilter("confirmed")
	require.NoError(t, err)
	require.Equal(
		t, waverpc.RoundState_ROUND_STATE_CONFIRMED, state,
	)

	state, err = parseRoundStateFilter("partial_sigs_sent")
	require.NoError(t, err)
	require.Equal(
		t, waverpc.RoundState_ROUND_STATE_PARTIAL_SIGS_SENT, state,
	)

	_, err = parseRoundStateFilter("definitely_not_a_state")
	require.ErrorContains(t, err, "unknown round state")
}
