-- A buffered request on a primary-fallback endpoint moves to the next binding
-- when its first binding gave no response byte. It keeps one request record,
-- attributed to the binding that answered. These columns name the first
-- binding and revision it left, so revision and binding metrics (Release
-- Guard and the release monitor) count it as one request and one error of
-- what it left.
--   fallback_from_binding_id   binding the request left; NULL when it did not
--                              fail over
--   fallback_from_revision_id  that binding's deployment revision; NULL when
--                              it did not fail over or left a binding with no
--                              revision, such as an external API
ALTER TABLE request_records
  ADD COLUMN fallback_from_binding_id TEXT REFERENCES backend_bindings(id) ON DELETE SET NULL,
  ADD COLUMN fallback_from_revision_id TEXT REFERENCES deployment_revisions(id) ON DELETE SET NULL;

-- Revision and binding metrics read rows served by, or failed over away
-- from, one subject: each side of that OR keeps its own index.
CREATE INDEX request_records_fallback_from_revision_started_idx ON request_records(fallback_from_revision_id,started_at DESC)
  WHERE fallback_from_revision_id IS NOT NULL;
CREATE INDEX request_records_fallback_from_binding_started_idx ON request_records(fallback_from_binding_id,started_at DESC)
  WHERE fallback_from_binding_id IS NOT NULL;
