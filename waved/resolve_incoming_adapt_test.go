package waved

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/stretchr/testify/require"
)

// TestAdaptResolveIncomingResultMapsSemanticErrorToFailEvent verifies that a
// phase-1 resolve response which parses but is semantically invalid is mapped
// to a FailEvent driving the session to terminal Failed, rather than bubbling
// an Adapt error that would replay the offending envelope forever and wedge all
// subsequent OOR mailbox ingress.
func TestAdaptResolveIncomingResultMapsSemanticErrorToFailEvent(t *testing.T) {
	t.Parallel()

	sessionID := oor.SessionID(chainhash.Hash{0x11, 0x22, 0x33})
	limits := oor.DefaultReceiveLimits()

	testCases := []struct {
		name string
		resp *arkrpc.ListOORRecipientEventsByScriptResponse
	}{{
		// An empty event list is the canonical "operator answered with
		// nothing useful" case for a known session id.
		name: "empty events",
		resp: &arkrpc.ListOORRecipientEventsByScriptResponse{},
	}, {
		// A recipient event whose id does not match the correlation id
		// is attributable to this session but semantically wrong.
		name: "recipient event id mismatch",
		resp: &arkrpc.ListOORRecipientEventsByScriptResponse{
			Events: []*arkrpc.OORRecipientEvent{{
				EventId:   99,
				SessionId: sessionID[:],
			}},
		},
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The correlation id carried recipient event id 7; the
			// responses above do not satisfy it.
			msg := adaptResolveIncomingResult(
				sessionID, 7, tc.resp, limits,
			)
			require.NotNil(t, msg)
			require.Equal(t, sessionID, msg.SessionID)

			// The driving event must be a FailEvent so the session
			// is driven to terminal Failed and the cursor advances,
			// not a raw error.
			fail, ok := msg.Event.(*oor.FailEvent)
			require.True(
				t, ok, "expected FailEvent, got %T", msg.Event,
			)
			require.Contains(
				t, fail.Reason, "resolve incoming transfer",
			)
		})
	}
}
