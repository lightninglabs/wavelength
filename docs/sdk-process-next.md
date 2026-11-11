# Single-batch Host Intake

`Client.ProcessNext(ctx, id)` exposes a guarded intake step. It requires the
embedded durable runtime, paused foreground ingress and host serialization of
state-creating calls. Its unique invocation ID shares `CancelReceiptWindow`
for early OS expiry. The mobile binding takes that ID and a bounded timeout.

The result preserves `Batches`, `MailboxEmpty` and `NeedsForeground`. On
successful nonempty return, recoverable handoffs and the remote ACK are
committed. A rejected route stays unacknowledged. A canceled or failed call
may already have committed a prefix, which the next invocation resumes from
its checkpoint.

This API does not read receipts, run maintenance, sleep between polls or wait
for domain workers. Existing windows remain the higher-level bounded recovery
APIs. `awaiting_peer` is deliberately absent: neither a handoff nor an empty
pull proves that classification for all downstream owners.
