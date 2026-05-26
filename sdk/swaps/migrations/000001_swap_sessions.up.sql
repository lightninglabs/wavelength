-- Swap client session persistence.
-- This isolated schema stores high-level Lightning<->Ark swap session state in
-- a dedicated database so the swap SDK can resume across process restarts
-- without sharing migration history with the main Ark client database.

-- receive_swaps stores durable client-side Lightning->Ark receive sessions.
-- Each row captures the prepared invoice, negotiated vHTLC parameters, and
-- the latest observed claim/funding progress for one payment hash.
CREATE TABLE IF NOT EXISTS receive_swaps (
    -- payment_hash is the unique Lightning payment hash for this receive
    -- session and doubles as the primary resume key.
    payment_hash BLOB PRIMARY KEY,

    -- amount_sat is the requested invoice amount in satoshis.
    amount_sat BIGINT NOT NULL,

    -- payer_fee_msat is the payer-paid route fee quoted by the swap server.
    -- The fee is not deducted from amount_sat.
    payer_fee_msat BIGINT NOT NULL,

    -- state is the current client-side receive FSM state name.
    state TEXT NOT NULL,

    -- invoice is the BOLT-11 payment request returned to the caller.
    invoice TEXT NOT NULL,

    -- preimage is the fixed 32-byte preimage committed into the invoice and
    -- the matching vHTLC claim path.
    preimage BLOB NOT NULL,

    -- deadline_unix is the receive expiry deadline as a unix timestamp in
    -- seconds.
    deadline_unix BIGINT NOT NULL,

    -- client_pubkey is the receiver/client key used in the expected vHTLC.
    client_pubkey BLOB NOT NULL,

    -- payment_addr is the invoice MPP payment address the client validates
    -- against the forwarded final-hop onion.
    payment_addr BLOB NOT NULL DEFAULT x'',

    -- operator_pubkey is the Ark operator key used in the expected vHTLC.
    operator_pubkey BLOB NOT NULL,

    -- swap_server_pubkey is the swap server key used in the expected vHTLC.
    swap_server_pubkey BLOB NOT NULL,

    -- refund_locktime is the absolute refund locktime negotiated for the
    -- expected vHTLC.
    refund_locktime BIGINT NOT NULL,

    -- unilateral_claim_delay is the unilateral claim CSV delay in blocks.
    unilateral_claim_delay BIGINT NOT NULL,

    -- unilateral_refund_delay is the unilateral refund CSV delay in blocks.
    unilateral_refund_delay BIGINT NOT NULL,

    -- unilateral_refund_without_receiver_delay is the unilateral refund
    -- without-receiver CSV delay in blocks.
    unilateral_refund_without_receiver_delay BIGINT NOT NULL,

    -- vhtlc_pkscript is the exact vHTLC output script the client expects the
    -- swap server to fund.
    vhtlc_pkscript BLOB NOT NULL,

    -- vhtlc_policy_template is the semantic vHTLC policy template sent into
    -- the daemon when claiming the receive-side vHTLC.
    vhtlc_policy_template BLOB NOT NULL,

    -- vhtlc_outpoint is the observed funded vHTLC outpoint once indexed.
    vhtlc_outpoint TEXT NOT NULL DEFAULT '',

    -- vhtlc_amount is the observed funded vHTLC amount in satoshis.
    vhtlc_amount BIGINT NOT NULL DEFAULT 0,

    -- pending_htlc_ack_cursor is the mailbox cursor that still needs to be
    -- acknowledged after the HTLC event is durably accepted.
    pending_htlc_ack_cursor BIGINT NOT NULL DEFAULT 0,

    -- claim_receive_pubkey is the wallet-owned x-only pubkey allocated before
    -- the invoice is returned and reused as the destination for the
    -- receive-side claim spend.
    claim_receive_pubkey BLOB,

    -- claim_receive_pkscript is the exact wallet-owned receive script
    -- registered for the receive-side claim destination.
    claim_receive_pkscript BLOB,

    -- claim_session_id is the deterministic OOR session identifier returned
    -- by the daemon when the client submits the receive-side claim.
    claim_session_id TEXT NOT NULL DEFAULT '',

    -- intervention_reason is the durable terminal explanation stored when a
    -- receive session fails or stops in NeedsIntervention.
    intervention_reason TEXT NOT NULL DEFAULT '',

    -- created_at_unix is when this durable receive session row was first
    -- created.
    created_at_unix BIGINT NOT NULL,

    -- updated_at_unix is when this durable receive session row was last
    -- updated.
    updated_at_unix BIGINT NOT NULL
);

-- idx_receive_swaps_state supports resume sweeps over non-terminal receive
-- sessions while preserving stable creation/update ordering.
CREATE INDEX IF NOT EXISTS idx_receive_swaps_state
    ON receive_swaps(state, updated_at_unix);

-- pay_swaps stores durable client-side Ark->Lightning pay sessions.
-- Each row captures the negotiated in-swap parameters, the client funding
-- attempt, and the latest claim-preimage observation for one payment hash.
CREATE TABLE IF NOT EXISTS pay_swaps (
    -- payment_hash is the unique Lightning payment hash for this pay session
    -- and doubles as the primary resume key.
    payment_hash BLOB PRIMARY KEY,

    -- invoice is the original BOLT-11 invoice the client is paying.
    invoice TEXT NOT NULL,

    -- max_fee_sat is the caller-supplied routing fee ceiling in satoshis.
    max_fee_sat BIGINT NOT NULL,

    -- state is the current client-side pay FSM state name.
    state TEXT NOT NULL,

    -- amount_sat is the total amount the client must lock into the vHTLC.
    amount_sat BIGINT NOT NULL,

    -- fee_sat is the negotiated swap-server fee in satoshis.
    fee_sat BIGINT NOT NULL,

    -- expiry_unix is the server-provided wall-clock expiry as a unix timestamp
    -- in seconds.
    expiry_unix BIGINT NOT NULL,

    -- client_pubkey is the sender/client key used in the expected vHTLC.
    client_pubkey BLOB NOT NULL,

    -- operator_pubkey is the Ark operator key used in the expected vHTLC.
    operator_pubkey BLOB NOT NULL,

    -- server_pubkey is the swap server key used in the expected vHTLC.
    server_pubkey BLOB NOT NULL,

    -- refund_locktime is the absolute refund locktime negotiated for the
    -- expected vHTLC.
    refund_locktime BIGINT NOT NULL,

    -- unilateral_claim_delay is the unilateral claim CSV delay in blocks.
    unilateral_claim_delay BIGINT NOT NULL,

    -- unilateral_refund_delay is the unilateral refund CSV delay in blocks.
    unilateral_refund_delay BIGINT NOT NULL,

    -- unilateral_refund_without_receiver_delay is the unilateral refund
    -- without-receiver CSV delay in blocks.
    unilateral_refund_without_receiver_delay BIGINT NOT NULL,

    -- vhtlc_pkscript is the exact vHTLC output script the client expects to
    -- fund for the in-swap.
    vhtlc_pkscript BLOB NOT NULL,

    -- vhtlc_policy_template is the semantic vHTLC policy template sent into
    -- the daemon when funding the in-swap.
    vhtlc_policy_template BLOB NOT NULL,

    -- vhtlc_outpoint is the observed funded vHTLC outpoint once indexed.
    vhtlc_outpoint TEXT NOT NULL DEFAULT '',

    -- vhtlc_amount is the observed funded vHTLC amount in satoshis.
    vhtlc_amount BIGINT NOT NULL DEFAULT 0,

    -- funding_session_id is the deterministic OOR session identifier returned
    -- by the daemon when the client submits funding.
    funding_session_id TEXT NOT NULL DEFAULT '',

    -- refund_receive_pubkey is the wallet-owned x-only pubkey allocated as
    -- the destination for a timeout refund of the funded vHTLC.
    refund_receive_pubkey BLOB,

    -- refund_receive_pkscript is the exact wallet-owned receive script
    -- registered for the timeout refund destination.
    refund_receive_pkscript BLOB,

    -- refund_session_id is the deterministic OOR session identifier returned
    -- by the daemon when the client submits the timeout refund, or the
    -- observed spender txid when resume adopts an already-indexed refund.
    refund_session_id TEXT NOT NULL DEFAULT '',

    -- preimage is the claim preimage once the swap server's spend is
    -- authoritatively observed.
    preimage BLOB,

    -- intervention_reason is the durable terminal explanation stored when a
    -- pay session fails or stops in NeedsIntervention.
    intervention_reason TEXT NOT NULL DEFAULT '',

    -- created_at_unix is when this durable pay session row was first created.
    created_at_unix BIGINT NOT NULL,

    -- updated_at_unix is when this durable pay session row was last updated.
    updated_at_unix BIGINT NOT NULL
);

-- idx_pay_swaps_state supports resume sweeps over non-terminal pay sessions
-- while preserving stable creation/update ordering.
CREATE INDEX IF NOT EXISTS idx_pay_swaps_state
    ON pay_swaps(state, updated_at_unix);
