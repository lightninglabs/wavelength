// Package mobile is a gomobile-safe facade over sdk/wavewalletdk. It wraps the
// hand-written wallet SDK in a flat API that respects gomobile's type
// restrictions: no context.Context, no channels, no maps, no slices other
// than []byte, and no unsigned integers crossing the boundary.
//
// The package borrows the falafel / lnd-mobile bytes-out idea without the
// protoc generator or its callback interfaces, because sdk/wavewalletdk already
// owns the in-process bufconn gRPC wiring and wavewalletdk.Start returns once
// gRPC is serving (lnd.Main blocks forever, so lnd needs a callback; we do
// not). RPC verbs use a JSON bytes-in / bytes-out convention (the host decodes
// with kotlinx.serialization / Codable, no protobuf runtime needed), with a
// handful of scalar convenience methods for the hottest paths. Start is
// synchronous (call it off the main thread), and the one streaming verb,
// Subscribe, hands back a pull-based Subscription handle (Next / Close) instead
// of a callback, since a Go channel cannot cross the gomobile boundary.
//
// Everything is gated behind the mobile, wavewalletrpc, and swapruntime build
// tags so the package only compiles into a gomobile bind output; ordinary
// go build ./... sees the empty stub in stub.go.
package mobile
