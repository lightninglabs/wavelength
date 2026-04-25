//go:build itest

package itest

import (
	"testing"

	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
)

// TestSealTimeFeeQuoteAdminGetRoundStatusUnknown verifies the admin
// handler's round-not-found path: an unknown UUID must produce a
// clear "not found" reply rather than a deserialization panic or a
// stale-state leak. Guards the operator-facing error surface.
//
// The happy-path coverage for GetRoundStatus is folded into
// TestBoardingIntegrationSingleClient (which already runs a
// confirmed round); the seal-time-quote happy path itself is folded
// into TestRefreshIntegrationSingleVTXOLifecycle (which already
// asserts the refreshed VTXO amount equals the seal-time quote
// residual via expectedNetAfterRefresh). Keeping this test
// standalone preserves the empty-harness precondition the negative
// case requires.
func TestSealTimeFeeQuoteAdminGetRoundStatusUnknown(t *testing.T) {
	t.Parallel()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()

	// A valid-shape UUID that the actor has never seen. The handler
	// must reply cleanly rather than panic or block.
	const unknownRoundID = "00000000-0000-0000-0000-000000000001"

	_, err := h.ArkAdminClient.GetRoundStatus(
		t.Context(), &adminrpc.GetRoundStatusRequest{
			RoundId: unknownRoundID,
		},
	)
	require.Error(t, err,
		"unknown round_id must surface as an RPC error rather than "+
			"a zero-value success reply")
}
