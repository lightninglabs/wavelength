DROP TABLE IF EXISTS batch_consumer_creator_lineage;
DROP TABLE IF EXISTS batch_provisional_consumers;
DROP TABLE IF EXISTS batch_dependent_vtxos;
DROP TABLE IF EXISTS batch_consumed_inputs;
DROP TABLE IF EXISTS batch_canonicality;

ALTER TABLE vtxos DROP COLUMN forfeit_consumer_txid;
ALTER TABLE vtxos DROP COLUMN business_revision;
