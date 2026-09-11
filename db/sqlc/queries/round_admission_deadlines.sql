-- name: ConstrainRoundAdmissionDeadline :exec
INSERT INTO round_admission_deadlines (round_id, expires_at)
VALUES ($1, $2)
ON CONFLICT (round_id) DO UPDATE SET
    expires_at = CASE
        WHEN excluded.expires_at < round_admission_deadlines.expires_at
        THEN excluded.expires_at ELSE round_admission_deadlines.expires_at
    END;

-- name: GetRoundAdmissionDeadline :one
SELECT expires_at, closed FROM round_admission_deadlines WHERE round_id = $1;

-- name: CloseRoundAdmissionDeadline :exec
UPDATE round_admission_deadlines SET closed = TRUE WHERE round_id = $1;

-- name: AbandonRoundAdmissionDeadlines :exec
UPDATE round_admission_deadlines SET closed = TRUE WHERE closed = FALSE;
