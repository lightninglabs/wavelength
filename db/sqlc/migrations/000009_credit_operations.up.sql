-- credit_operations stores the full durable state for one client-side credit
-- orchestration operation per op id. The credit registry actor owns these
-- rows: it spawns one durable per-operation actor per non-terminal record and
-- restores them on boot. This table is the single source of truth for credit
-- orchestration state -- the per-operation actor reads and writes it directly
-- inside its Read/Stage/Commit phases and does NOT use the generic
-- actor-delivery fsm_checkpoints blob.
--
-- The server credit ledger remains authoritative for the MONEY; these rows
-- track only the client's progress through a multi-step flow (top-up, await
-- credit, pay; receive; redeem) so the flow survives a crash or a client
-- disconnect and resumes with a STABLE idempotency key instead of minting a
-- fresh one each retry. The control-plane fields (kind, state, op_key, status)
-- are first-class queryable columns; the irreducible opaque resume material
-- rides in snapshot_data, which nothing queries by and only needs to
-- round-trip.
CREATE TABLE IF NOT EXISTS credit_operations (
    -- op_id is the stable, unique per-admission credit operation identifier.
    -- The durable actor mailbox id is derived from it, so it must be stable
    -- across restarts. A keyed retry after a terminal failure admits a fresh
    -- op_id under the same op_key.
    op_id TEXT NOT NULL,

    -- op_key is the client idempotency key for this operation:
    --   pay:<payment_hash_hex>      sub-dust / shortfall pay (+ optional top-up)
    --   recv:<random_hex>           credit receive
    --   redeem:<random_hex>         credits -> vTXO redemption
    -- A pay key is STABLE: it is the payee invoice payment hash, so a re-issued
    -- pay reuses the same key, and the SAME key is passed to the server
    -- CreateCredit AND to the delegated OOR top-up transfer, so at most one OOR
    -- transfer ever exists per pay regardless of crash timing. This is the
    -- invariant that closes the double-top-up window.
    --
    -- A receive key is freshly RANDOM, not derived from the payment hash: the
    -- hash is not known until the server mints the invoice, and an inbound
    -- credit lands on the account by identity key regardless of which invoice
    -- the payer settles, so a receive carries no cross-call double-spend risk
    -- that a stable key would need to dedup. A redeem key is likewise random:
    -- each sweep is a distinct one-shot materialization.
    op_key TEXT NOT NULL,

    -- kind records the credit operation family:
    --   1 = pay      (optional Ark top-up, then credit/mixed pay)
    --   2 = receive  (server-owned Lightning receive that credits the account)
    --   3 = redeem   (materialize available credits back into an Ark vTXO)
    kind INTEGER NOT NULL,

    -- state is the latest FSM state string, kept as a queryable column for
    -- diagnostics and restore filtering.
    state TEXT NOT NULL,

    -- status is the coordinator-facing operation status:
    --   0 = pending (in flight)
    --   1 = completed
    --   2 = failed
    status INTEGER NOT NULL,

    -- server_op_id is the swap-server credit operation id returned by
    -- CreateCredit / RedeemCredit. Persisted before advancing so a resume
    -- reconciles against the same server operation.
    server_op_id TEXT,

    -- payment_hash is the BOLT-11 payment hash for pay and receive operations.
    payment_hash BLOB,

    -- destination_pubkey is the server-owned Ark destination for an ARK_TOPUP
    -- (pay), or the wallet-owned receive destination for a redemption.
    destination_pubkey BLOB,

    -- oor_session_id is the delegated OOR transfer session id (top-up funding
    -- or redemption payout) once the OOR registry has admitted it.
    oor_session_id TEXT,

    -- invoice is the BOLT-11 invoice: the target invoice for a pay, or the
    -- server-owned receive invoice for a receive.
    invoice TEXT,

    -- amount_sat is the principal amount for the operation (pay invoice amount,
    -- receive amount, or redeemed amount).
    amount_sat BIGINT NOT NULL DEFAULT 0,

    -- topup_sat is the Ark top-up amount required to cover a pay shortfall.
    topup_sat BIGINT NOT NULL DEFAULT 0,

    -- max_credit_sat is the credit cap passed to StartPay for a pay operation.
    max_credit_sat BIGINT NOT NULL DEFAULT 0,

    -- max_fee_sat is the caller's max routing fee for a pay operation.
    max_fee_sat BIGINT NOT NULL DEFAULT 0,

    -- last_error stores the latest terminal failure reason.
    last_error TEXT,

    -- snapshot_data is the TLV-encoded per-operation resume snapshot.
    snapshot_data BLOB,

    -- snapshot_version is the encoding version of snapshot_data.
    snapshot_version INTEGER NOT NULL DEFAULT 0,

    -- created_at is the unix timestamp when the row was first written.
    created_at BIGINT NOT NULL,

    -- updated_at is the unix timestamp of the latest row update.
    updated_at BIGINT NOT NULL,

    PRIMARY KEY (op_id)
);

-- The (status, created_at) index serves the boot-time non-terminal restore
-- scan and any future bounded-retention sweep of terminal rows. Terminal rows
-- are retained for status RPCs and diagnostics; they are not pruned at reap
-- time.
CREATE INDEX IF NOT EXISTS idx_credit_operations_status_created
    ON credit_operations(status, created_at ASC);

-- At most one live-or-completed operation may carry a given op_key: the partial
-- UNIQUE index enforces the dedup invariant in the schema rather than in Go.
-- Failed rows (status 2) drop out of the index so a keyed retry after a failure
-- can admit a fresh operation under the same key.
CREATE UNIQUE INDEX IF NOT EXISTS idx_credit_operations_op_key
    ON credit_operations(op_key)
    WHERE status != 2;
