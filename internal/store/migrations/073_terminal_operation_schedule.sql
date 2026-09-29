-- Terminal operations are intentionally unscheduled. The operation store
-- clears next_attempt_at on success and cancellation so terminal rows cannot
-- be mistaken for claimable work or expose stale retry metadata.
ALTER TABLE operations ALTER COLUMN next_attempt_at DROP NOT NULL;
