# swaprpc

## Purpose

Generated gRPC/REST/mailbox-RPC stubs for `SwapService`, the external swap
server API consumed by the client SDK: Lightning<->Ark swaps (in-swap/
out-swap), channel-ID allocation for Lightning-to-Ark receives, and durable
credit funding/redemption/listing. Proto source: `swaprpc/swap.proto`
(REST-gateway rules in `swap.yaml`).

Almost entirely generated. The exceptions are the hand-written canonical
message constructions that both client and server must agree on byte for
byte — `out_swap_ack.go` and `credit_account_auth.go` — which live here
because they are part of the wire contract, not of either side's logic.

## Key Types

- `SwapServiceClient` / `SwapServiceServer` — standard gRPC client/server
  interfaces.
- `SwapServiceMailboxClient` / `SwapServiceMailboxServer` — durable-mailbox
  transport bindings (`mailbox/rpc.RPCClient`/`Router`) for the same RPCs.
- `SettlementType` — which path backs a swap: `LIGHTNING`, `IN_ARK`,
  `CREDIT`, or `MIXED` (funded by both a vHTLC and reserved credit).
- `SwapMailboxEvent` — oneof wrapper for server-pushed mailbox events
  (`OutSwapHtlcEvent`, and others as added) delivered outside the
  request/response RPCs.
- `CreditOperationState` / `CreditOperationType` — externally visible FSM
  states/kinds for durable credit funding, pay, redemption, and receive
  operations.
- Request/response messages per RPC (`CreateInSwapRequest/Response`,
  `QuoteInSwapRequest/Response`, `CreateCreditRequest/Response`,
  `RedeemCreditRequest/Response`, `ListCreditsRequest/Response`,
  `AuthorizeInSwapRefundRequest/Response`,
  `AcknowledgeOutSwapHtlcRequest/Response`,
  `SignInSwapForfeitRequest/Response`,
  `SubmitOutSwapForfeitSignatureRequest/Response`,
  `RequestChannelIdRequest/Response`).
- `CreditAccountAuthorization` — per-request authorization proving the caller
  holds the wallet identity key named by an account-scoped request. Carries
  `ExpiresAtUnix`, a 32-byte `Nonce`, and a 64-byte BIP-340 `Signature`.

### Hand-written canonical constructions

- `CreditAccountRequestDigest(req)` — returns the canonical request digest and
  the account key committed by it. Clears the authorization field and marshals
  deterministically, so the signature never commits to itself, then tags
  `method || 0x00 || encoded` with `CreditAccountRequestTag`.
- `CreditAccountAuthMessage` / `CreditAccountAuthDigest` — the message and
  BIP-340 tagged digest an identity key signs over
  `accountKey || requestDigest || bigEndian(expiresAtUnix) || nonce`, tagged
  with `CreditAccountAuthTag`.
- `CreditAccountAuthorizationForRequest` / `SetCreditAccountAuthorization` —
  read/write the authorization on any of the six supported request types
  (`RequestChannelId`, `CreateInSwap`, `QuoteInSwap`, `CreateCredit`,
  `RedeemCredit`, `ListCredits`); anything else is an error.
- `CreditAccountNonceSize` (32) and `CreditAccountMaxAuthTTL` (5 min) — the
  authorization nonce length and the upper bound on signed-request validity.
- `OutSwapHTLCAckMessage` (`out_swap_ack.go`) — the same idea for the
  out-swap HTLC acknowledgement.

## Relationships

- **Depends on**: `mailbox/rpc` (mailbox-RPC runtime types used by the
  generated mailbox client/server), `google.golang.org/grpc`,
  `grpc-ecosystem/grpc-gateway/v2` (REST gateway in `swap.pb.gw.go`).
- **Depended on by**: `sdk/swaps` (`grpc_conn.go`, `out_swap_mailbox.go` —
  typed clients for the swap FSM), `rpc/restclient` (REST transport
  adapter). `swapclientserver` implements `rpc/swapclientrpc`, not this
  package — only its tests import `swaprpc`.

## Invariants

- **Never edit generated code** (`swap.pb.go`, `swap_grpc.pb.go`,
  `swap.pb.gw.go`, `swap_mailboxrpc.pb.go`) — regenerate via `make rpc` after
  editing `swap.proto` or `swap.yaml`. `credit_account_auth.go` and
  `out_swap_ack.go` are hand-written and are the exception.
- **The credit-account digest is a wire contract.** Client and server derive
  it independently, so any change to the tag strings, the method name
  constants, the field ordering, or the deterministic-marshal option is a
  breaking protocol change that invalidates every in-flight authorization.
  Adding a new account-scoped RPC means adding it to *both* switch statements
  in `unsignedCreditAccountRequest` and `SetCreditAccountAuthorization`;
  missing one makes the digest unsignable rather than wrong, which is the
  intended failure direction.
- The authorization field must be cleared before the request is marshalled
  for the digest. `unsignedCreditAccountRequest` clones the request to do
  this, so callers never see their own message mutated.
- `SettlementType.SETTLEMENT_TYPE_UNSPECIFIED` (0) is treated as Lightning
  for backward compatibility with older server responses; do not repurpose
  the zero value.
- `SwapMailboxEvent` is a proto oneof: read the populated variant, don't
  assume `OutSwapHtlcEvent` is the only case as new event kinds are added.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
