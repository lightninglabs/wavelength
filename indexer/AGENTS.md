# indexer

## Purpose

Wallet-scoped VTXO, round, and OOR event query service for connected clients.
Each client is authenticated as a `Principal` and can only query events
relevant to their wallet. Dispatched via the mailbox RPC pipeline like other
services.

## Key Concepts

Use `go doc indexer.<Symbol>` for signatures.

- **`Operator`** — RPC dispatcher factory; `RegisterService` adds services
  (e.g., ArkService) on its `ServeMux`; `ServiceDispatchers` builds
  `DispatcherMap` entries. Exposes `PublishVTXOEvent` and
  `PublishOORRecipientEvent` for `rounds` and `oor` to fan out
  `IncomingVTXOEvent` / `IncomingOOREvent` to registered receive-script
  holders. Registers 8 RPC handlers: `RegisterReceiveScript`,
  `UnregisterReceiveScript`, `GetReceiveScriptStatus`,
  `ListOORRecipientEventsByScript`, `ListVTXOsByScripts`,
  `GetOORSessionByTxid`, `GetSubtreeByScripts`, `ListVTXOEventsByScripts`.
- **`Service`** — Query implementation. `SetVTXOProofPolicy(operatorKey,
  exitDelay)` enables owner-pubkey proof verification and standardized-Ark
  receive-script classification at registration time.
- **`Principal`** — Authenticated client context (mailbox ID, wallet scope).
- **Lineage resolution** — `LineageResolver` interface +
  `lineageResolver` concrete (handles round-backed and virtual OOR VTXOs,
  with per-outpoint caching; wrapped in `ExecReadTx` for atomic multi-query
  reads). `EstimateOORLineageVBytes` (in `lineage_vbytes.go`) is the
  public entrypoint consumed by `oor/`'s submit cap check — walks every
  input's ancestry, de-duplicates by txid, returns cumulative
  witness-discounted vbytes for unilateral exit.
- **Multi-tree ancestry** — `vtxoLineage.ancestryPaths` is a slice of
  `ancestryFragment`. Round-direct + same-commitment OOR surface length 1;
  cross-commitment multi-input OOR surfaces one entry per distinct
  commitment tx. `combineVirtualLineage` groups by `commitmentTxID` and
  runs `tryResolveCombinedRoundPath` per group. The legacy
  `mixedSingularLineage` graceful-degrade is gone — the resolver
  hard-errors when no fragment carries a tree path.
  `applyLineageMetadata` writes `arkrpc.VTXO.AncestryPaths` (one proto
  entry per fragment); retired scalar `tree_path` / `tree_depth` fields
  are not written. Wire shape in `client/arkrpc/indexer.proto`.
- **Shared ancestry driver** — `walkOORSessionAncestryDriver` in
  `ancestry_walk.go` is the recursion driver used by both the
  lineage-vbytes cap and the recipient-events path so depth-bound, cycle
  protection (`chainhash.Hash`-keyed seen-set), and visit ordering stay
  consistent. Visitor callbacks: `AncestryPreVisitor` (returns parents to
  recurse into) + `AncestryPostVisitor`. Depth beyond
  `DefaultMaxLineageDepth` returns a typed error.
- **`VTXOEventMetadata`** (`ValueSat`, `RoundID`, `BatchExpiryHeight`,
  `RelativeExpiry`, `Origin`, `CommitmentTxid`) is persisted alongside
  VTXO lifecycle events so poll and push payloads stay symmetric.
- **Standard receive-script classification** — `matchesStandardVTXOReceiveScript`
  (in `proof.go`) reports whether a registered pkScript matches the
  operator's current standardized Ark VTXO policy for a given owner
  pubkey; only then are `(owner, operator, exit_delay)` metadata
  persisted. Non-matches are generic scripts (not errors); metadata
  columns round-trip as nil.
- **OOR session by txid** — `GetOORSessionByTxid` returns the Ark package
  and finalized checkpoints for a session identified by its deterministic
  txid, gated by proof of a consumed script.
- **Settlement-pair-restricted policy auth** —
  `participantKeysFromRow` / `authorizePolicySignerByRows`
  (`policy_auth.go`) derive non-operator participant keys from a row's
  persisted `PolicyTemplate`, restricted to keys appearing in a valid
  settlement pair with the operator (unilateral-auth leaf + operator-
  backed forfeit sibling). Stalker keys in custom policy templates can't
  poison read access. `authorizeScriptScopeQuery` (`query_auth.go`) is
  the **single canonical entrypoint** — runs proof verification and row-
  based policy auth together so handlers can't skip a step.
- **Keyset pagination** — `ListVTXOsByPkScriptsAfter` is keyset-paginated;
  cursor is `(outpoint_hash, outpoint_index)` bytes
  (`encodeVTXOCursor`/`decodeVTXOCursor`), status filter pushed into SQL.
  Concurrent inserts can't cause skip/dup across pages (unlike retired
  offset-cursor).
- **TLV proof OOM bound** — `maxProofMessageSize = 4096`.
  `decodeProofMessage` rejects oversized blobs before TLV decoding starts
  and uses `tlvStream.DecodeP2P` (bounded reader) rather than `Decode`
  (unbounded) so a crafted proof can't drive an OOM allocation.
- **Correlation key opt-out** — `indexerEventMessage.CorrelationKey()`
  returns the empty string; push notifications participate only in global
  available-at order (no per-key FIFO).
- **`RoundRow.OperatorPubKey`** — Compressed operator pubkey committed to
  VTXOs in this round. Replaces the retired `SweepKey` field on
  `RoundRow`.
- **`BuildScriptScopeProofMessageWithSigner`** (`proof.go`) — Exported
  helper that builds and TLV-encodes a script-scope proof bound to one
  explicit signer; used by the test harness without a full client daemon.

## Relationships

- **Depends on**: `clientconn`, `db` (wallet-scoped queries, `ExecReadTx`,
  event persistence), `rounds`, `batch`.
- **Depended on by**: root `darepo`, `oor` (`RecipientNotifier`), `rounds`
  (via the `rounds.VTXOEventPublisher` adapter in `server_rounds.go`).
- **Messages**:
  - Query requests ↔ `clientconn`.
  - `PublishVTXOEvent` ← `rounds` (confirmed round leaves) and
    `server_indexer.go` (legacy path, zero metadata).
  - `PublishOORRecipientEvent` ← `oor` after finalization.
  - `IncomingVTXOEvent` / `IncomingOOREvent` → `clientconn` push; also
    persisted to `indexer_vtxo_events` / `indexer_oor_recipient_events`
    for offline-recipient poll reconciliation.

## Invariants

- All queries are scoped to the authenticated Principal's wallet.
- Indexer is read-only WRT round/OOR/VTXO state; it writes only its own
  event feed + receive-script registry.
- Owner-pubkey proof: when a receive-script proof carries an owner pubkey
  (TLV type 10), the server reconstructs the expected tapscript from
  `(ownerKey, operatorKey, exitDelay)` and verifies the pkScript matches.
  Signature verifies against the raw owner key, not the taproot output
  key. When absent, the direct-P2TR path is used.
- `authorizeScriptScopeQuery` is the mandatory entrypoint for all four
  script-scope handlers; calling `verifyQueryScriptScopes` or
  `authorizeRegisteredOrPolicyScripts` in isolation creates a security
  gap.
- `ServiceMethod()` on indexer event messages returns `arkServiceName`
  (not `indexerServiceName`) to match client-side EventRouter routes.
- Lineage resolver must return errors on checkpoint fetch failure (no
  silent skipping); partial lineage data is worse than an error.
- Tree path uses the proto `TreePath` representation, not raw TLV bytes.
- Query limits are enforced to prevent unbounded result sets.
- VTXO event metadata persisted at `AddVTXOEvent` time must match the
  transient push payload.
- `BatchExpiryHeight` is absolute (`confirmation_height + sweep_delay`),
  not the relative sweep delay.
- `CommitmentTxid` is the signed commitment tx hash, not a leaf txid.
- `Origin` is `VTXO_ORIGIN_IN_ROUND` for confirmed round leaves; other
  origins are reserved for future OOR/refresh flows.
- Script and recipient event reads (`ListOORRecipientEventsByScript`,
  `ListVTXOsByScripts`) accumulate inside `ExecReadTx` so all reads
  composing one response see a consistent snapshot and retry atomically.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide map.
