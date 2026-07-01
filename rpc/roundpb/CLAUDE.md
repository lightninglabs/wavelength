# rpc/roundpb

## Purpose

Generated protobuf/gRPC stubs for the round protocol, plus hand-written
`service.go` containing the canonical mailbox method name constants used
for routing client↔server round messages through the durable transport
layer.

## Key Types

All `*.pb.go` files are generated — never edit directly; regenerate with
`make rpc`. The manually-maintained `service.go` defines:

- `ServiceName` — Fully-qualified protobuf service name
  (`"round.v1.RoundService"`) used for mailbox event routing.
- Server→client push method names: `MethodJoinAck`, `MethodBatchInfo`,
  `MethodAwaitingInputSigs`, `MethodAggNonces`, `MethodAggSigs`,
  `MethodRoundFailed`, `MethodError`, `MethodJoinRoundQuote`.
- Client→server method names: `MethodJoinRound`, `MethodAcceptQuote`,
  `MethodRejectQuote`, `MethodSubmitNonces`, `MethodSubmitPartialSigs`,
  `MethodSubmitForfeitSigs` (boarding input sigs),
  `MethodSubmitVTXOForfeitSigs` (VTXO forfeit sigs).

`MethodSubmitForfeitSigs` and `MethodSubmitVTXOForfeitSigs` are distinct
wire methods for two different payload types; see `round/CLAUDE.md` for
the `SubmitForfeitSigRequest` vs `SubmitVTXOForfeitSigsToServer`
distinction.

- `ClientBatchInfo` — per-round parameters that used to be advertised
  globally in `arkrpc.GetInfoResponse`/`daemonrpc.ServerInfo` and are now
  scoped to the round the client joined, so an operator key rotation never
  changes what a client must agree on mid-round: `tree_cosign_key` (fresh
  per-round MuSig2 cosigner pubkey for the VTXO tree), `connector_operator_key`
  (key used to build this round's connector tree), `sweep_key`/`sweep_delay`
  (this round's VTXO-tree sweep leaf key and timelock), and `forfeit_key`
  (dedicated per-round forfeit penalty key; BIP-86 key-spend target for the
  forfeit-tx penalty output).
- `ForfeitRequest` — gained `auth_spend_path` (proves control of
  non-wallet-managed VTXOs when joining) and `forfeit_spend_path` (custom
  spend path the operator uses to build the exact forfeit tx after connector
  assignment; required for swap vHTLC refreshes whose three-party branch
  isn't inferable from a standard wallet descriptor).
- `ForfeitTxSig.participant_sigs` (`ForfeitParticipantSig`: pubkey +
  schnorr signature) — carries additional non-operator signatures for
  policy leaves needing more than one client-side participant. When
  non-empty, it is authoritative and `client_vtxo_sig` is treated as an
  optional backwards-compatible duplicate.
- `VTXORequest.fixed_amount` — requires the server quote to preserve
  `target_amount_sat` exactly, disabling the single-output implicit-change
  exception (a separate fee-bearing change output is then required if fees
  apply).

## Relationships

- **Depends on**: nothing (generated proto types only).
- **Depended on by**: `round` (outbox routing, `FromProto` helpers),
  `serverconn` (mailbox method dispatch), `darepod` (proto conversion).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- Method name constants in `service.go` must match the proto service
  definition; mismatches silently drop events at the mailbox router.

## Deep Docs

- [rpc/CLAUDE.md](../CLAUDE.md) — Parent rpc package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
