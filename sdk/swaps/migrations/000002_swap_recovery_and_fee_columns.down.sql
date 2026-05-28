ALTER TABLE pay_swaps DROP COLUMN refund_recovery_id;

ALTER TABLE receive_swaps DROP COLUMN claim_recovery_id;
ALTER TABLE receive_swaps DROP COLUMN payer_fee_msat;
