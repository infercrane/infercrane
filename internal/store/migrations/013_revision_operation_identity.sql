ALTER TABLE deployment_revisions ADD COLUMN created_by_operation_id TEXT REFERENCES operations(id) ON DELETE SET NULL;
CREATE UNIQUE INDEX revisions_one_per_operation ON deployment_revisions(created_by_operation_id) WHERE created_by_operation_id IS NOT NULL;
