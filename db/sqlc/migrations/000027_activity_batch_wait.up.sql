-- Keep the selected window on the current activity row as well as the
-- append-only event payload. Empty metadata clears a completed wait.
ALTER TABLE activity_entries ADD COLUMN batch_wait_json TEXT NOT NULL DEFAULT '';
