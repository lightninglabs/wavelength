package roundpb

import "fmt"

// FlowVersion is the per-round flow version: a permanent property describing
// the choreography rules (signing ceremony, tree building) under which a round
// was conducted. It is hosted here, next to the wire field it travels on
// (ClientBatchInfo.flow_version), so the operator and client cannot drift on
// the value -- both modules import this one definition rather than mirroring
// it.
type FlowVersion uint32

// FlowVersionV1 is the only round flow version this build understands. It is
// stamped when the operator creates the round and read back whenever the round
// is loaded, so a future, genuinely different round flow can be introduced
// additively without ambiguity about how an existing round was conducted.
// Until that future version exists, every round is V1.
//
// The versions are zero-indexed: V1 is the Go zero value, so an unstamped
// object and an omitted wire field both read as V1 with no normalization step,
// and a NOT NULL DEFAULT 0 column defaults to V1. V2 will be 1, and so on.
const FlowVersionV1 FlowVersion = 0

// ValidateFlowVersion fails closed on any round flow version this build does
// not understand -- i.e. any value past the latest this build knows. It is the
// guard applied at the ingress edge where a version arrives from another party
// (the client receiving a round's flow version from the operator): an unknown
// version means the round was conducted under rules this software does not
// implement, so it is rejected rather than acted upon.
func ValidateFlowVersion(v FlowVersion) error {
	if v != FlowVersionV1 {
		return fmt.Errorf("unknown round flow version %d (this build "+
			"understands only version %d)", v, FlowVersionV1)
	}

	return nil
}
