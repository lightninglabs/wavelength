-- vhtlc_recovery_jobs is the durable control table for unilateral vHTLC
-- recovery. Each row represents one pre-armed recovery action for one swap:
-- either claiming with the receiver preimage or refunding without receiver
-- cooperation. The swap row owns the raw preimage; this table stores only the
-- preimage hash plus enough deterministic, non-secret policy material to
-- build and persist the exit transaction before broadcast.
CREATE TABLE IF NOT EXISTS vhtlc_recovery_jobs (
    -- id is the daemon-owned recovery identifier returned to callers and used
    -- in logs. It is distinct from request_id so retries can be idempotent
    -- without forcing the caller to pick the durable row id.
    id TEXT PRIMARY KEY,

    -- request_id is the caller-owned idempotency key. Repeating a request with
    -- the same request_id returns the existing row only when the durable
    -- parameters match.
    request_id TEXT NOT NULL UNIQUE,

    -- swap_id links the recovery action back to the swap's durable state. The
    -- swap table remains the source of truth for swap lifecycle and preimage
    -- material.
    swap_id BLOB NOT NULL,

    -- direction records which side of the swap owns this recovery action. It
    -- is intentionally denormalized for logs and SQL/Grafana queries.
    direction TEXT NOT NULL CHECK (
        direction IN ('pay', 'receive', 'server_in', 'server_out')
    ),

    -- action selects the unilateral vHTLC leaf this job is allowed to spend.
    -- Cooperative refund with the receiver is not a recovery action; it stays
    -- on the cooperative OOR path.
    action TEXT NOT NULL CHECK (
        action IN ('claim', 'refund_without_receiver')
    ),

    -- state is the recovery FSM state. Terminal states are completed,
    -- cancelled, and failed. waiting_for_target and building_exit_spend are
    -- written by the execution-layer PR; this schema accepts them now so the
    -- later worker can restart from every pipeline boundary. cancelled means
    -- cooperative resolution won before recovery spent on-chain; failed means
    -- recovery needs operator attention.
    state TEXT NOT NULL CHECK (state IN (
        'armed',
        'unroll_started',
        'waiting_for_target',
        'waiting_for_csv',
        'building_exit_spend',
        'exit_spend_built',
        'submitting_exit_spend',
        'exit_spend_pending_confirmation',
        'completed',
        'cancelled',
        'failed'
    )),

    -- vtxo_* identifies the vHTLC VTXO that the unroll subsystem must
    -- materialize on-chain before this recovery can build its final exit
    -- spend.
    vtxo_txid BLOB NOT NULL,
    vtxo_vout INTEGER NOT NULL CHECK (vtxo_vout >= 0),
    vtxo_amount_sat BIGINT NOT NULL CHECK (vtxo_amount_sat > 0),

    -- *_pubkey columns are the vHTLC policy participants needed to reconstruct
    -- and validate the output script. They are public keys, not private
    -- signing material.
    sender_pubkey BLOB NOT NULL,
    receiver_pubkey BLOB NOT NULL,
    server_pubkey BLOB NOT NULL,

    -- Timelock and CSV parameters reconstruct the exact vHTLC policy leaves.
    -- refund_locktime is stored as SQLite INTEGER/sqlc int32 even though
    -- Bitcoin locktimes are unsigned; policy construction validates it before
    -- converting to wire-format locktime values. The CSV parameters are copied
    -- into the recovery row so the job can restart without depending on
    -- in-memory swap FSM state.
    refund_locktime INTEGER NOT NULL CHECK (refund_locktime > 0),
    unilateral_claim_delay INTEGER NOT NULL CHECK (
        unilateral_claim_delay > 0
    ),
    unilateral_refund_delay INTEGER NOT NULL CHECK (
        unilateral_refund_delay > 0
    ),
    unilateral_refund_without_receiver_delay INTEGER NOT NULL CHECK (
        unilateral_refund_without_receiver_delay > 0
    ),

    -- preimage_hash is safe to persist and log. It is the stable lookup key
    -- for claim-preimage material.
    preimage_hash BLOB NOT NULL,

    -- claim_preimage is nullable secret witness material. It is populated only
    -- for cross-process claim recovery where the daemon cannot call an
    -- in-process swap preimage resolver. The value must never be logged.
    claim_preimage BLOB,

    -- signer_key_* identifies the wallet key that signs the exit spend. It is
    -- a key locator, not a private key.
    signer_key_family INTEGER NOT NULL,
    signer_key_index INTEGER NOT NULL,

    -- destination_script is the wallet-controlled output script that receives
    -- recovered funds once the vHTLC exit spend confirms.
    destination_script BLOB NOT NULL,

    -- max_fee_rate_sat_per_kw caps the fee rate, in sat/kw, that the recovery
    -- worker may pay for the final exit spend. If the estimator exceeds this
    -- cap, recovery pauses/fails according to the worker policy rather than
    -- silently overpaying.
    max_fee_rate_sat_per_kw INTEGER NOT NULL CHECK (
        max_fee_rate_sat_per_kw > 0
    ),

    -- unroll_target_outpoint_* records the materialized on-chain output once
    -- unroll has produced it. Until then these columns remain NULL and the
    -- recovery job watches the unroll job for progress.
    unroll_target_outpoint_hash BLOB,
    unroll_target_outpoint_index INTEGER CHECK (
        unroll_target_outpoint_index IS NULL OR
        unroll_target_outpoint_index >= 0
    ),

    -- exit_policy_kind is the unroll policy kind registered by this recovery
    -- action. The CHECK is local to vHTLC recovery because this table owns only
    -- the vHTLC policy variants; generic unroll policy extensibility lives on
    -- the unilateral-exit job's exit-policy identity.
    exit_policy_kind TEXT NOT NULL CHECK (exit_policy_kind IN (
        'vhtlc_claim',
        'vhtlc_refund_without_receiver'
    )),

    -- exit_tx is the exact signed exit transaction persisted before broadcast.
    -- exit_txid is denormalized for log/search convenience. cooperative_txid
    -- records the transaction that made recovery unnecessary when the job is
    -- cancelled by a cooperative resolution.
    exit_tx BLOB,
    exit_txid BLOB,
    cooperative_txid BLOB,

    -- last_error is the latest retry or terminal failure detail. cancel_reason
    -- records why recovery was cancelled, usually because cooperative
    -- settlement won the race.
    last_error TEXT,
    cancel_reason TEXT,

    -- *_at columns are unix timestamps used for restart ordering, operator
    -- runbooks, and SQL/Grafana observability without requiring a separate
    -- metrics surface in v1.
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    armed_at BIGINT,
    escalated_at BIGINT,
    target_detected_at BIGINT,
    exit_tx_built_at BIGINT,
    exit_tx_broadcast_at BIGINT,
    terminal_at BIGINT,

    -- At most one claim and one refund-without-receiver recovery can exist for
    -- a swap. This also makes retries by swap/action safe when the caller lost
    -- the original request_id.
    UNIQUE(swap_id, action)
);

-- Claims non-terminal jobs in oldest-updated order and powers stuck-state
-- dashboards grouped by FSM state.
CREATE INDEX IF NOT EXISTS idx_vhtlc_recovery_jobs_state_updated
    ON vhtlc_recovery_jobs(state, updated_at DESC);

-- Supports idempotency and swap-centric inspection.
CREATE INDEX IF NOT EXISTS idx_vhtlc_recovery_jobs_swap_action
    ON vhtlc_recovery_jobs(swap_id, action);

-- Finds the recovery row attached to an already materialized unroll target.
CREATE INDEX IF NOT EXISTS idx_vhtlc_recovery_jobs_unroll_target
    ON vhtlc_recovery_jobs(
        unroll_target_outpoint_hash,
        unroll_target_outpoint_index
    )
    WHERE unroll_target_outpoint_hash IS NOT NULL;
