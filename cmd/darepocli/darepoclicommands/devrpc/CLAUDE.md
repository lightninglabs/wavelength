# devrpc

## Purpose

Generated low-level developer RPC console for `darepocli`. Introspects proto
descriptors at runtime to build a cobra command tree that covers all
`daemonrpc` and `swapclientrpc` methods. Flags are auto-generated from
message fields with smart flattening of nested scalar messages; responses are
printed as JSON. Intended for operator debugging and runbook use, not for
end-user flows (which use the top-level wallet verbs).

The `registry_generated.go` file in this package is a generated artifact
produced by `cmd/darepocli/internal/gen-devrpc`; do not edit it manually.

## Key Types

- `Config` — Integration contract the parent CLI must satisfy: `GetConn`
  (returns a live `*grpc.ClientConn`), `PrintJSON` (renders a proto message),
  `MapRPCError` (translates gRPC status to a user-friendly error).
- `NewDevCmd(cfg Config) *cobra.Command` — Creates the root `dev` command tree
  with one sub-command per service and one leaf per method.
- (internal) `serviceSpec` / `methodSpec` — Runtime service and method
  metadata from `registry_generated.go`, including proto-descriptor references,
  display name, alias, and extracted doc comment.
- (internal) `fieldBinder` — Maps a protobuf field descriptor to a cobra flag.
  Scalar fields become string/bool flags; repeated scalars become string-array
  flags; complex fields become `--field.name-json` flags; nested scalar
  messages are flattened with dot notation.

## Field Binding Strategy

| Field type | Flag shape |
|------------|-----------|
| Scalar string/int/bytes | `--field-name` string |
| Bool | `--field-name` bool (default false) |
| Repeated scalar | `--field-name` string array |
| Nested scalar message | `--parent.child` (flattened, once per type) |
| Complex / message | `--field-name-json` raw JSON |

Oneof fields are validated after parsing: at most one member of each oneof
may be set; violations return a clear error before the RPC fires.

## Relationships

- **Depends on**: `google.golang.org/protobuf` (reflection, dynamic messages,
  JSON marshalling), `google.golang.org/grpc` (invocation, streaming),
  `daemonrpc` and `rpc/swapclientrpc` (proto descriptor references in
  `registry_generated.go`).
- **Depended on by**: `cmd/darepocli/darepoclicommands` (`root.go` registers
  the dev command returned by `NewDevCmd`).
- **Sends**: user-invoked gRPC calls via `conn.Invoke` (unary) or
  `conn.NewStream` (server-streaming).
- **Receives**: proto responses over the gRPC connection.

## Invariants

- Client-streaming RPC methods are rejected with a clear error; only unary
  and server-streaming methods are supported.
- Bytes fields parse hex strings (with optional `0x` prefix and spaces).
- Enum fields accept both numeric values and name strings; name matching
  normalises initialisms (`VTXO`→`Vtxo`, `OOR`→`Oor`) for ergonomic input.
- `--json` raw payload is merged BEFORE flag-level field binders, so explicit
  flags override the JSON body rather than the reverse.
- Proto unmarshalling uses `DiscardUnknown: true` for forward compatibility
  with proto evolution.
- Service and method lists in `generatedRegistry()` are sorted for stable JSON
  output.

## Deep Docs

- [cmd/darepocli/internal/gen-devrpc/CLAUDE.md](../../internal/gen-devrpc/CLAUDE.md)
  — Code generator that produces `registry_generated.go`.
- [cmd/darepocli/darepoclicommands/CLAUDE.md](../CLAUDE.md) — Parent CLI
  package; registers `dev` as a sub-command.
- [ARCHITECTURE.md](../../../../ARCHITECTURE.md) — System-wide package map.
