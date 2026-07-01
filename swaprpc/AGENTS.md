# swaprpc

## Purpose

Generated gRPC stubs for `swaprpc.SwapService` — the external swap-server
API consumed by the client SDK. Defines Ark↔Lightning swap negotiation
(in-swap/out-swap), server-owned sat-native credit accounts, and the
connector-bound forfeit-signature handshake used for vHTLC refresh rounds.
This is the client-facing view of a service implemented by the (separate)
swap server process, not by this repo. Proto source: `swaprpc/swap.proto`.

## Key Types

- `SwapService` — `RequestChannelId` (allocate a receive route hint),
  `CreateInSwap`/`QuoteInSwap` (Ark→Lightning payment negotiation/preview),
  `CreateCredit`/`RedeemCredit`/`ListCredits` (credit account),
  `AuthorizeInSwapRefund` (server signs a cooperative refund after a
  terminal Lightning failure), `AcknowledgeOutSwapHtlc` (receiver confirms
  durable acceptance of an intercepted HTLC before the server funds the
  vHTLC), `SignInSwapForfeit`/`SubmitOutSwapForfeitSignature`
  (connector-bound forfeit signature exchange for vHTLC refresh rounds).
- `SettlementType` — LIGHTNING, IN_ARK, CREDIT, or MIXED; UNSPECIFIED is
  accepted as LIGHTNING for backward compatibility with older responses.
- `OutSwapHtlcEvent` / `InArkHtlcEvent` — Mailbox-delivered notifications
  (see `SwapMailboxEvent`) informing a receiver that the server has
  accepted funding responsibility for an intercepted Lightning HTLC
  (out-swap) or that a same-Ark sender funded/accepted a direct vHTLC
  (in-swap). `OutSwapHtlcEvent.parts` supports multi-part payments (each
  `OutSwapHtlcPart` carries its own onion blob); when empty, `onion_blob`
  on the event itself is the legacy single-part shape.
- `SwapMailboxEvent` — `oneof` wrapper for the three swap event kinds
  carried over the mailbox: `out_swap_htlc`, `in_ark_htlc`,
  `out_swap_forfeit_signature_request`.
- `VHTLCConfig` — Timelocks (`refund_locktime`, unilateral claim/refund
  delays) and the `swapserver_pubkey` that define one vHTLC output; shared
  shape across in-swap and out-swap negotiation responses/events.
- `ForfeitSignaturePayload` / `ForfeitParticipantSignature` — The exact
  connector-bound forfeit transcript one vHTLC refresh participant signs
  (mirrors `daemonrpc.SignVTXOForfeitRequest`/`ForfeitSigningContext` shape)
  and the raw Schnorr signature response. `OutSwapForfeitSignatureRequest`
  carries the payload to the receiver over the mailbox;
  `SubmitOutSwapForfeitSignatureRequest` echoes it back with the receiver's
  signature for idempotency/transcript validation.
- `CreditQuote` / `CreditFundingSource` / `CreditOperationType` /
  `CreditOperationState` / `CreditOperation` / `CreditLedgerEntry` — Same
  credit-account vocabulary as `rpc/swapclientrpc` (funding source,
  operation lifecycle, ledger rows); kept in sync field-for-field since the
  daemon proxies these RPCs to the swap server.
- `TaprootScriptSignature` — Externally produced tapscript signature
  (pubkey, witness script, signature, sighash); explicitly kept in sync with
  `daemonrpc.TaprootScriptSignature`.

## Relationships

- **Depends on**: `google.protobuf.Timestamp`.
- **Depended on by**: `sdk/swaps` (client-side swap SDK; primary caller of
  `SwapServiceClient` and consumer of `SwapMailboxEvent`/
  `OutSwapForfeitSignatureRequest` over the mailbox), `swapclientserver`
  (daemon-side bridge that proxies `swapclientrpc` calls to this service),
  `rpc/restclient` (`SwapServiceClient` REST wrapper), `daemonrpc`
  (cross-references this package's proto types for shared shapes).
- **Sends** (client → swap server): `CreateInSwapRequest`,
  `QuoteInSwapRequest`, `CreateCreditRequest`, `RedeemCreditRequest`,
  `ListCreditsRequest`, `AuthorizeInSwapRefundRequest`,
  `AcknowledgeOutSwapHtlcRequest`, `SignInSwapForfeitRequest`,
  `SubmitOutSwapForfeitSignatureRequest`, `RequestChannelIdRequest`.
- **Receives** (swap server → client, over the mailbox, not gRPC):
  `SwapMailboxEvent` wrapping `OutSwapHtlcEvent`, `InArkHtlcEvent`, or
  `OutSwapForfeitSignatureRequest`.

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- The service implementation lives in the separate swap-server process;
  this repo only calls it (via `sdk/swaps` and `swapclientserver`) and
  receives its mailbox-delivered events. Do not add a local
  `SwapServiceServer` implementation here.
- `OutSwapHtlcEvent`/`InArkHtlcEvent` are notify-then-validate: the receiver
  must validate the vHTLC script/onion against the invoice payment hash and
  sender/server key before acting, and must call
  `AcknowledgeOutSwapHtlc` only after durably accepting the event — the
  server waits for that acknowledgement before funding the Ark vHTLC.
- `ForfeitSignaturePayload.request_id` is a stable idempotency key for one
  signing request; `SubmitOutSwapForfeitSignatureRequest` echoes the full
  payload (not just the id) so the server can validate the receiver signed
  the exact transcript it published, not a stale or substituted one.
- `TaprootScriptSignature` and the credit-vocabulary enums
  (`CreditFundingSource`, `CreditOperationType`, `CreditOperationState`)
  must be kept field-for-field in sync with their `daemonrpc`/
  `rpc/swapclientrpc` counterparts; the daemon proxies requests/responses
  between these shapes without a translation layer.

## Deep Docs

- [sdk/swaps/CLAUDE.md](../sdk/swaps/CLAUDE.md) — Client-side swap SDK that
  is the primary consumer of this service and its mailbox events.
- [swapwallet/CLAUDE.md](../swapwallet/CLAUDE.md) — Daemon-side swap
  orchestration that surfaces `rpc/swapclientrpc`, which proxies to this
  service.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
