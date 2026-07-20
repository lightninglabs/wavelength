DROP TABLE vtxo_redemption_outbox;
DROP TABLE round_vtxo_claims;

ALTER TABLE rounds DROP COLUMN sweep_delay;
ALTER TABLE round_vtxo_requests DROP COLUMN origin;

ALTER TABLE vtxos DROP COLUMN redemption_round_id;
