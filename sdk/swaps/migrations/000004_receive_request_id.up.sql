-- request_id is the caller-chosen key for one receive request. It lets a
-- caller whose reply was lost find the invoice it already created instead of
-- creating another one. memo is the invoice memo the request asked for, kept
-- so a repeated request can be checked against the original parameters.
ALTER TABLE receive_swaps ADD COLUMN request_id TEXT;
ALTER TABLE receive_swaps ADD COLUMN memo TEXT NOT NULL DEFAULT '';

-- receive_swaps_request_id makes each request ID own at most one receive.
CREATE UNIQUE INDEX IF NOT EXISTS receive_swaps_request_id
    ON receive_swaps(request_id) WHERE request_id IS NOT NULL;
