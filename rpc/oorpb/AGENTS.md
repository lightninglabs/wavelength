# rpc/oorpb

## Purpose

Generated protobuf/gRPC stubs for `oorpb.OORMailboxService` — the two-phase
submit/finalize wire contract for out-of-round (OOR) Ark transfers, plus a
hand-written `payloads.go` that converts between the wire messages and
domain types (`psbt.Packet`, `wire.OutPoint`, `oortx.RecipientOutput`).
Proto source: `rpc/oorpb/oorwire.proto`.

## Key Types

- `OORMailboxService` — `SubmitPackage` (submit an Ark PSBT + checkpoint
  PSBTs, get back server co-signed checkpoints) and `FinalizePackage`
  (submit finalized checkpoint PSBTs for a session).
- `SubmitPackageRequest` — Carries the serialized Ark PSBT,
  `checkpoint_psbts`, per-input `signing_descriptors`
  (`OORSigningDescriptor`: policy template, spend path, owner-leaf policy),
  and `recipient_outputs` (`OORRecipientOutput`: pk_script, value_sat,
  optional `vtxo_policy_template`).
- `SubmitPackageResponse` — `oneof result`: `SubmitPackageSuccess`
  (session_id, co-signed checkpoint PSBTs, `co_signed_ark_psbt` for
  unilateral-recovery persistence) or `SubmitPackageRejection` (typed
  `OORRejectCode` + human reason + echoed session_id).
- `OORRejectCode` — Typed rejection causes so clients can route on cause
  without string-matching: `OOR_REJECT_LINEAGE_TOO_LARGE` (on-chain claim
  cost too high, retryable after restructuring inputs),
  `OOR_REJECT_OUTPUT_POLICY` (output violates operator policy, e.g. exceeds
  max per-VTXO amount — retrying the same shape will fail again),
  `OOR_REJECT_USER_BALANCE` (would push recipient's aggregate balance above
  the operator's `MaxUserBalance` cap — unlike OUTPUT_POLICY, retrying the
  same shape can succeed later once the recipient's balance drops).
- `FinalizePackageRequest`/`Response` — session_id + final checkpoint PSBTs.
- `SigningDescriptor` (domain type in `payloads.go`) — Go-native mirror of
  `OORSigningDescriptor` (`wire.OutPoint`, policy template, spend path,
  owner-leaf policy) used by callers that don't want to touch proto types
  directly.
- `SubmitRejectedError` (`payloads.go`) — Typed Go error wrapping
  `OORRejectCode`/reason, returned by `ParseSubmitPackageResponse` so callers
  can `errors.As` into it instead of inspecting the oneof.
- `NewSubmitPackageRequest` / `ParseSubmitPackageRequest` /
  `NewSubmitPackageResponse` / `NewSubmitPackageRejection` /
  `ParseSubmitPackageResponse` / `NewFinalizePackageRequest` /
  `ParseFinalizePackageRequest` / `NewFinalizePackageResponse` /
  `ParseFinalizePackageResponse` — Typed builder/parser pairs in
  `payloads.go` that are the intended way to construct and consume these
  messages (rather than touching generated struct fields directly).

## Relationships

- **Depends on**: `lib/tx/oor` (`oortx.RecipientOutput` domain type),
  `lib/tx/psbtutil` (PSBT serialize/parse helpers).
- **Depended on by**: `oor` (builds/parses submit and finalize
  payloads for the client-side OOR FSM), `darepod` (server-side OOR actor
  handling submit/finalize).
- **Sends** (via `oor` as the client-side caller): `SubmitPackageRequest`,
  `FinalizePackageRequest` — routed over the mailbox transport using
  `ServiceName`/`MethodSubmitPackage`/`MethodFinalizePackage`.
- **Receives**: `SubmitPackageResponse` (success or typed rejection),
  `FinalizePackageResponse`; `MethodIncomingAck` names the client-side
  incoming-transfer acknowledgment path (see `arkrpc.IncomingOOREvent` /
  `arkrpc.OORRecipientEvent` for the notify-then-query flow this
  acknowledges).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`; `payloads.go`
  is hand-written and is the one file in this package that may be edited
  directly.
- `ServiceName` (`"oorpb.OORMailboxService"`) and the `Method*` constants in
  `payloads.go` must match the proto service/method names exactly; they are
  used for mailbox RPC-overlay routing (`RpcMeta.service`/`method`).
- `co_signed_ark_psbt` on `SubmitPackageSuccess` is additive: older
  operators may omit it, and `ParseSubmitPackageResponse` treats empty bytes
  as "operator did not include the artifact" rather than a parse error, so
  clients keep talking to older operators during a rolling upgrade.
- `SubmitPackageRejection.session_id` must be decoded best-effort even on a
  malformed session id, because the durable `EventRouter` dispatch path
  needs to route the failure to the correct OOR session FSM rather than
  stalling the ingress cursor on an undispatchable envelope.
- `OORRejectCode` values are additive and must not be renumbered; clients
  match on the numeric enum, and mis-routing a rejection cause (e.g. treating
  a retryable `USER_BALANCE` rejection like a non-retryable
  `OUTPUT_POLICY` one) causes incorrect retry/fallback behavior.

## Deep Docs

- [oor/CLAUDE.md](../../oor/CLAUDE.md) — Client-side OOR FSM that is the
  primary caller of these builders/parsers (if present; otherwise see
  `ARCHITECTURE.md`).
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
