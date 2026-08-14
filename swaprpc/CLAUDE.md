# swaprpc

## Purpose

Generated gRPC/REST/mailbox-RPC stubs for `SwapService`, the external swap
server API consumed by the client SDK: Lightning<->Ark swaps (in-swap/
out-swap), channel-ID allocation for Lightning-to-Ark receives, and durable
credit funding/redemption/listing. Proto source: `swaprpc/swap.proto`
(REST-gateway rules in `swap.yaml`).

Almost all generated. The one hand-written file is
`credit_account_auth.go`, which defines the canonical digests a client
signs to prove it owns a credit account, and is deliberately here rather
than in `sdk/swaps` so client and server derive the transcript from a
single definition.

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
- `CreditAccountAuthorization` — per-request proof that the caller holds
  the account identity key: `ExpiresAtUnix`, a 32-byte `Nonce`, and a
  BIP-340 `Signature`. Carried on every account-scoped request message.
- Hand-written credit-account auth helpers (`credit_account_auth.go`):
  `CreditAccountRequestDigest` (canonical request digest + the account
  key the signature commits to), `CreditAccountAuthMessage` /
  `CreditAccountAuthDigest` (the signed message and its tagged hash),
  `CreditAccountAuthorizationForRequest` / `SetCreditAccountAuthorization`
  (read/attach the authorization on any supported request), and the
  constants `CreditAccountRequestTag`, `CreditAccountAuthTag`,
  `CreditAccountNonceSize` (32), `CreditAccountMaxAuthTTL` (5 min).
- Request/response messages per RPC (`CreateInSwapRequest/Response`,
  `QuoteInSwapRequest/Response`, `CreateCreditRequest/Response`,
  `RedeemCreditRequest/Response`, `ListCreditsRequest/Response`,
  `AuthorizeInSwapRefundRequest/Response`,
  `AcknowledgeOutSwapHtlcRequest/Response`,
  `SignInSwapForfeitRequest/Response`,
  `SubmitOutSwapForfeitSignatureRequest/Response`,
  `RequestChannelIdRequest/Response`).

## Relationships

- **Depends on**: `mailbox/rpc` (mailbox-RPC runtime types used by the
  generated mailbox client/server), `google.golang.org/grpc`,
  `grpc-ecosystem/grpc-gateway/v2` (REST gateway in `swap.pb.gw.go`),
  `btcd/chainhash/v2` + `protobuf/proto` (tagged-hash digests and
  deterministic marshalling in `credit_account_auth.go`). Still no repo
  packages — the auth helpers are kept dependency-free so both a client
  and a server can import them.
- **Depended on by**: `sdk/swaps` (`grpc_conn.go`, `out_swap_mailbox.go` —
  typed clients for the swap FSM; `credit_account_auth.go` — signs the
  digests defined here), `sdk/ark` (`CreditAccountNonceSize` in the
  daemon signing call), `rpc/restclient` (REST transport adapter).
  `swapclientserver` implements `rpc/swapclientrpc`, not this package —
  only its tests import `swaprpc`.

## Invariants

- **Never edit generated code** (`swap.pb.go`, `swap_grpc.pb.go`,
  `swap.pb.gw.go`, `swap_mailboxrpc.pb.go`) — regenerate via `make rpc` after
  editing `swap.proto` or `swap.yaml`. `credit_account_auth.go` is the
  exception: it is hand-written and `make rpc` will not touch it.
- `CreditAccountRequestDigest` clears the authorization field on a
  **clone** before marshalling, so the signature never commits to itself
  and the caller's request is left untouched. Marshalling is
  `proto.MarshalOptions{Deterministic: true}` — a non-deterministic
  encode would make the client's digest and the server's disagree on
  semantically identical messages.
- The digest commits to the **full gRPC method name** (`method || 0x00 ||
  encoded`) under `CreditAccountRequestTag`. That domain separation is
  what stops an authorization minted for one RPC being replayed against
  another whose request happens to encode identically.
- Adding a new account-scoped RPC means touching **four** places in
  `credit_account_auth.go`: the method constant, and a case in each of
  `CreditAccountAuthorizationForRequest`, `SetCreditAccountAuthorization`,
  and `unsignedCreditAccountRequest`. Miss the last and the request is
  signed over a transcript containing its own signature; miss the others
  and it fails closed with "unsupported credit account request".
- `CreditAccountMaxAuthTTL` (5 min) is a **ceiling the verifier applies**,
  not the client's chosen expiry. A far-future `ExpiresAtUnix` does not
  extend validity; `sdk/swaps` signs for one minute.
- `SettlementType.SETTLEMENT_TYPE_UNSPECIFIED` (0) is treated as Lightning
  for backward compatibility with older server responses; do not repurpose
  the zero value.
- `SwapMailboxEvent` is a proto oneof: read the populated variant, don't
  assume `OutSwapHtlcEvent` is the only case as new event kinds are added.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
