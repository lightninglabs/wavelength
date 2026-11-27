-- name: SaveWalletFeeSplit :execrows
INSERT INTO wallet_fee_split (singleton, candidate) VALUES (1, $1)
ON CONFLICT (singleton) DO UPDATE SET candidate = excluded.candidate
WHERE wallet_fee_split.candidate = excluded.candidate;

-- name: LoadWalletFeeSplit :one
SELECT candidate FROM wallet_fee_split WHERE singleton = 1;

-- name: ClearWalletFeeSplit :exec
DELETE FROM wallet_fee_split WHERE singleton = 1 AND candidate = $1;
