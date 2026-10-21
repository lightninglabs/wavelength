# Durable Handoff and Recovery Model

`DurableHandoff.tla` models one message crossing from a source durable mailbox
to a dynamically named target actor. The first fold is forced to fail, then a
retry either commits the source acknowledgement and target enqueue atomically
or follows a deliberately weakened split-write profile. The target row begins
delayed or leased so exhaustive exploration covers both recovery cases.

The model then permits a process crash, restart, terminal domain transition,
delay expiry, lease expiry, and final consumption. Each event represents a
durability boundary rather than a Go function:

| TLA+ action | Durable actor event |
| --- | --- |
| `Rollback` | The transaction fails; source acknowledgement and handoff both roll back. |
| `AtomicCommit` | Source acknowledgement and target enqueue commit together. |
| `Crash` | In-memory consumers disappear while mailbox rows remain. |
| `Restart` | Pending mailbox IDs reconstruct dynamically named consumers. |
| `EnterTerminal` | Domain state completes while a durable retry still exists. |
| `DelayElapses` / `LeaseExpires` | A persisted row becomes claimable. |
| `Consume` | The restored target actor drains the durable row. |

The production configuration checks three safety invariants and two liveness
properties:

- an acknowledged source always has a durable successor;
- the atomic retry remains enabled after rollback;
- every pending target mailbox has a consumer while the runtime is up;
- the failed fold is eventually retried and committed;
- the target row is eventually consumed.

Four negative configurations use the same transitions with one guarantee
weakened:

- `SplitHandoff.cfg` separates acknowledgement from enqueue;
- `OmitDelayedRecovery.cfg` omits delayed rows from restart discovery;
- `OmitLeasedRecovery.cfg` omits leased rows from restart discovery;
- `ReapTerminalConsumer.cfg` removes the consumer as soon as domain state is
  terminal.

Each negative configuration must produce an invariant violation. This makes
the model check fail if an invariant becomes too weak to detect its intended
failure mode.

Run the complete suite from the repository root:

```shell
./tla-models/scripts/check.sh
```

To invoke TLC directly with an existing tools jar:

```shell
java -cp "$TLA2TOOLS_JAR" tlc2.TLC -cleanup -deadlock -workers 1 \
  -config tla-models/durablehandoff/Production.cfg \
  tla-models/durablehandoff/DurableHandoff.tla
```
