package arkrpc

// ArkProtocolVersionV1 is the initial Ark protocol version. The Ark protocol
// version defines how the client and operator communicate over a decodable
// mailbox transport: Ark RPC request and response shapes, round and OOR
// choreography, application-level validation, and service/method routing
// expectations.
//
// It is negotiated through the direct GetInfo bootstrap RPC and bound to a
// client runtime for its lifetime. This constant is deliberately separate
// from the mailbox transport version and the VTXO construction version, which
// evolve independently.
const ArkProtocolVersionV1 uint32 = 1

// ArkProtocolVersionV2 identifies the complete one-confirmation reorg-safety
// contract: durable lineage registration, fail-closed admission, reversible
// effects through the negotiated policy horizon, and recovery on both peers.
// Defining the compatibility boundary does not enable it; clients must not
// advertise v2 until every required gate and end-to-end proof is present.
const ArkProtocolVersionV2 uint32 = 2
