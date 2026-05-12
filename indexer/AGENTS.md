# indexer

## Purpose

Wallet-scoped VTXO, round, and OOR event query service for connected clients.
Each client is authenticated as a `Principal` and can only query events relevant
to their wallet. Dispatched via the mailbox RPC pipeline like other services.

## Key Types

- `Operator` — RPC dispatcher factory that creates per-request handlers.
  Exposes `RegisterService` to host additional services (e.g., ArkService) on
  its internal `ServeMux`, and `ServiceDispatchers` to build `DispatcherMap`
  entries for any registered service. Also exposes `PublishVTXOEvent` and
  `PublishOORRecipientEvent` used by the rounds and OOR layers to fan out
  `IncomingVTXOEvent`s / `IncomingOOREvent`s to registered receive-script
  holders. Registers **8** RPC handlers: `RegisterReceiveScript`,
  `UnregisterReceiveScript`, `GetReceiveScriptStatus`,
  `ListOORRecipientEventsByScript`, `ListVTXOsByScripts`,
  `GetOORSessionByTxid`, `GetSubtreeByScripts`, `ListVTXOEventsByScripts`.
- `Service` — Query service implementation (list VTXOs, rounds, OOR events,
  VTXO event feeds). Supports `SetVTXOProofPolicy(operatorKey, exitDelay)` for
  owner-pubkey proof verification and for classifying receive scripts as
  standardized Ark VTXO receive scripts at registration time.
- `Principal` — Authenticated client context (mailbox ID, wallet scope).
- `LineageResolver` — Interface for per-request resolvers of authoritative VTXO
  lineage metadata (round ID, commitment tx, batch expiry, tree depth, chain
  depth, tree path). Extracted as an interface for testability.
- `lineageResolver` — Concrete implementation handling both round-backed and
  virtual (OOR) VTXOs with checkpoint chain tracing and per-outpoint caching.
  Wrapped in `ExecReadTx` for atomic multi-query reads.
- `ScriptAuthorizer` — Interface for wallet-scope authorization of receive
  script operations.
- `VTXOEventMetadata` — Optional round metadata (`ValueSat`, `RoundID`,
  `BatchExpiryHeight`, `RelativeExpiry`, `Origin`, `CommitmentTxid`) persisted
  alongside VTXO lifecycle events so poll-path queries return the same payload
  as transient mailbox push notifications.
- `VTXOEvent` — Indexer view of a VTXO lifecycle event row, embedding
  `VTXOEventMetadata` for `VTXO_CREATED` events from confirmed rounds.
- `ReceiveScript` — Indexer view of a receive-script registration row,
  including optional `OwnerPubKey` / `OperatorPubKey` / `ExitDelay` fields
  populated only when a registration validates as a standardized Ark VTXO
  receive script.
- Event types (`IncomingOOREvent`, `IncomingVTXOEvent`) with `ServiceMethod()`
  routing metadata for client-side EventRouter dispatch.
- `ExecReadTx` — Atomic read transaction wrapper for multi-query consistency.
- `matchesStandardVTXOReceiveScript` (helper in `proof.go`) — Reports whether
  a registered pkScript matches the operator's current standardized Ark VTXO
  policy for a given owner pubkey. Used at registration time to decide whether
  to persist `(owner, operator, exit_delay)` metadata.
- `GetOORSessionByTxid` — New RPC that returns the Ark package and finalized
  checkpoints for an OOR session identified by its deterministic txid, gated
  by proof of a script that the session consumed.
- `participantKeysFromRow` / `authorizePolicySignerByRows` (in `policy_auth.go`)
  — Settlement-pair-restricted policy auth helpers. `participantKeysFromRow`
  derives the non-operator participant keys from a VTXO row's persisted
  `PolicyTemplate`, restricted to keys that appear in a valid settlement pair
  with the operator (not just any leaf in the policy). Used by
  `authorizeRegisteredOrPolicyScripts` to gate queries against persisted policy
  bytes rather than the looser registration-auth path.
- `authorizeScriptScopeQuery` (in `query_auth.go`) — Single canonical entry
  point for script-scope RPC authorization; runs proof verification and
  row-based policy authorization together so handlers cannot skip either step.
- `BuildScriptScopeProofMessageWithSigner` (in `proof.go`) — Exported helper
  that constructs and TLV-encodes a script-scope proof message bound to one
  explicit participant signer. Used by tests (e.g., harness) to build signed
  indexer query proofs without running a full client daemon.
- `RoundRow.OperatorPubKey` — Compressed operator public key committed to VTXOs
  created in this round. Added to the indexer's narrow `RoundRow` projection
  (the retired `SweepKey` field is removed).
- `ListVTXOsByPkScriptsAfter` — Keyset-paginated replacement for the previous
  offset-based `ListVTXOsByPkScripts`. The cursor is encoded as
  `(outpoint_hash, outpoint_index)` bytes; status filtering is pushed into the
  SQL query rather than applied in memory. `decodeVTXOCursor` / `encodeVTXOCursor`
  handle serialization. The old in-memory sort + slice pagination path is gone.
- `AncestryPreVisitor` / `AncestryPostVisitor` (in `ancestry_walk.go`) —
  Visitor callbacks for the shared OOR ancestry graph walk driver. `pre` runs
  in pre-order and returns parent session IDs to recurse into; `post` runs
  in post-order after all parents of the current session have been visited.
- `walkOORSessionAncestryDriver` (in `ancestry_walk.go`) — Shared recursion
  driver for OOR ancestry graph walks. Both the lineage-vbytes cap path
  (`lineage_vbytes.go`) and the recipient-events path (`service.go`) use this
  driver so depth bound, cycle protection, and visit ordering stay in lockstep.
  Cycle protection uses a `chainhash.Hash`-keyed seen-set; depth beyond
  `DefaultMaxLineageDepth` returns a typed error.

## Relationships

- **Depends on**: `clientconn` (per-client dispatch), `db` (wallet-scoped
  queries, `ExecReadTx`, indexer event persistence), `rounds` (round event
  subscription), `batch` (VTXO spend metadata).
- **Depended on by**: root `darepo` (wiring), `oor` (`RecipientNotifier`
  implementation), `rounds` (via the `rounds.VTXOEventPublisher` interface
  wired in `server_rounds.go` through `vtxoEventPublisherAdapter`).
- **Messages to/from**:
  - Receives query requests <- `clientconn` (from clients).
  - Returns query results -> `clientconn` (to clients).
  - Receives `PublishVTXOEvent` calls <- `rounds` (for confirmed round leaves)
    and <- `server_indexer.go` (legacy path, passes zero metadata).
  - Receives `PublishOORRecipientEvent` calls <- `oor` after finalization.
  - Fans out `IncomingVTXOEvent` / `IncomingOOREvent` -> `clientconn` (push to
    receive-script principals) and persists them to the `indexer_vtxo_events`
    / `indexer_oor_recipient_events` tables so offline recipients can
    reconcile via poll queries.

## Multi-Tree Ancestry

- `vtxoLineage.ancestryPaths` is a slice of `ancestryFragment` rather
  than a singular `treePath`. Round-direct and same-commitment OOR
  VTXOs surface a length-1 slice; cross-commitment multi-input OOR
  VTXOs surface one entry per distinct contributing commitment tx.
- `combineVirtualLineage` groups parents by `commitmentTxID`, runs the
  existing `tryResolveCombinedRoundPath` per group, and collects each
  result into the final `ancestryPaths`. The legacy
  `mixedSingularLineage` graceful-degrade branch is gone — the resolver
  now hard-errors when no fragment carries a tree path, surfacing the
  break loudly instead of silently dropping unilateral-exit info.
- `applyLineageMetadata` populates `arkrpc.VTXO.AncestryPaths` (one
  proto entry per fragment) and no longer writes the retired scalar
  `tree_path` / `tree_depth` fields. The wire shape is documented in
  `client/arkrpc/indexer.proto`.
- `LineageVBytes` (in `lineage_vbytes.go`) walks every input's
  ancestry — every tree node and OOR ancestor tx in the resolved
  lineage — de-duplicates by txid, and returns the cumulative
  witness-discounted vbytes a recipient would need to publish to
  claim the produced VTXO unilaterally. `EstimateOORLineageVBytes`
  is the public entrypoint consumed by `oor/`'s submit cap check.

## Invariants

- All queries are scoped to the authenticated Principal's wallet.
- Indexer is read-only with respect to rounds/OOR/VTXO state; it only writes
  its own event feed and receive-script registry.
- Owner-pubkey proof: when a receive script proof carries an owner pubkey
  (TLV type 10), the server reconstructs the expected VTXO tapscript from
  `(ownerKey, operatorKey, exitDelay)` and verifies the pkScript matches.
  The signature is verified against the raw owner key, not the taproot
  output key. When absent, the direct-P2TR path is used.
- Policy query auth uses settlement pairs, not all leaves: a key is a
  queryable participant only when it appears in at least one valid settlement
  pair (unilateral-auth leaf + operator-backed forfeit sibling). A key that
  appears only in a non-operator-backed leaf is filtered out to prevent
  read-access poisoning by stalker keys in custom policy templates.
- `authorizeScriptScopeQuery` is the mandatory single entry point for all
  four script-scope handlers; calling `verifyQueryScriptScopes` or
  `authorizeRegisteredOrPolicyScripts` in isolation skips a step and creates
  a security gap.
- `matchesStandardVTXOReceiveScript` returning false is **not** an error: it
  means the registration is for a generic script and receive-script metadata
  columns round-trip as nil.
- `ServiceMethod()` on indexer event messages returns `arkServiceName`
  (not `indexerServiceName`) to match client-side EventRouter routes.
- Lineage resolver must return errors on checkpoint fetch failure (not
  silently skip); partial lineage data is worse than an error.
- Tree path uses proto `TreePath` representation instead of raw TLV bytes.
- Query limits are enforced to prevent unbounded result sets.
- `ListVTXOsByScripts` uses keyset pagination: the cursor is
  `(outpoint_hash, outpoint_index)` bytes; status filtering runs in SQL.
  Concurrent inserts cannot cause items to be skipped or duplicated across
  pages (unlike the retired offset-cursor approach).
- VTXO event metadata persisted at `AddVTXOEvent` time must match the
  transient push payload — poll and push paths are symmetric.
- `BatchExpiryHeight` on published VTXO events is the **absolute** height
  (`confirmation_height + sweep_delay`), not the relative sweep delay.
- `CommitmentTxid` on published VTXO events is the signed commitment tx
  hash, not a leaf txid.
- `Origin` is `VTXO_ORIGIN_IN_ROUND` for confirmed round leaves; other origins
  are reserved for future OOR/refresh flows.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
