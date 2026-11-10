# SDK shutdown ownership

A host may have less time to wait than its wallet needs to shut down. The SDK
retains the cleanup owner when that wait expires. A timeout means cleanup is
still pending. Retain the handle and re-wait before opening its database again.

`Client.Stop` and `Client.Close` keep the existing five-second caller wait.
`Client.CloseContext(ctx)` lets a Go host choose that wait. The first call closes
local callback admission, cancels those callbacks, and starts one cleanup owner.
Later calls wait the same result. Caller cancellation never becomes the cached
terminal cleanup error. Cleanup uses a handle-owned context and ends only when
its close function returns. A non-nil final error remains observable on re-wait.

Most calls cross the private gRPC connection, whose server shutdown owns their
join. Embedded readiness, wallet recovery and notification signing callbacks
run directly in process. Their admission and Close share one mutex. An admitted
callback derives from its caller's context and is also cancelled by Close. The
handle joins its final resource access before running daemon cleanup. Cancellation
alone is not completion. Calls arriving after shutdown starts return
`ErrClientClosed` without touching the signer or recovery store.

Startup failures have no published handle to retain. Those paths cancel the
daemon and join its return before releasing the unpublished owner. The external
seed startup path does the same if opening, readiness or recovery fails. A caller
startup deadline still cancels startup work, but does not force a second daemon
to open while the first instance is cleaning up.

The mobile singleton holds its starting/stopping generation until that join
completes. Stop during startup cancels both startup and wallet-operation contexts
and re-waits that startup owner. Stop after startup keeps the client during
cleanup. New Start calls reject throughout that interval. RPC admission is closed
in the stopping state. `IsRunning == false` is not a cleanup completion signal;
wait for successful Stop before reopening. A final cleanup error requires host
handling even when the owner has returned.

This contract proves that the SDK retains and joins its daemon handle. It does
not certify that every daemon actor has durably checkpointed or joined before
Run returns. Bounded background execution and a suspend-safe result require
separate daemon ownership and recovery work. Neither a Close timeout nor an
empty mailbox is a suspend-safe signal.

`TestCloseContextRetainsCleanup` blocks cleanup past a caller cancellation and
checks that a later waiter sees the actual terminal result. The callback suite
blocks final access after cancellation for both notification sources, readiness
and recovery. The mobile test blocks a cancelled startup and proves another
Start stays rejected until the original owner finishes.

See [the implementation plan](sdk-shutdown-ownership-execplan.md) for ownership
boundaries and validation commands.
