-- Bind amountless-invoice retries to the original payer amount. Historical
-- authorizations are fixed invoices, whose signed amount remains authoritative.
ALTER TABLE pay_authorizations ADD COLUMN amount_sat TEXT NOT NULL DEFAULT '0';
