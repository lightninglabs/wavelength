-- Per-round sweep key locator persistence.
--
-- BatchSweeperActor used to sign every batch's CSV-timeout sweep with a
-- single ActorConfig.SweepKey resolved at startup, while the only sweep
-- key material persisted per round was the compressed pubkey. The lnd
-- KeyLocator (family + index) was thrown away. If the operator-configured
-- sweep key rotates between a round being finalized and its CSV expiry --
-- for example keyFamilyArkSweep moving from KeyFamilyMultiSig to 200 --
-- the sweeper signs with the new locator, producing a witness that does
-- not satisfy the historical tapleaf committed in the pre-rotation tree.
-- Broadcast then fails indefinitely and the batch's value is stranded
-- on-chain.
--
-- Persist the KeyLocator alongside the existing pubkey at round insert
-- so loadRoundFSM can reconstruct the historical KeyDescriptor on
-- restart. Columns are nullable so pre-migration rows -- where only the
-- compressed pubkey was recorded -- can still be loaded; the sweeper's
-- legacy-row fallback policy (matching pubkey proceeds with WarnS,
-- mismatched pubkey or fully absent metadata refuses to broadcast) lives
-- in batchsweeper/actor.go.
--
-- BIGINT (not INTEGER) because keychain.KeyLocator.Family and .Index are
-- uint32. Postgres INTEGER is a signed 32-bit type, so locator values
-- above MaxInt32 would not round-trip without sign-extension corruption.
-- BIGINT (signed 64-bit) safely covers the entire uint32 range.

ALTER TABLE rounds ADD COLUMN sweep_key_family BIGINT;
ALTER TABLE rounds ADD COLUMN sweep_key_index BIGINT;
