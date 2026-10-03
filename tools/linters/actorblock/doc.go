// Package actorblock implements a go/analysis analyzer that reports actor
// turns that can block.
//
// An actor processes its mailbox one message at a time, so a Receive that
// parks its goroutine stalls every message behind it. Two shapes have caused
// real deadlocks, and the analyzer reports both:
//
//   - A Future.Await on another actor's reply. If the callee is waiting on
//     the caller, neither makes progress.
//   - A Tell or Ask whose context is not the turn's context. Only a send
//     made with the turn context is non-parking on a full mailbox, so one
//     made with, say, context.Background() or a context captured at
//     construction parks.
//
// # Entry points
//
// An entry point is a method named Receive with the shape
// func(context.Context, M) R where M implements the actor Message
// interface (optionally followed by an actor.Exec parameter, as in a
// TxBehavior), or a function passed to actor.NewFunctionBehavior or
// actor.FunctionBehaviorFromSimple. Test files are ignored.
// Extra entry methods can be configured with [Config.EntryMethods], and by
// default cover protofsm state handlers (ProcessEvent) and outbox events
// (Dispatch), which run in the driving actor but are reached only through an
// interface.
//
// # Reachability
//
// Most real Await sites sit in helpers that Receive calls, so the analyzer
// is interprocedural. Each function that can reach a blocking site, through
// statically resolved calls, gets a [MayBlock] fact. Facts flow across
// package boundaries, so a helper in another package is seen as long as that
// package is analyzed too. An entry point is reported at the call that
// starts the path, and the message spells out the path.
//
// The walk follows plain function calls and concrete method calls. It
// deliberately does not follow:
//
//   - Interface dispatch. A call through an interface (a dependency held as
//     an interface, or an actor ref) has no single target, and exploding it
//     to every implementation would report paths that never run.
//   - Function values, method values, and reflection.
//   - A go statement, and a function literal passed to OnComplete, AskThen,
//     ThenApply, Go, or AfterFunc, because those run off the actor
//     goroutine. A function literal passed to anything else is assumed to
//     run inline and is walked as part of the caller.
//
// These are false-negative classes, not suppressions.
//
// # Send contexts
//
// A Tell or Ask is flagged only when its context is clearly not derived
// from the turn. A context is not derived when it is context.Background or
// context.TODO, a struct field, or a local or package variable assigned only
// from such values, optionally wrapped in context.With*. Anything else, such
// as a parameter or the result of an arbitrary call, is assumed derived. The
// check is intraprocedural: a helper that sends with its own context
// parameter is not flagged even if its caller passes a bad one.
//
// # Exemptions
//
// A "//actor:allow-await <reason>" or "//actor:allow-send <reason>" comment
// on the call line or the line above exempts that call. The reason is
// mandatory, and a directive without one is itself reported. A baseline file
// lists functions with legacy sites so the analyzer fails only on new ones,
// see [Baseline].
package actorblock
