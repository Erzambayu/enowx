-- Remove warmup_logs rows whose account no longer exists. Historically
-- DeleteAccount only removed the accounts row (warmup_logs has no FK to
-- accounts), so deleted/bulk-deleted accounts left their warmup history behind
-- as orphans that bloated the DB. DeleteAccount now clears them transactionally;
-- this backfills existing orphans. Idempotent: re-running finds nothing.
DELETE FROM warmup_logs
WHERE account_id NOT IN (SELECT id FROM accounts);
