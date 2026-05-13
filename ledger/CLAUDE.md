# ledger

## Purpose

Client-side durable actor that serializes all accounting writes (fees, VTXO
receipts/sends, wallet UTXO deposits, exit costs) as double-entry ledger
entries and UTXO audit log records. Provides a crash-safe financial audit
trail for tax reporting and fee transparency. Ledger event types:
`wallet_utxo_created` (wallet UTXO deposit), `boarding_fee_paid`,
`refresh_fee_paid`, `onchain_fee_paid`, `boarding_sweep_fee_paid`,
`vtxo_received`, `vtxo_sent`.

## Chart of Accounts

The client ledger uses seven accounts seeded by migration
`000006_fee_accounting.up.sql`:

- `wallet_balance` (asset) — on-chain wallet funds.
- `vtxo_balance` (asset) — current VTXO holdings.
- `fees_paid` (expense) — Ark protocol fees paid to the operator.
- `onchain_fees` (expense) — L1 chain/miner fees (exit costs, etc).
- `transfers_in` (revenue) — counterparty side of received VTXOs.
- `transfers_out` (expense) — counterparty side of sent VTXOs.
- `opening_balance` (equity) — source of funds for wallet UTXO
  deposits. Counterparty account for every confirmed wallet UTXO
  so `wallet_balance` has a matching inflow leg to balance the
  boarding outflow. Without this account `wallet_balance` would
  drift negative on every boarding.

`transfers_in` and `transfers_out` are kept as separate accounts so gross
send and gross receive flows are visible independently instead of netted
on a single account. This matters for tax reporting where gross figures
are typically required.

For the walkthrough of how each client-side flow touches these
accounts, see [docs/fee_ledger.md](../docs/fee_ledger.md).

## Key Types

- `LedgerActor` — Durable actor that processes accounting messages and persists ledger entries. Caches a resolved `clock.Clock` at construction (`a.clk`) so handlers stamp `CreatedAt` via `a.clk.Now()` without re-optioning the field on every message.
- `ActorConfig` — Configuration: logger, delivery store, ledger store, UTXO audit store, actor ID, and optional `Clock` (`fn.Option[clock.Clock]`). When None, the actor falls back to `clock.NewDefaultClock()`; tests inject a deterministic clock.
- `Sink` — Alias for `actor.TellOnlyRef[LedgerMsg]`, constructed via `NewSink(system *actor.ActorSystem)`. Producers (round / OOR / VTXO / wallet) hold an `fn.Option[ledger.Sink]` so they can fire-and-forget emissions without resolving the service key on every event.
- `LedgerStore` — Interface for DB persistence of ledger entries (implemented by `db.LedgerStoreDB`). Has a single `InsertLedgerEntry` method; multi-leg handlers rely on the durable actor's outer tx for atomicity rather than a batch API.
- `LedgerEntry` — Domain-level double-entry record (debit/credit accounts, amount, round ID, session ID, event type, description, created_at, and optional `IdempotencyKey []byte` for outpoint-keyed dedup on events that carry neither a round_id nor a session_id).
- `exitIdempotencyKey(hash, index) []byte` — Internal helper that derives the 36-byte `outpoint_hash || outpoint_index` dedup key `handleExitCost` stamps on both the send leg and the fee leg. Keeps outpoint-keyed entries distinct from round-keyed and session-keyed ones via the separate `idx_client_ledger_idempotent_key` partial unique index.
- `UTXOAuditStore` — Interface for DB persistence of UTXO audit log entries (implemented by `db.UTXOAuditStoreDB`).
- `UTXOAuditEntry` — Domain-level UTXO audit record (outpoint, amount, event, block height, classification).
- `LedgerMsg` / `LedgerResp` — Message and response type constraints for the durable mailbox.
- `FeePaidMsg` — Records fee payments. Three flavors keyed by `FeeType`:
  `FeeTypeBoarding` / `FeeTypeRefresh` book Ark protocol operator fees
  (`fees_paid` debit, `vtxo_balance` credit); `FeeTypeOnchainSweep` books
  an L1 miner fee from the boarding-sweep flow (`onchain_fees` debit,
  `wallet_balance` credit). `FeeTypeOnchainSweep` entries carry an
  `IdempotencyKey` (sweep txid) and store `RoundID = nil`, bypassing
  the round-keyed dedup index and using `idx_client_ledger_idempotent_key`
  instead.
- `VTXOReceivedMsg` — Records incoming VTXOs. `Source` must be one of `SourceRoundBoarding` (client's own on-chain wallet funds boarded into a round; offsets wallet_balance), `SourceRoundRefresh` (refresh output or directed-send self-change; offsets transfers_out so the paired VTXOSent cancels on that account and only the operator fee moves vtxo_balance), `SourceRoundTransfer` (in-round receive from another participant; offsets transfers_in), or `SourceOOR` (out-of-round receive; offsets transfers_in). Any other value is rejected.
- `VTXOSentMsg` — Records outgoing VTXO transfers. Carries either `SessionID` (32-byte OOR) or `RoundID` (16-byte in-round) — exactly one must be non-zero; both-zero and both-set inputs are rejected. Also carries an optional `Outpoint wire.OutPoint` so in-round multi-VTXO events (e.g. paired refresh emissions) disambiguate per-VTXO via an outpoint-derived `IdempotencyKey` instead of collapsing on `idx_client_ledger_idempotent_round`.
- `ExitCostMsg` — Records a unilateral exit as two ledger entries: a send leg (`transfers_out` debit, `vtxo_balance` credit) for the net-of-fee value and a fee leg (`onchain_fees` debit, `vtxo_balance` credit) for the miner fee. Together the credits reduce `vtxo_balance` by the gross exited amount. Wallet-side movement is covered separately by the `wallet_utxo_log` audit trail.
- `UTXOCreatedMsg` — Records new wallet UTXO confirmations with classification. `handleUTXOCreated` writes TWO rows per event: a `wallet_utxo_log` audit row and a double-entry ledger row (`debit wallet_balance, credit opening_balance`, `event_type = wallet_utxo_created`) stamped with an outpoint-derived idempotency key via `walletUTXOIdempotencyKey`. The deposit leg is what gives `wallet_balance` a non-negative balance in the presence of `SourceRoundBoarding` outflows.
- `UTXOSpentMsg` — Records wallet UTXO spends with classification. Currently only writes the `wallet_utxo_log` audit row (double-entry leg for non-boarding wallet spends is a planned follow-up).

## Relationships

- **Depends on**: `baselib/actor` (durable actor framework, TLV codec, service keys), `lnd/clock` (injectable time source).
- **Depended on by**: `db` (provides `LedgerStoreDB` and `UTXOAuditStoreDB`), `darepod` (wires actor at startup and exposes `LedgerStoreDB` to the RPC layer), `round` / `oor` / `vtxo` / `wallet` (hold `fn.Option[ledger.Sink]` on their configs and Tell emissions on hot-path transitions).
- **Receives** (via `Sink` Tell):
  - ← `round` (origin-routed on `VTXOCreatedNotification`):
    `VTXOReceivedMsg{Source=SourceRoundBoarding}` for boarding-origin VTXOs;
    paired `VTXOSentMsg{Outpoint}` + `VTXOReceivedMsg{Source=SourceRoundRefresh}`
    for refresh-origin VTXOs (legs cancel on transfers_out);
    `VTXOReceivedMsg{Source=SourceRoundTransfer}` for in-round participant transfers;
    one `FeePaidMsg{FeeType=FeeTypeRefresh}` per round when `VTXOCreatedNotification.OperatorFeeSat > 0` and any refresh-origin VTXO was present (boarding-fee emission deferred to a follow-up).
  - ← `oor`: `VTXOSentMsg` after FinalizeAcceptedEvent; `VTXOReceivedMsg` (`Source=SourceOOR`) per descriptor in `notifyMaterializedVTXOs`.
  - ← `vtxo`: `ExitCostMsg` after chain resolver determines miner fee (currently a no-op emission with a TODO — chain resolver wiring pending).
  - ← `wallet`: `UTXOCreatedMsg` on confirmed wallet UTXO observation;
    `FeePaidMsg{FeeType=FeeTypeOnchainSweep, IdempotencyKey=sweepTxid}` on
    boarding-sweep confirmation (emitted by `emitSweepConfirmedLedger`).
    `UTXOSpentMsg` emission is pending.

## Caller Contract

Handlers book ledger entries exactly as written — there is no hidden
fee netting or balance reconciliation. Callers must emit the right
pairs of messages:

- **Wallet UTXO confirmed (deposit):** `UTXOCreatedMsg` with the
  UTXO amount and classification. The handler writes BOTH the
  `wallet_utxo_log` audit row AND a ledger row
  (`debit wallet_balance, credit opening_balance`,
  `event_type = wallet_utxo_created`). This is what makes
  `wallet_balance` a proper asset account with a source-of-funds
  leg; without it every boarding would drift `wallet_balance`
  negative.
- **Boarding (round flow, client's wallet input → VTXO):**
  `VTXOReceivedMsg` with `Source = SourceRoundBoarding` and
  `AmountSat` net. The leg books
  `debit vtxo_balance, credit wallet_balance`. Boarding-fee
  emission (`FeePaidMsg`) is deferred to a follow-up PR; for
  now the operator fee is absorbed silently on this flow.
- **Refresh / directed-send self-change (round flow, client's VTXO → new VTXO):**
  emit BOTH `VTXOSentMsg{Outpoint, RoundID, AmountSat=gross}` AND
  `VTXOReceivedMsg{Source=SourceRoundRefresh, RoundID, AmountSat=gross}`.
  The two legs share the gross forfeited amount and cancel on
  `transfers_out`; the real net change on `vtxo_balance` comes
  from the paired `FeePaidMsg{FeeType=FeeTypeRefresh}` that the
  round actor also emits when `OperatorFeeSat > 0`. Crediting
  `wallet_balance` here would be wrong since no wallet UTXO
  funded the new VTXO. The round actor automates this emission;
  wallet-level composition just needs to tag `VTXOOrigin = RoundRefresh`.
- **In-round participant transfers (receive):** `VTXOReceivedMsg` with
  `Source = SourceRoundTransfer` and `AmountSat` net; no `FeePaidMsg`.
- **OOR receive:** `VTXOReceivedMsg` with `Source = SourceOOR` and
  `AmountSat` net; no `FeePaidMsg`.
- **OOR send:** `VTXOSentMsg` with `SessionID` non-zero, `AmountSat`
  net. No `FeePaidMsg`.
- **In-round send:** `VTXOSentMsg` with `RoundID` non-zero, `AmountSat`
  net. No `FeePaidMsg`. (`SessionID` and `RoundID` are mutually
  exclusive: the handler rejects messages that set both or neither.)
- **Unilateral exit:** `ExitCostMsg` with `AmountSat` gross and
  `ExitCostSat` fee. The handler expands this into two ledger entries
  internally (send leg + fee leg).
- **Boarding sweep confirmed:** `FeePaidMsg{FeeType=FeeTypeOnchainSweep,
  AmountSat=feeSat, IdempotencyKey=sweepTxid[:]}`  (standalone entry; no
  paired `VTXOReceivedMsg`). Books `onchain_fees += feeSat`,
  `wallet_balance -= feeSat`. The UTXO returned to the wallet appears via
  the normal `UTXOCreatedMsg` path when the sweep output confirms.

## Invariants

- All accounting writes are serialized through a single durable actor instance (no concurrent DB writes).
- Every ledger entry is double-entry: debit and credit accounts must differ.
- Handlers reject non-positive `AmountSat` up front with `ErrInvalidMessage` so a malformed TLV dead-letters cleanly instead of hitting the SQL `CHECK (amount_sat > 0)` constraint and driving infinite nack-and-retry on a permanent condition.
- Unknown fee types and VTXO sources return errors (no silent misclassification). Callers should use the exported `FeeType*`, `Source*`, and `Classification*` constants rather than literal strings so typos are caught at compile time.
- `FeeTypeBoarding` and `FeeTypeRefresh` always debit `fees_paid` / credit `vtxo_balance` and are keyed by `RoundID`. `FeeTypeOnchainSweep` debits `onchain_fees` / credits `wallet_balance`, sets `RoundID = nil`, and requires a non-nil `IdempotencyKey` for dedup. Mixing these routing rules (e.g. sending `FeeTypeOnchainSweep` with a RoundID) would produce wrong entries — the handler enforces the per-type account assignment at runtime.
- Zero-valued RoundIDs / SessionIDs are stored as NULL via `roundIDOrNil` / `sessionIDOrNil` so the DB partial unique indexes (`WHERE round_id IS NOT NULL`, `WHERE session_id IS NOT NULL`) correctly bypass idempotency checks for non-round / non-session events.
- Fire-and-forget pattern: `LedgerResp` is always nil; callers use `Tell`, not `Ask`.
- Messages use TLV stream encoding (variable-length fields) for forward-compatible extensibility. `decodeAmountSat` narrows decoded `uint64` to `int64` to reject values past `math.MaxInt64`; `decodeFixedBytes` enforces exact `RoundID=16` / `SessionID=32` / `OutpointHash=32` byte lengths so a crafted payload cannot smuggle wrong-sized IDs.
- `Start` validates both `DeliveryStore` and `LedgerStore` are non-nil before launching the runtime.
- `UTXOAuditStore` is optional: when nil, UTXO audit messages are logged but not persisted.
- UTXO classification context is provided by the sending subsystem, not inferred from chain data.
- UTXO audit inserts are idempotent on `(outpoint_hash, outpoint_index, event)` via `ON CONFLICT DO NOTHING`, so RestartMessage replay after a crash is a silent no-op rather than a duplicate row.
- **Replay safety (ledger entries):** `InsertClientLedgerEntry` uses `ON CONFLICT DO NOTHING` against every partial unique index (`idx_client_ledger_idempotent_round`, `_session`, `_key`). Redelivered messages resolve to silent no-ops. Crash atomicity for multi-leg events (ExitCost) is guaranteed by the durable actor's outer tx: handlers run inside `TxAwareDeliveryStore.ExecTx`, and `db.TransactionExecutor.ExecTx` joins the outer tx via `actor.TxFromContext` so two `InsertLedgerEntry` calls from one handler commit atomically with the mailbox ack.
- Handler-level errors (both `ErrInvalidMessage` and DB failures) log at `WarnS` — Error-level logging is reserved for internal bugs, and both handler failure classes are externally triggered.

## Deep Docs

- [docs/durable_actor_architecture.md](../docs/durable_actor_architecture.md) — CDC pattern, durable mailbox lifecycle.
- [docs/durable_actor_quickstart.md](../docs/durable_actor_quickstart.md) — TLVMessage, ActorBehavior, migration checklist.
- [db/CLAUDE.md](../db/CLAUDE.md) — DB layer including LedgerStoreDB and UTXOAuditStoreDB adapters.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
