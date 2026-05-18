# SQL Durability Operator Runbook

This runbook is for the client database after the durable-actor removal. The
database is the restart authority. Actors are in-memory execution lanes; if a
process restarts, pending SQL rows and domain state decide what resumes.

All timestamps in the new durability tables are Unix seconds. Replace `:now`
with `strftime('%s','now')` for SQLite, or `extract(epoch from now())::bigint`
for Postgres.

## First Response

Run these checks before restarting anything repeatedly:

```sql
SELECT 'client_round_effects' AS table_name, status, count(*)
FROM client_round_effects
GROUP BY status
UNION ALL
SELECT 'oor_client_effects', status, count(*)
FROM oor_client_effects
GROUP BY status
UNION ALL
SELECT 'unroll_effects', status, count(*)
FROM unroll_effects
GROUP BY status
UNION ALL
SELECT 'wallet_effects', status, count(*)
FROM wallet_effects
GROUP BY status
UNION ALL
SELECT 'mailbox_egress', status, count(*)
FROM mailbox_egress
GROUP BY status;
```

```sql
SELECT round_id, status, commitment_txid, confirmation_height,
       creation_time, last_update_time
FROM rounds
WHERE status NOT IN ('confirmed', 'failed', 'archived')
ORDER BY last_update_time ASC
LIMIT 50;
```

```sql
SELECT local_mailbox_id, remote_mailbox_id, pull_cursor,
       dispatch_committed_to, ack_target, ack_committed_to, last_error,
       updated_at
FROM mailbox_ingress_cursors
ORDER BY updated_at ASC
LIMIT 50;
```

Interpretation:

- `pending`: work is available or waiting for `next_attempt_at`.
- `claimed`: a worker owns the row until `claim_until`.
- `done`: completed. A growing `done` count is usually healthy.
- `dead`: max attempts exhausted. This needs investigation; the system will not
  retry it automatically.

## Effect Queues

Due client round effects:

```sql
SELECT id, round_id, effect_type, attempts, max_attempts, next_attempt_at,
       last_error, created_at, updated_at
FROM client_round_effects
WHERE status = 'pending' AND next_attempt_at <= :now
ORDER BY next_attempt_at ASC, created_at ASC
LIMIT 100;
```

Client round effects claimed past their lease:

```sql
SELECT id, round_id, effect_type, claim_owner, claim_until, attempts,
       last_error
FROM client_round_effects
WHERE status = 'claimed' AND claim_until IS NOT NULL AND claim_until < :now
ORDER BY claim_until ASC
LIMIT 100;
```

Dead client round effects:

```sql
SELECT id, round_id, effect_type, attempts, max_attempts, last_error,
       updated_at
FROM client_round_effects
WHERE status = 'dead'
ORDER BY updated_at DESC
LIMIT 100;
```

Due OOR client effects:

```sql
SELECT id, hex(session_id) AS session_id, direction, effect_type,
       attempts, max_attempts, next_attempt_at, last_error
FROM oor_client_effects
WHERE status = 'pending' AND next_attempt_at <= :now
ORDER BY next_attempt_at ASC, created_at ASC
LIMIT 100;
```

Dead OOR client effects:

```sql
SELECT id, hex(session_id) AS session_id, direction, effect_type,
       attempts, max_attempts, last_error, updated_at
FROM oor_client_effects
WHERE status = 'dead'
ORDER BY updated_at DESC
LIMIT 100;
```

Due unroll effects:

```sql
SELECT id, hex(target_outpoint_hash) AS outpoint_hash,
       target_outpoint_index, effect_type, hex(txid) AS txid, attempts,
       max_attempts, next_attempt_at, last_error
FROM unroll_effects
WHERE status = 'pending' AND next_attempt_at <= :now
ORDER BY next_attempt_at ASC, created_at ASC
LIMIT 100;
```

Dead unroll effects:

```sql
SELECT id, hex(target_outpoint_hash) AS outpoint_hash,
       target_outpoint_index, effect_type, attempts, max_attempts,
       last_error, updated_at
FROM unroll_effects
WHERE status = 'dead'
ORDER BY updated_at DESC
LIMIT 100;
```

Due wallet effects:

```sql
SELECT id, effect_type, hex(outpoint_hash) AS outpoint_hash, outpoint_index,
       hex(txid) AS txid, amount_sat, fee_sat, block_height, classification,
       attempts, max_attempts, next_attempt_at, last_error
FROM wallet_effects
WHERE status = 'pending' AND next_attempt_at <= :now
ORDER BY next_attempt_at ASC, created_at ASC
LIMIT 100;
```

Mailbox egress work that should send now:

```sql
SELECT id, connector, local_mailbox_id, remote_mailbox_id, rpc_kind,
       service, method, attempts, max_attempts, next_attempt_at, last_error
FROM mailbox_egress
WHERE status = 'pending' AND next_attempt_at <= :now
ORDER BY next_attempt_at ASC, created_at ASC
LIMIT 100;
```

Mailbox egress rows claimed past their lease:

```sql
SELECT id, connector, local_mailbox_id, remote_mailbox_id, service, method,
       claim_owner, claim_until, attempts, last_error
FROM mailbox_egress
WHERE status = 'claimed' AND claim_until IS NOT NULL AND claim_until < :now
ORDER BY claim_until ASC
LIMIT 100;
```

## Client Rounds

Active rounds:

```sql
SELECT round_id, status, hex(commitment_txid) AS commitment_txid,
       confirmation_height, creation_time, last_update_time
FROM rounds
WHERE status NOT IN ('confirmed', 'failed', 'archived')
ORDER BY last_update_time ASC;
```

Persisted MuSig2 secret nonces. These must exist before `send_nonces` is
considered safe to retry:

```sql
SELECT round_id, hex(signing_key) AS signing_key,
       length(pub_nonce) AS pub_nonce_bytes,
       length(sec_nonce) AS sec_nonce_bytes,
       creation_time, last_update_time
FROM client_round_nonce_state
ORDER BY last_update_time DESC
LIMIT 100;
```

Persisted aggregate nonces received from the server:

```sql
SELECT round_id, hex(txid) AS txid,
       length(agg_nonce) AS agg_nonce_bytes, creation_time, last_update_time
FROM client_round_agg_nonce_state
ORDER BY last_update_time DESC
LIMIT 100;
```

Persisted partial signatures before send:

```sql
SELECT round_id, hex(signing_key) AS signing_key, hex(txid) AS txid,
       length(partial_sig) AS partial_sig_bytes, creation_time,
       last_update_time
FROM client_round_partial_sig_state
ORDER BY last_update_time DESC
LIMIT 100;
```

Forfeit signature collection:

```sql
SELECT round_id, hex(vtxo_outpoint_hash) AS vtxo_hash,
       vtxo_outpoint_index, hex(connector_outpoint_hash) AS connector_hash,
       connector_outpoint_index, vtxo_amount, creation_time,
       last_update_time
FROM client_round_forfeit_request_state
ORDER BY last_update_time ASC
LIMIT 100;
```

Expected client round workers:

- `send_nonces`: reloads `client_round_nonce_state` and sends the public
  nonces. The secret nonce remains in SQL for restart-safe partial signing.
- `send_boarding_sigs` and `send_partial_sigs`: reload persisted signing facts
  and send to the server.
- `request_vtxo_forfeit_sigs` and `send_vtxo_forfeit_sigs`: bridge the round
  FSM and VTXO signing state.
- `register_confirmation`: registers the commitment transaction watch from
  persisted round facts.

## Client OOR

Active OOR sessions:

```sql
SELECT hex(session_id) AS session_id, direction, state, ark_txid,
       created_at, updated_at, fail_code, fail_reason
FROM oor_client_sessions
WHERE state NOT IN ('finalized', 'failed')
ORDER BY updated_at ASC
LIMIT 100;
```

Outgoing package artifacts:

```sql
SELECT hex(session_id) AS session_id, phase,
       length(ark_psbt) AS ark_psbt_bytes,
       updated_at
FROM oor_client_ark_artifacts
ORDER BY updated_at DESC
LIMIT 100;
```

Checkpoint artifacts:

```sql
SELECT hex(session_id) AS session_id, checkpoint_index, phase,
       length(checkpoint_psbt) AS checkpoint_psbt_bytes, updated_at
FROM oor_client_checkpoints
ORDER BY updated_at DESC
LIMIT 100;
```

Incoming materialization progress:

```sql
SELECT hex(session_id) AS session_id, output_index, hex(round_id) AS round_id,
       chain_depth, batch_expiry, updated_at
FROM oor_client_incoming_metadata
ORDER BY updated_at ASC
LIMIT 100;
```

Recipient cursor state:

```sql
SELECT hex(recipient_pk_script) AS recipient_pk_script, last_event_id,
       hex(last_session_id) AS last_session_id, updated_at
FROM oor_recipient_cursors
ORDER BY updated_at ASC
LIMIT 100;
```

## Unroll

Active jobs:

```sql
SELECT hex(target_outpoint_hash) AS outpoint_hash, target_outpoint_index,
       state, trigger, best_height, target_confirm_height,
       hex(sweep_txid) AS sweep_txid, sweep_confirm_height, updated_at,
       fail_reason
FROM unroll_jobs
WHERE state NOT IN ('completed', 'failed')
ORDER BY updated_at ASC
LIMIT 100;
```

Transaction progress:

```sql
SELECT hex(target_outpoint_hash) AS outpoint_hash, target_outpoint_index,
       hex(txid) AS txid, role, status, confirm_height,
       updated_at, last_error
FROM unroll_tx_progress
ORDER BY updated_at ASC
LIMIT 100;
```

Watches to re-arm after restart:

```sql
SELECT hex(target_outpoint_hash) AS outpoint_hash, target_outpoint_index,
       role, watch_id, hex(txid) AS txid, height_hint, status, updated_at,
       last_error
FROM unroll_watches
WHERE status NOT IN ('confirmed', 'spent', 'cancelled')
ORDER BY updated_at ASC
LIMIT 100;
```

## Mailbox Transport

Ingress cursor invariant:

```sql
SELECT local_mailbox_id, remote_mailbox_id, pull_cursor,
       dispatch_committed_to, ack_target, ack_committed_to,
       (ack_committed_to <= ack_target
        AND ack_target <= dispatch_committed_to) AS cursor_order_ok,
       last_error, updated_at
FROM mailbox_ingress_cursors
ORDER BY updated_at ASC;
```

How to read the cursors:

- `pull_cursor`: next remote event sequence to request.
- `dispatch_committed_to`: events before this cursor have committed domain SQL.
- `ack_target`: highest cursor the local process intends to ack remotely.
- `ack_committed_to`: highest cursor successfully acked remotely.

If `dispatch_committed_to` advances but `ack_committed_to` does not, the local
domain state is committed and the remote ack worker is the part to inspect. On
restart, duplicate envelopes before the ack cursor must be idempotent.

## Safe Manual Actions

Prefer restart first. A normal restart is safe because claimed rows are leased
and will become retryable when `claim_until < now`.

Only after identifying and fixing the root cause, an operator may requeue a
`dead` row by setting it back to `pending` with a future or immediate
`next_attempt_at`. Preserve `attempts` and `last_error` for audit unless there
is a specific reason to clear them.

Example shape:

```sql
UPDATE client_round_effects
SET status = 'pending',
    next_attempt_at = :now,
    claim_owner = NULL,
    claim_token = NULL,
    claim_until = NULL,
    updated_at = :now
WHERE id = :effect_id AND status = 'dead';
```

Do not delete domain rows to unstick work. Domain rows are the recovery source.
If a row is semantically obsolete, prefer a typed terminal state such as
`failed`, `done`, or `dead` with a useful `last_error` / `failure_reason`.
