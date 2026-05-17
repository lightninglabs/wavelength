# Actor Delivery Store (Removed)

The actor delivery store schema and runtime were removed from production use.

Current persistence rules:

- Mailbox transport durability is stored in `mailbox_ingress_cursors` and
  `mailbox_egress`.
- OOR client state is stored in OOR-owned session, artifact, and effect tables.
- Rounds, unroll, wallet, and ledger own their own SQL durability surfaces.
- Actor messages are not persisted through a generic delivery queue.
- Restart recovery must load explicit subsystem SQL facts, reconstruct the FSM,
  and redrive missing side effects from named state.

This file remains only to make old links fail safely with the current model.
