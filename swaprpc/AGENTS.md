# swaprpc

## Purpose

Generated gRPC/REST/mailbox-RPC stubs for `SwapService`, the external swap
server API consumed by the client SDK: Lightning<->Ark swaps (in-swap/
out-swap), channel-ID allocation for Lightning-to-Ark receives, and durable
credit funding/redemption/listing. Proto source: `swaprpc/swap.proto`
(REST-gateway rules in `swap.yaml`). Almost entirely generated; the one
hand-written file is `credit_account_auth.go`, which defines the canonical
digest and signing message for account-scoped credit requests so client and
server cannot disagree on what a signature covers.

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
- `CreditAccountAuthorization` — Per-request authorization attached to the six
  account-scoped RPCs: `ExpiresAtUnix`, `Nonce`, `Signature`.
- `CreditAccountRequestDigest(req)` (hand-written) — Returns the canonical
  `[32]byte` digest and the account key committed by one account-scoped
  request. Clears the authorization field on a clone, marshals
  deterministically, and prefixes the fully-qualified gRPC method name plus a
  NUL separator before BIP-340 tagged-hashing under `CreditAccountRequestTag`.
- `CreditAccountAuthMessage` / `CreditAccountAuthDigest` (hand-written) — The
  message and BIP-340 tagged digest (`CreditAccountAuthTag`) the wallet
  identity key actually signs: `accountKey || requestDigest ||
  BigEndian(expiresAtUnix) || nonce`.
- `CreditAccountAuthorizationForRequest` / `SetCreditAccountAuthorization`
  (hand-written) — Typed get/set over the six supported request messages;
  both return an error for any other type.
- `CreditAccountRequestTag` / `CreditAccountAuthTag` /
  `CreditAccountNonceSize` (32) / `CreditAccountMaxAuthTTL` (5 min) —
  Protocol constants for the scheme.

## Relationships

- **Depends on**: `mailbox/rpc` (mailbox-RPC runtime types used by the
  generated mailbox client/server), `google.golang.org/grpc`,
  `grpc-ecosystem/grpc-gateway/v2` (REST gateway in `swap.pb.gw.go`),
  `btcd/chainhash` + `protobuf/proto` (canonical digest construction).
- **Depended on by**: `sdk/swaps` (`grpc_conn.go`, `credit_account_auth.go`,
  `out_swap_mailbox.go` — typed clients for the swap FSM and the client half
  of the authorization scheme), `waved` (`rpc_wallet.go` — signs the
  authorization with the daemon identity key), `sdk/ark` (the
  `SignCreditAccountAuth` daemon RPC wrapper), `rpc/restclient` (REST
  transport adapter). `swapclientserver` implements `rpc/swapclientrpc`, not
  this package — only its tests import `swaprpc`.

## Invariants

- **Never edit generated code** (`swap.pb.go`, `swap_grpc.pb.go`,
  `swap.pb.gw.go`, `swap_mailboxrpc.pb.go`) — regenerate via `make rpc` after
  editing `swap.proto` or `swap.yaml`.
- `SettlementType.SETTLEMENT_TYPE_UNSPECIFIED` (0) is treated as Lightning
  for backward compatibility with older server responses; do not repurpose
  the zero value.
- `SwapMailboxEvent` is a proto oneof: read the populated variant, don't
  assume `OutSwapHtlcEvent` is the only case as new event kinds are added.
- **The signature never commits to itself.** `CreditAccountRequestDigest`
  clears `account_authorization` on a *clone* before marshaling. Signing the
  populated message would make verification impossible, and mutating the
  caller's request in place would corrupt it.
- **The digest is method-bound.** The fully-qualified gRPC method name plus a
  NUL byte prefixes the serialized request, so an authorization minted for
  `ListCredits` cannot be replayed against `RedeemCredit` even when the two
  requests serialize identically.
- **Marshaling must stay `Deterministic: true`.** Protobuf map/field ordering
  is otherwise unspecified, and a non-deterministic encoding would make signer
  and verifier compute different digests for the same request.
- `CreditAccountRequestTag` and `CreditAccountAuthTag` are two *distinct*
  BIP-340 tags. Collapsing them would let a request digest be presented as an
  authorization digest.
- Adding a new account-scoped RPC means extending **all three** switch
  statements (`unsignedCreditAccountRequest`,
  `SetCreditAccountAuthorization`, `CreditAccountAuthorizationForRequest`)
  plus its method constant. Each defaults to an "unsupported request" error
  rather than passing through unsigned, so a missed case fails closed.
- `RequestChannelIdRequest` commits to `client_vhtlc_pubkey` as its account
  key; every other supported request commits to `account_pubkey`.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
