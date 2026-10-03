# tools

## Purpose

Build-tooling package: pins Go tool dependencies (`tools.go`) and hosts
`linters/`, which holds two custom golangci-lint plugins: `ll`, an
80-column line limit that is tab- and log-call-aware, and `actorblock`,
a go/analysis analyzer that reports actor turns that can block.

## Key Types

- `linters.LLPlugin` — golangci-lint plugin implementing the `ll`
  linter (`register.LinterPlugin`); reports lines exceeding
  `LLConfig.LineLength` after expanding leading tabs to
  `LLConfig.TabWidth` spaces, skipping `//go:` directives, import
  blocks, and lines matching `LLConfig.LogRegex` (structured log
  calls, which may wrap args across lines).
- `linters.New(settings)` — Plugin constructor golangci-lint calls via
  `.custom-gcl.yml`; fills default line length (80), tab width (8),
  and log regex when unset.

- `actorblock.Analyzer` / `actorblock.NewAnalyzer(cfg)` — Interprocedural
  analyzer for actor `Receive` turns (`linters/actorblock`), wrapped for
  golangci-lint by `linters.ActorBlockPlugin` and runnable on its own
  through `linters/cmd/actorblock`. Entry points are `Receive` methods
  whose message implements `actor.Message` (optionally with a trailing
  `actor.Exec` parameter) and functions passed to
  `actor.NewFunctionBehavior`. Extra entry methods are configurable
  (`-entry-methods` or the plugin's `entry-methods` setting, as
  `Method@pkgpath.Interface`) and default to `ProcessEvent` on protofsm
  `State` implementers and `Dispatch` on `ActorOutboxEvent` implementers,
  because state handlers run inside the driving actor but are only reached
  through interface dispatch. The interface is matched by method names and
  parameter counts, so generic interfaces need no instantiation. A
  `MayBlock` fact on every function that
  reaches a blocking site carries the call graph across packages.
  Findings:
  (a) a reachable `Future.Await`; (b) a reachable `Tell`/`Ask` whose context
  is clearly not derived from the turn context (`context.Background()`,
  `context.TODO()`, a struct field, or a variable assigned only from those,
  optionally wrapped in `context.With*`).
- `actorblock.Baseline` — Checked-in list
  (`linters/actorblock_baseline.txt`,
  `<pkgpath.Func> await=<n> send=<n> # <reason>`) of functions that still
  hold legacy sites. A site is keyed by the function that directly contains
  the call, with the number of direct sites per kind. A function holding
  more sites of a kind than its entry allows is reported, so a second
  `Await` added to a baselined function is not silent. The remaining limit:
  a new `Receive` branch that calls an existing baselined helper adds no
  site to the helper and is not flagged.

## Running actorblock

- `make actorblock-check` runs the analyzer tests, then the standalone
  binary over `./... ./baselib/...`. It fails on any finding outside the
  baseline and on any baseline entry no finding depends on any more, so the
  baseline can only shrink: a count that is higher than the tree holds is
  stale too, and must be lowered. The whole-program run peaks at about
  2.3 GB of RSS. `make lint`, `make lint-local` and
  `make lint-changed-local` run it, and the same analyzer also runs inside
  `custom-gcl` (without the stale check, which needs the whole program).
- `go vet -vettool=$(pwd)/tools/actorblock -actorblock.baseline=<file> ./pkg`
  works after `make actorblock-build`. The `-actor-pkg` flag names the
  actor package, so another repository can reuse the analyzer.
- `tools/actorblock -write-baseline ./...` prints a baseline for the current
  tree. Use it to adopt the analyzer, not to admit new sites.

### Fixing a finding

1. The reply is needed to continue the work: use `actor.AskThen`, which
   delivers the reply to the actor's own mailbox as a message.
2. The reply is only forwarded to the caller: use `actor.DetachAskPromise`.
3. A send with a non-turn context: pass the turn's `ctx` (or
   `context.WithTimeout(ctx, ...)` of it), or use `TryTell` if dropping on a
   full mailbox is acceptable.
4. The wait is safe (a leaf actor that never calls back, a self-owned FSM):
   put `//actor:allow-await <reason>` or `//actor:allow-send <reason>` on the
   call line or the line above. The reason is mandatory, and a directive
   without one is itself a finding. A directive on the line of an `Await`
   inside a shared helper silences every caller. A directive above a call to
   a shared helper exempts only that caller's path, and other callers of the
   helper are still reported.

### What it does not see

- Interface dispatch is not followed: a call through an interface (a
  dependency or an actor ref held as an interface) has no single target.
  An `Await` hidden behind such a call is invisible.
- Function values, method values, and reflection are not followed.
- `go` statements and function literals passed to `OnComplete`,
  `ThenApply`, `AskThen`, `Go` and `AfterFunc` are treated as off the actor
  goroutine, so an `Await` there is not reported. Any other function literal
  is assumed to run inline.
- A send is judged by its context expression inside one function. A helper
  that sends with its own `ctx` parameter is not flagged even when a caller
  passes it `context.Background()`, and a context returned by an arbitrary
  call is assumed derived.
- Only non-test files are analyzed, and only `Tell` and `Ask` on the actor
  package are treated as sends.

## Relationships

- **Depends on**: `golangci-lint`/`plugin-module-register`,
  `golang.org/x/tools/go/analysis` (analyzer framework),
  `golang.org/x/tools/go/packages` (standalone driver only). `actorblock`
  matches `baselib/actor` by import path and does not import it.
- **Depended on by**: `make lint` / `make lint-changed-local` /
  `make install-custom-gcl` (builds `custom-gcl` per
  `.custom-gcl.yml`, which registers this module as a plugin),
  `make actorblock-check`.

## Invariants

- `tools.go` is guarded by `//go:build tools` and never compiled into
  the main binary; it exists only to pin tool versions in `go.mod`.
- The `ll` linter's log-line skip relies on `LogRegex` matching the
  start of a structured log call; changing log helper naming
  conventions requires updating `defaultLogRegex` here too.

- `actorblock_baseline.txt` is sorted and every line carries a reason.
  Never add a line for a new site, fix the site or exempt it with a
  reasoned directive.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
- [baselib/actor/CLAUDE.md](../baselib/actor/CLAUDE.md) — Turn rules the
  analyzer enforces
