package arkrpc

import "fmt"

// ConstructionVersion is the per-VTXO construction version: a permanent
// property describing the script / spend / exit rules a VTXO was built under.
// It is hosted here, next to the wire field it travels on (VTXO's
// construction_version in indexer.proto), so whoever creates a VTXO (round or
// OOR) and whoever later spends or exits it cannot drift on the value -- both
// modules import this one definition rather than mirroring it. It is
// deliberately separate from the Ark protocol version (see
// ArkProtocolVersionV1): the protocol version is a negotiated, per-session
// property, whereas the construction version is a durable, per-object one.
type ConstructionVersion uint32

// ConstructionVersionV1 is the only VTXO construction version this build
// understands. It is stamped when a VTXO is first created and read back
// whenever it is loaded, so that a future, genuinely different VTXO
// construction can be introduced additively without ambiguity about how an
// existing VTXO must be spent or exited. Until that future version exists,
// every VTXO is V1.
//
// The versions are zero-indexed: V1 is the Go zero value, so an unstamped
// object and an omitted wire field both read as V1 with no normalization step,
// and a NOT NULL DEFAULT 0 column defaults to V1. V2 will be 1, and so on.
const ConstructionVersionV1 ConstructionVersion = 0

// ValidateConstructionVersion fails closed on any construction version this
// build does not understand -- i.e. any value past the latest this build
// knows. It is applied at the ingress edge where a version arrives from
// another party (a client adopting a VTXO's construction version from an
// operator/indexer response): an unknown version means the VTXO was built
// under rules this software cannot safely spend, so it is rejected rather than
// acted upon.
func ValidateConstructionVersion(v ConstructionVersion) error {
	if v != ConstructionVersionV1 {
		return fmt.Errorf("unknown VTXO construction version %d (this "+
			"build understands only version %d)", v,
			ConstructionVersionV1)
	}

	return nil
}
