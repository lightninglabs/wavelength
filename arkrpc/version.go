package arkrpc

// ArkProtocolVersionV1 is the initial Ark protocol version. The Ark protocol
// version defines how the client and operator communicate over a decodable
// mailbox transport: Ark RPC request and response shapes, round and OOR
// choreography, application-level validation, and service/method routing
// expectations.
//
// It is negotiated through the direct GetInfo bootstrap RPC and bound to a
// client runtime for its lifetime. Production currently supports only v1; a
// synthetic v2 may be configured in tests to exercise selection, binding, and
// rejection behavior, but no production default advertises a version beyond
// v1. This constant is deliberately separate from the mailbox transport
// version and the VTXO construction version, which evolve independently.
const ArkProtocolVersionV1 uint32 = 1
