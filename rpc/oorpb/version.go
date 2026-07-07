package oorpb

import "fmt"

// FlowVersion is the per-session OOR flow version: a permanent property
// describing the choreography rules (e.g. whether the transfer uses per-input
// checkpoints) under which an out-of-round transfer was conducted. It is
// hosted here, next to the wire field it travels on (the submit request's
// flow_version), so the client and operator cannot drift on the value -- both
// modules import this one definition rather than mirroring it.
type FlowVersion uint32

// FlowVersionV1 is the only OOR flow version this build understands. The client
// stamps it on the outgoing submit and it is persisted with the session, so a
// future, genuinely different OOR flow can be introduced additively without
// ambiguity about how an existing session was conducted. Until that future
// version exists, every session is V1.
//
// The versions are zero-indexed: V1 is the Go zero value, so an unstamped
// object and an omitted wire field both read as V1 with no normalization step,
// and a NOT NULL DEFAULT 0 column defaults to V1. V2 will be 1, and so on.
const FlowVersionV1 FlowVersion = 0

// ValidateFlowVersion fails closed on any OOR flow version this build does not
// understand -- i.e. any value past the latest this build knows. It is the
// guard applied at the ingress edge where a version arrives from another party
// (the operator receiving a submit's declared flow version): an unknown
// version means the transfer was conducted under rules this software does not
// implement, so it is rejected rather than acted upon.
func ValidateFlowVersion(v FlowVersion) error {
	if v != FlowVersionV1 {
		return fmt.Errorf("unknown OOR flow version %d (this build "+
			"understands only version %d)", v, FlowVersionV1)
	}

	return nil
}
