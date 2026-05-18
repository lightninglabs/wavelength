DROP INDEX IF EXISTS idx_mailbox_egress_correlation;
DROP INDEX IF EXISTS idx_mailbox_egress_pair;
DROP INDEX IF EXISTS idx_mailbox_egress_due;
DROP TABLE IF EXISTS mailbox_egress;

DROP INDEX IF EXISTS idx_mailbox_ingress_remote;
DROP TABLE IF EXISTS mailbox_ingress_cursors;
