-- Preserve caller authority before a server can reserve or debit credit.
-- Text preserves the full uint64 cap and fee range without signed wrapping.
CREATE TABLE pay_authorizations (
    payment_hash BLOB PRIMARY KEY,
    invoice TEXT NOT NULL,
    account_pubkey BLOB NOT NULL,
    max_fee_sat TEXT NOT NULL,
    max_credit_sat TEXT NOT NULL
);

-- NULL means the historical client did not retain the server credit plan.
ALTER TABLE pay_swaps ADD COLUMN credit_quote BLOB;
