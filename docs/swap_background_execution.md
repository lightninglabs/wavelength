# Optional Background Swap Execution

This note sketches a small, opt-in path for moving client-side swap
execution into `waved` without making every default daemon or CLI build
carry the swap runtime yet.

## Problem

The swap SDK already has durable pay and receive FSMs under `sdk/swaps`,
but production swap execution needs a single owner. A short-lived CLI
process should not be the thing driving progress. If the CLI exits, swap
progress should continue in the daemon instead of waiting for another
manual command.

That is fine for manual regtest work, but it is not the model we want for
WaveWalletDK or application integrations. A user should submit a swap intent,
receive a stable payment hash or invoice, and then let the long-lived
client runtime complete, refund, expire, or mark the swap as requiring
intervention.

## Recommendation

Add a daemon-owned swap executor behind a build tag, tentatively:

```text
swapruntime
```

Default builds remain unchanged from a dependency and runtime perspective:

```shell
make build
```

Swap-enabled builds opt in explicitly:

```shell
make build-swapruntime
make unit-swapruntime
```

The tag gates both sides that would otherwise embed swap dependencies:

- `waved` starts the background swap executor only in `swapruntime`
  builds.
- `wavecli` exposes daemon-backed `swap` commands only in
  `swapruntime` builds.
- the CLI talks to a daemon-hosted swap subserver; it does not import
  `sdk/swaps`, open the swap DB, or dial `swapdk-server` directly.

This keeps the feature easy to review and test while avoiding a broad
default-build dependency expansion before the runtime contract is settled.

## Build-Tag Boundary

Use one tag for the first slice. Splitting daemon and CLI tags now would
create unsupported combinations without much benefit.

Suggested file layout:

```text
rpc/swapclientrpc/swap_client.proto
swapclientserver/*.go                             //go:build swapruntime
cmd/waved/swapruntime.go                        //go:build swapruntime
cmd/waved/swapruntime_stub.go                   //go:build !swapruntime
cmd/wavecli/waveclicommands/cmd_swap_rpc.go   //go:build swapruntime
cmd/wavecli/waveclicommands/cmd_swap_stub.go  //go:build !swapruntime
```

The stub path should be boring:

- `waved` compiles with no swap executor and no swap server dialer.
- `waved` does not register the swap subserver in non-`swapruntime`
  builds.
- `wavecli` exposes a tiny command that returns `swap support requires
  building with tags="swapruntime"`.

A stub command keeps swap support discoverable in non-swapruntime builds
without linking a second CLI-side swap runtime into ordinary binaries.

The tagged CLI command should depend on the generated swap subserver client,
not `sdk/swaps`. It should be structurally similar to `cmd_getinfo.go`: parse
flags, build a protobuf request, call the daemon-hosted service, and render the
protobuf response. Any richer Go models can live in `sdk/ark` or a future
WaveWalletDK-facing package, but the CLI should not become a second swap runtime.

The daemon core should also avoid importing `sdk/swaps` directly. Today
`sdk/swaps -> sdk/ark -> waved`, so a direct import from `waved` creates an
import cycle. Register the optional subserver through a programmatic daemon RPC
registrar and place the swap runtime in a package outside `waved`.

Generated protobuf code is harder to build-tag cleanly. Prefer keeping the
swap subserver protobuf and generated client/server stubs compiled in all
builds, while making service registration build-tagged. That avoids
generated-code split-brain while still keeping the actual swap executor,
subserver registration, and `sdk/swaps` dependency out of default daemon code.

## Runtime Shape

The executor is a daemon subsystem that wraps `sdk/swaps`; it should not
reimplement pay or receive state transitions.

```text
wavecli / SDK caller
    |
    v
swapclientrpc.SwapClientService
    |
    v
waved swap executor      // build tag: swapruntime
    |
    v
sdk/swaps
    |
    v
sdk/ark -> waverpc local client -> waved Ark runtime
swaprpc -> swapdk-server
```

The executor owns:

- swap store lifecycle (`~/.waved/data/<network>/swaps.db` by default,
  configurable);
- swap-server transport config;
- construction of `sdk/swaps.SwapClient`;
- a per-payment-hash worker registry;
- startup resume for non-terminal sessions;
- status snapshots and progress notification.

It does not own Ark wallet logic, OOR construction, vHTLC policy logic, or
swap FSM transitions. Those remain in `sdk/ark`, `oor`, policy packages, and
`sdk/swaps`.

## Daemon API

The swap RPC interface is the control-plane contract for swaps. It should be a
daemon-hosted subserver registered on the same gRPC listener as
`waverpc.DaemonService`, not new methods bolted onto `DaemonService` itself.
The CLI, WaveWalletDK, and any other external process should all use this subserver.
The executor is an internal implementation detail behind it.

The split keeps `DaemonService` focused on the core Ark wallet/runtime API and
lets swap support remain optional without making the main daemon RPC interface
look like every build supports swaps. It also gives WaveWalletDK a clean service
name and package to bind to later.

The first API can stay small:

- `StartPay(invoice, max_fee_sat, idempotency_key?) -> payment_hash`
- `StartReceive(amount_sat, idempotency_key?) -> payment_hash, invoice`
- `ListSwaps(pending_only) -> []SwapSummary`
- `GetSwap(payment_hash) -> SwapSummary`
- `SubscribeSwaps(payment_hash?) -> stream SwapUpdate` if cheap, otherwise defer
  and let callers poll `GetSwap`.

Suggested proto shape:

```protobuf
syntax = "proto3";

package swapclientrpc;

service SwapClientService {
    rpc StartPay(StartPayRequest) returns (StartPayResponse);
    rpc StartReceive(StartReceiveRequest) returns (StartReceiveResponse);
    rpc ListSwaps(ListSwapsRequest) returns (ListSwapsResponse);
    rpc GetSwap(GetSwapRequest) returns (GetSwapResponse);
    rpc SubscribeSwaps(SubscribeSwapsRequest)
        returns (stream SubscribeSwapsResponse);
}
```

The request/response types should be daemon-owned protobufs, not imported
`swaprpc` server messages. The daemon may translate internally to `sdk/swaps`,
but callers should see one stable client-daemon contract.

Possible location:

```text
rpc/swapclientrpc/swap_client.proto
```

Putting the proto under `rpc/swapclientrpc` gives it a real Go package named
`swapclientrpc` while keeping `waverpc.DaemonService` unchanged. The important
boundary is separate service, same daemon listener.

Minimal message shape:

- `StartPayRequest`: BOLT11 invoice, max routing fee, optional caller
  idempotency key, optional wait policy only if we want server-side wait later.
- `StartPayResponse`: payment hash and initial `SwapSummary`.
- `StartReceiveRequest`: amount, optional caller idempotency key.
- `StartReceiveResponse`: payment hash, BOLT11 invoice, and initial
  `SwapSummary`.
- `ListSwapsRequest`: pending-only flag, optional direction filter.
- `ListSwapsResponse`: repeated `SwapSummary`.
- `GetSwapRequest`: payment hash.
- `GetSwapResponse`: one `SwapSummary`.
- `SubscribeSwapsRequest`: optional payment hash and optional direction filter.
- `SubscribeSwapsResponse`: `SwapSummary` plus update reason if useful.

`StartReceive` should return after the invoice and receive session are
durably persisted. The daemon worker then waits for the swap server to fund
the vHTLC and claims in the background.

`StartPay` should return after the pay session has a durable payment hash
and enough state to resume. The worker then drives funding, claim observation,
timeout refund, and terminalization in the background.

The public status model should mirror `sdk/swaps.SwapSummary` rather than
inventing a second vocabulary. It must preserve:

- direction: pay or receive;
- payment hash;
- current state;
- amount, fee, max fee;
- deadline / expiry;
- funding, claim, and refund OOR session IDs when known;
- intervention / failure reason when terminal and non-happy.

Represent directions and states as daemon protobuf enums. The RPC layer should
translate the SDK's durable FSM state strings at the boundary so callers get a
stable control-plane vocabulary while the swap store can continue to persist
SDK-native state names. Later enum changes must reserve removed field numbers,
following the rest of the daemon RPC compatibility rules.

The non-`swapruntime` daemon should not register the swap subserver at all.
Clients that call the service on a default daemon will receive the normal gRPC
unknown-service / unimplemented response. Tagged CLI builds should map that
case to a clear user-facing message such as "daemon was built without
swapruntime support".

The repo also provides convenience targets for the tagged path:

```shell
make build-swapruntime
make build-swapclient   # alias
make install-swapruntime
make unit-swapruntime
```

These targets are thin wrappers around `tags="swapruntime"` (`unit-swapruntime`
also adds `wavewalletrpc`) so local developer and integration-test environments
do not need to remember the exact build tag.

Implementation registration should look like the existing `DaemonService`
registration path in `waved/server.go`, but through a programmatic registrar
so the tag split stays contained and `waved` does not import `sdk/swaps`:

```go
waverpc.RegisterDaemonServiceServer(s.grpcServer, s.rpcServer)
for _, registrar := range s.cfg.RPCServiceRegistrars {
    cleanup, err := registrar(ctx, s.grpcServer, s.rpcServer, s.cfg)
    // ...
}
```

Only the tagged `cmd/waved` file appends the `swapclientserver.Register`
registrar. Non-tag builds do not import the swap runtime package from daemon
startup code and do not advertise swap support on the gRPC server.

Inside that registrar, the swap executor should consume `sdk/ark` through a
private in-process `bufconn` transport over the daemon's existing
`DaemonService` implementation. That keeps the future deployment shape honest:
the swap client can later be split into a separate process that dials Ark over
network gRPC, while the single-binary daemon path uses the same generated RPC
contract without dialing its own public listener. The swap subserver should not
implement a partial local `DaemonServiceClient` for the handful of Ark methods
the swap FSM happens to call today.

## Coordination With Sputnik's PRs

This work intentionally does not duplicate the receive-event or server
protocol changes in:

- `lightninglabs/wavelength#337`, which owns the SDK receive-side mailbox
  HTLC event flow, onion validation, `RequestChannelID` signature change,
  receive auth key plumbing, and swap-store schema changes.
- `lightninglabs/swapdk-server#24`, which owns the swapd-hosted mailbox edge,
  payment-hash-bound virtual route registration, and server publication of
  funded out-swap HTLC notifications.

The daemon-owned executor should treat those as lower-layer dependencies. This
PR should remain a control-plane/background-execution layer:

- no changes to `swaprpc/swap.proto`;
- no custom mailbox or onion validation implementation;
- no server-side route binding changes;
- no WaveWalletDK API work.

With `wavelength#337` merged, `swapclientserver` should treat receive auth
as a lower-layer daemon capability instead of owning a separate key file. The
daemon-side swap client wraps the existing waved RPC implementation with
`sdk/ark` over bufconn, passes that Ark facade to `sdk/swaps` as its
`DaemonConn`, and lets `sdk/swaps` request payment-scoped receive-auth signing
and ECDH through the daemon-backed `ReceiveAuthKey` flow. This keeps the
background executor focused on control-plane work while restart continuity for
receive auth remains anchored in the daemon wallet API introduced by #337.

## Worker Model

The executor should maintain a `map[lntypes.Hash]*worker` protected by a
mutex. Starting or resuming a swap first claims the worker slot. A duplicate
start/resume for an active payment hash returns the existing status or wakes
the existing worker; it must not create a second driver.

Startup flow:

1. open the swap store;
2. construct the swap client;
3. list pending pay and receive sessions;
4. register one worker per payment hash;
5. call the existing `ResumePayViaLightning` or `ResumeReceiveViaLightning`
   path inside each worker;
6. publish a status update when the worker exits.

Manual resume becomes a wake-up signal, not a separate execution path. This is
the Loop-like behavior we want: the daemon owns progress, while CLI and SDK
callers control or observe it.

## CLI Behavior

> **Superseded (#907).** Every reference to a user-facing `wavecli swap`
> verb in this document — here and in the Testing Plan, Rollout, and Planned
> Commit Stack sections below — was retired: `send`/`recv --offchain` and
> `activity` (plus `activity inspect <id>`) cover them. The `swapruntime`
> daemon runtime this document describes is unchanged and still powers those
> verbs; only the curated CLI/MCP `swap` surface was removed. The generated
> `dev swapclient` surface remains for low-level access. These sections are
> retained as design background.

With `swapruntime`, `wavecli swap` should become a daemon-control surface.
The CLI should not open the swap DB, construct `sdk/swaps.SwapClient`, dial
`swapdk-server`, or drive the FSM directly. All CLI operations go through
`swapclientrpc.SwapClientServiceClient`.

Recommended commands:

- `swap pay --invoice ... [--maxfee ...]`
- `swap receive --amount ...`
- `swap list [--pending]`
- `swap show [payment_hash]`
- `swap watch [--pending] [--include-existing]`

Exiting the CLI must not stop the swap; `pay` and `receive` return as soon as
the daemon has durably persisted the session.

`SubscribeSwaps` landed in the first slice, so `swap watch` streams live
updates directly instead of falling back to polling `GetSwap`.

Swap storage and swap-server settings belong in `waved` config. That keeps
daemon startup as the single place where background worker dependencies are
configured, and keeps the CLI focused on the daemon RPC contract.

## Config

The tagged daemon config should include:

- implicit enable when the tag is built and config is set, rather than a
  separate `swap.enable` field;
- `swap.serveraddress`;
- `swap.servertlscertpath`;
- `swap.serverinsecure` for regtest/dev;
- `swap.databasefilename`;
- receive auth key policy.

Receive auth material is the one real design question. The current CLI invoice
generator is process-local, which is not suitable once the daemon owns receive
swaps across restart. The executor needs a stable receive key source:

- best long-term: derive from the wallet/keychain so backup and unlock policy
  are shared with the Ark runtime;
- acceptable first slice: daemon-scoped encrypted local key, only if we are
  explicit that it is temporary;
- avoid: unprotected standalone key file.

The mailbox-onion receive work should be the baseline for this path: the
daemon receives durable HTLC events, validates the forwarded onion locally,
persists accepted event metadata, and only then claims the vHTLC.

## Idempotency

The executor should expose optional caller idempotency keys at the daemon API.
Internally:

- pay funding OOR sends use stable keys derived from the payment hash;
- receive claim OOR sends use stable keys derived from the payment hash and
  claim role;
- pay refund OOR sends use stable keys derived from the payment hash and refund
  role.

This keeps retries and daemon restarts from creating duplicate OOR sends even
when coin selection would otherwise choose different inputs.

Empty caller keys can be allowed for CLI parity, but WaveWalletDK should pass
stable keys once its surface exists.

## Testing Plan

Unit tests under `swapruntime`:

- executor startup resumes pending pay and receive rows;
- duplicate active payment hash does not start a second worker;
- terminal rows are skipped on startup;
- swap subserver methods translate requests into executor calls and summaries
  back into protobufs;
- tagged CLI commands call a fake `SwapClientServiceClient` and never construct
  `sdk/swaps.SwapClient`;
- non-tag builds compile without registering the swap subserver and without
  importing `sdk/swaps` from `waved` or `wavecli`.

Integration tests under `swapruntime`:

> _Superseded (#907): the `wavecli swap` CLI verbs below were removed; the
> daemon runtime they exercised is unchanged. See the note under "CLI
> Behavior"._

- `swap pay` returns, CLI exits, daemon completes the swap;
- `swap receive` returns invoice, CLI exits, daemon claims after payment;
- daemon restart with pending pay resumes without duplicate OOR funding;
- daemon restart with pending receive resumes from mailbox/funding metadata;
- timeout/refund path completes without manual `swap resume`;
- `swap list/show/watch` report terminal failure/intervention context.

Build checks:

```shell
go test ./cmd/wavecli/waveclicommands ./cmd/waved ./waved ./sdk/swaps
go test -tags=swapruntime ./cmd/wavecli/waveclicommands ./cmd/waved ./waved ./sdk/swaps ./swapclientserver
```

## Rollout

1. Land the build-tag scaffolding and no-op default stubs.
2. Add separate swap subserver protobuf messages and generated stubs.
3. Add daemon config and executor startup behind `swapruntime`.
4. Wire tagged subserver handlers to the executor.
5. Convert CLI swap commands to daemon-control mode behind `swapruntime`.
   _(Superseded — #907 removed the `wavecli swap` CLI surface entirely.)_
6. Add restart and timeout integration coverage through the CLI/RPC surface.
7. Document the WaveWalletDK handoff surface once the daemon API stops moving.

This lets us keep the default binaries conservative while giving swap-enabled
builds the runtime model we actually want.

## Planned Commit Stack

The implementation should land as a small stack where each commit either
compiles on its own or is a generated-code companion to the previous commit.

1. `swapclientrpc: add optional swap subserver proto`

   Add a separate swap client proto and generated gRPC bindings. The service
   should not be added to `DaemonService`; it should generate its own
   `SwapClientServiceClient` and `SwapClientServiceServer`. Message types
   include start pay, start receive, list, get, subscribe, `SwapSummary`, and
   a direction enum. State remains a string in this slice so it can mirror the
   SDK without inventing a second enum too early. No `waved` registration
   yet.

2. `waved: add swapruntime registration seam`

   Add the build-tagged registration hook. Default builds register only the
   existing daemon service. `swapruntime` builds register
   `SwapClientService` on the same gRPC server. This commit should prove the
   optional-subserver mechanics without implementing swap execution. If a
   default-build CLI stub is kept, it also lands here.

3. `waved: add tagged swap executor skeleton`

   Add `swapruntime` config, store opening/closing, swap-server dial config,
   executor lifecycle, and startup/shutdown integration. The executor can
   initially list pending sessions and maintain the per-payment-hash worker
   registry, but it does not need to drive all swap paths yet. Default builds
   must not import `sdk/swaps`.

4. `waved: resume pending swaps in background`

   Wire the executor to `sdk/swaps.ResumePayViaLightning` and
   `ResumeReceiveViaLightning`, register one worker per pending session on
   startup, skip terminal rows, and make manual resume a worker wake-up rather
   than a second driver. Add focused unit tests around dedupe and terminal
   skip behavior.

5. `waved: implement swap subserver start and status RPCs`

   Implement `StartPay`, `StartReceive`, `ListSwaps`, and `GetSwap` against
   the executor. Add conversion helpers between `sdk/swaps` summaries and
   protobuf summaries. Start calls return after the initial session is durably
   persisted and worker ownership is established.

6. `waved: stream swap updates`

   Implement `SubscribeSwaps` if the update path is simple enough. If it is
   not, leave the RPC unimplemented in tagged builds and document CLI polling
   as the first slice. This commit should not block the start/list/get
   control plane.

7. `wavecli: move swap commands to daemon subserver`
   _(Superseded — #907 removed the `wavecli swap` CLI surface instead.)_

   Under `swapruntime`, convert `swap pay`, `swap receive`, `swap list`,
   `swap show`, and `swap watch` to call `SwapClientServiceClient`. Remove
   direct CLI construction of `sdk/swaps.SwapClient`, swap DB opening, and
   direct `swapdk-server` dialing from the intended tagged path.

8. `itest: cover daemon-owned swap execution`

   Add integration coverage through the CLI/RPC surface: pay returns while the
   daemon completes, receive returns an invoice while the daemon later claims,
   daemon restart resumes pending pay/receive, and timeout/refund paths finish
   without manual `swap resume`.

9. `docs: capture WaveWalletDK swap handoff`

   Once the RPC shape has survived the first implementation pass, document the
   stable WaveWalletDK handoff: service name, start/list/get/subscribe semantics,
   state meanings, idempotency expectations, and terminal failure fields.
