# SDK Layered Architecture

This note describes the client-side layering direction in
`wavelength`.

The shape is:

- `waved` remains the canonical Ark client runtime.
- `sdk/ark` is the consumer-facing Go SDK for that runtime.
- the same `sdk/ark` API can talk to a remote daemon over gRPC or
  an embedded in-process daemon over `bufconn` or another injected
  listener.
- future higher-level libraries such as `sdk/swaps` build on top of
  `sdk/ark` rather than reaching directly into daemon internals.

The goal is to support both process separation and process
embedding from a single codebase, without ending up with two
different Ark client implementations that must be kept in sync.

## Motivation

Two deployment modes matter to us: a standalone Ark client daemon,
and an application that embeds the Ark client in the same binary.
Higher-level libraries, starting with the swap client, need to work
in both modes without caring how the Ark runtime is hosted.

That combination has historically tempted projects into building
two parallel implementations — a "real daemon" path and a
"just-the-good-bits" in-process path. Those always drift. Our
version avoids the drift by keeping exactly one runtime and
changing only the hosting environment around it.

Concretely, that gives us three design constraints:

1. one canonical Ark runtime (the daemon),
2. a consumer-facing SDK that depends only on the daemon's public
   RPC surface, not its internals, and
3. embedding as a hosting decision, not a separate behavior path.

## Layering

The intended stack:

```text
Applications / CLIs / services / mobile glue
    |
    v
sdk/swaps          future high-level swap orchestration
    |
    v
sdk/ark            consumer-facing Ark Go SDK
    |
    v
waverpc          stable transport contract
    |
    v
waved            canonical Ark client runtime
    |
    v
wallet, indexer, round, oor, ledger, db, mailbox transport
```

The important boundary is between `sdk/ark` and `waved`. `waved`
owns the real runtime: persistence, actor lifecycle, recovery
rules, wallet wiring, and Ark protocol execution. `sdk/ark` owns
transport selection, lifecycle management for embedders, and Go
ergonomics for callers on top of that runtime. Critically, `sdk/ark`
is not a second Ark engine. It is a facade, and it stays that way.

## Runtime modes

The same `sdk/ark` client API supports two transports.

**Remote mode** connects to an already-running `waved` over gRPC:

```text
app -> sdk/ark -> waverpc over TCP/TLS -> waved
```

This is the right fit when the daemon is managed separately, when
process isolation is desirable, when multiple clients need to talk
to the same daemon, or when non-Go consumers also need access.

**Embedded mode** starts `waved` in-process and talks to it
through an injected listener, typically `bufconn`:

```text
app -> sdk/ark -> waverpc over bufconn -> waved
```

This is the right fit when a single binary should contain the whole
Ark client (desktop, mobile, or service hosts that want simpler
lifecycle management), or when tests want a fast in-process runtime
without real port allocation.

The architectural point is that embedded mode still crosses the
same service boundary. Only the transport changes; the runtime does
not. That is what lets one SDK surface serve both modes and lets
the daemon stay the single source of truth for behavior.

Injected-listener support in `waved` is what makes this honest.
Without it, embedded hosts would have to allocate a real TCP port
and pretend to be a daemon process. With it, we get in-process
startup for SDK hosts, no fake port management, fast end-to-end
tests, and a uniform `waverpc` contract in both modes. That is
why the listener work lands first in the stack — it is the
substrate that makes embedding a first-class mode rather than an
afterthought.

Because embedded mode owns a daemon run goroutine in-process, the
SDK also owns its failure semantics. Startup blocks until the
embedded daemon is accepting RPCs, a `Wait()` method surfaces the
daemon's terminal run error through a blocking channel read without
requiring `Close()`, and
`Close()` cancels the embedded daemon, waits for shutdown with a
bounded timeout, and is safe to call more than once. This gives
higher-level callers a way to supervise embedded mode rather than
discovering failures only when the next RPC breaks.

A daemon subserver that already lives inside `waved` should use the
same boundary without starting a second daemon. In that case, `sdk/ark`
can wrap the already-running `DaemonService` implementation behind a
private `bufconn` transport. The caller gets a normal Ark SDK client,
but the process never dials its public RPC listener and never carries a
partial hand-written daemon client.

## Where `sdk/ark` ends and `waved` begins

`sdk/ark` is the consumer-facing Go API for Ark operations that the
daemon already supports. In the near term that covers:

- daemon status and operator terms
- wallet bootstrap and unlock flows
- balance and VTXO listing
- boarding address allocation
- policy-backed convenience helpers for OOR and indexed lookups that
  higher-level clients such as swaps need
- OOR receive script allocation
- indexer lookups
- round and refresh operations
- OOR send operations
- fee estimation and fee history
- stream access for daemon-driven updates

What stays below the SDK layer is everything that makes the runtime
a runtime: daemon lifecycle, actor orchestration, mailbox transport
implementation, wallet backend internals, durable state management,
Ark protocol execution, retry policy beyond transport-safe client
behavior, swap persistence and swap FSM state, secret-material
storage policy (the SDK passes wallet passwords through to the daemon
but does not persist them), and
cross-cutting instrumentation ownership for daemon internals.
Keeping those in `waved` is the thing that prevents drift between
the "real daemon" path and the "embedded SDK" path — there is only
one place where runtime behavior is defined.

Two boundary choices are worth calling out explicitly because they
surface in the first SDK slice.

**API model policy.** Some `sdk/ark` methods currently return
`waverpc` protobuf request and response types directly rather
than SDK-owned models. This is deliberate for the first slice: the
immediate goal is to establish one stable consumer entry point and
one stable transport story, not to invent domain types for every
RPC upfront. The tradeoff is that proto-typed passthrough methods
are pre-1.0 and may change as richer SDK-owned models are
introduced. Higher-level packages such as `sdk/swaps` should treat
those passthroughs as a temporary compatibility layer, not a
long-term domain model guarantee. The long-term direction is to
move high-traffic APIs toward SDK-owned typed models, but we are
not pretending all methods are there yet.

Because of that, `waverpc` changes carry SDK consequences today.
The working rule is that `waverpc` changes should be additive
whenever possible, that removed protobuf fields must reserve their
tags and names, and that generated-proto churn should be treated as
SDK-surface churn for as long as passthrough methods still exist.
This is another reason the long-term direction points toward
SDK-owned typed models.

**Embedded config boundary.** `EmbeddedConfig` currently accepts a
full `*waved.Config`. That is an explicit choice for the first
embedding slice: `sdk/ark` hides transport and lifecycle
management, but it does not yet hide the full daemon configuration
surface. A narrower SDK-owned embedded config may make sense later,
but we do not want to invent one before we know which daemon knobs
higher-level callers actually need.

## Caller contract

`sdk/ark.Client` is safe for concurrent use. Higher-level
orchestration such as swaps will mix unary RPCs, streaming
subscriptions, and shutdown coordination without serializing
through a single goroutine, so the client must support that
natively.

Client readiness is staged rather than binary. A running daemon
walks through several checkpoints — listener open, gRPC accepting
RPCs, wallet unlocked or initialized, round actor initialized,
mailbox transport connected, operator terms cached — and callers
need a way to observe them. Today `GetInfo` is the caller-facing
readiness surface: `Info.WalletReady()` returning `true` means
wallet-dependent RPCs are expected to work, `ServerConnected ==
true` is the current best-effort signal that mailbox ingress is
running, and `ServerInfo != nil` means operator terms have been
fetched and cached. Round-oriented callers should wait for all
three before issuing round-sensitive operations. `ServerInfo` is
intentionally nullable because operator
bootstrap happens after the daemon process starts accepting RPCs, and
the current daemon refreshes that snapshot only during bootstrap, so
it remains the latest known terms for the current session until
refresh wiring lands.

The SDK is not unary-only. The daemon already exposes streaming
state such as `WatchRounds`, and future higher-level callers will
likely need more event-driven hooks for round state changes,
mailbox connect and disconnect transitions, incoming VTXO
observations, and operator-policy refresh events. The direction is
to expose and use streams where the daemon already has them, and to
prefer subscription-driven orchestration over polling in
higher-level flows such as swaps. That keeps `sdk/swaps` aligned
with the daemon's real event model.

The error model is intentionally modest for now. The SDK wraps RPC
failures with contextual `fmt.Errorf(...: %w)` messages and
preserves the underlying gRPC status codes, so callers may branch
on status codes today. Daemon-specific semantic categories will be
introduced as exported SDK sentinels only once the daemon surfaces
them consistently, and richer SDK-owned error categories are
expected to land incrementally as higher-level consumers such as
`sdk/swaps` need them. We do not want to freeze a misleading error
taxonomy before the daemon surface is ready for it.

## Future `sdk/swaps`

The future swap client will sit on top of `sdk/ark`, not beside it:

```text
app
  -> sdk/swaps
      -> sdk/ark
          -> waved
```

That means swap logic can assume one Ark client facade, swap code
does not need to know whether the Ark client is remote or embedded,
swap persistence and FSM logic stay in the swap layer, and Ark-
specific wallet, OOR, indexer, and round operations are delegated
downward to `sdk/ark`.

This matters most for restart safety and correctness. The swap
layer orchestrates high-level workflow, while the Ark client
runtime remains the single source of truth for Ark-side execution
and durability.

A future umbrella SDK — for example, a higher-level wallet package
that bundles Ark and swaps — may still make sense if it becomes
useful. The intended dependency direction is
`umbrella SDK -> sdk/swaps -> sdk/ark`, not `umbrella SDK -> daemon
internals directly`. That keeps the layering honest and preserves
the daemon boundary as the runtime contract.

## Implementation sequence

The stack lands in four incremental slices:

1. make `waved` embeddable by accepting injected RPC listeners,
2. expose the daemon metadata needed by SDK callers, such as
   operator terms on `GetInfo`,
3. build the first `sdk/ark` facade on top of that boundary,
4. move higher-level clients, starting with swaps, onto `sdk/ark`.

This sequence keeps each step reviewable and avoids a single giant
"SDK rewrite" PR.

Correspondingly, the architecture does not try to replace `waved`
with a separate direct-library Ark implementation, bypass the
daemon boundary for main client flows, or fully redesign the daemon
RPC surface before an SDK can exist. Those are explicit non-goals.
The daemon is the runtime; the SDK is the consumer-facing facade
over it.
