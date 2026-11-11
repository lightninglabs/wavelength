-- asset_deposit_claims records a claim that spends a confirmed asset deposit
-- through its owner's exit path, which returns the asset to the asset wallet.
-- A deposit has at most one claim, because the signed claim is fixed.
CREATE TABLE IF NOT EXISTS asset_deposit_claims (
    request_id TEXT PRIMARY KEY REFERENCES asset_deposits(request_id),

    claim_txid BLOB NOT NULL CHECK (length(claim_txid) = 32),

    -- status is claiming until tapd records the claim as confirmed.
    status TEXT NOT NULL CHECK (status IN ('claiming', 'claimed')),

    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_asset_deposit_claims_status
    ON asset_deposit_claims(status);
