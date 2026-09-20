-- InferCrane owns the customer sandbox identity and content-free usage ledger.
-- Brezel identifiers stay server-side and are never authorization evidence.
CREATE TABLE native_sandboxes (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  created_by TEXT NOT NULL,
  display_name TEXT NOT NULL,
  purpose TEXT NOT NULL CHECK(purpose IN ('coding_agent','evaluation','background_task','blank_computer')),
  source_type TEXT NOT NULL CHECK(source_type IN ('git_repository','upload','empty_workspace')),
  source_reference TEXT NOT NULL DEFAULT '',
  template_id TEXT NOT NULL,
  model_endpoint TEXT NOT NULL DEFAULT '',
  brezel_project_id TEXT NOT NULL DEFAULT '',
  brezel_workspace_id TEXT NOT NULL DEFAULT '',
  brezel_sandbox_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK(status IN (
    'creating_workspace','creating','running','standby','pausing','resuming',
    'deleting','deleted','expired','failed','unknown','cleanup_pending'
  )),
  failure_code TEXT NOT NULL DEFAULT '',
  idempotency_key TEXT NOT NULL,
  input_digest TEXT NOT NULL CHECK(input_digest ~ '^[0-9a-f]{64}$'),
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  last_active_at TIMESTAMPTZ NOT NULL,
  deleted_at TIMESTAMPTZ,
  UNIQUE(tenant_id,idempotency_key)
);

CREATE UNIQUE INDEX native_sandboxes_provider_workspace_unique
  ON native_sandboxes(brezel_project_id,brezel_workspace_id)
  WHERE brezel_project_id<>'' AND brezel_workspace_id<>'';
CREATE UNIQUE INDEX native_sandboxes_provider_sandbox_unique
  ON native_sandboxes(brezel_project_id,brezel_sandbox_id)
  WHERE brezel_project_id<>'' AND brezel_sandbox_id<>'';
CREATE INDEX native_sandboxes_tenant_activity_idx
  ON native_sandboxes(tenant_id,last_active_at DESC);

CREATE TABLE sandbox_usage_events (
  event_id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  sandbox_id TEXT NOT NULL REFERENCES native_sandboxes(id) ON DELETE RESTRICT,
  provider_operation_id TEXT NOT NULL DEFAULT '',
  event_type TEXT NOT NULL,
  occurred_at TIMESTAMPTZ NOT NULL,
  template_id TEXT NOT NULL,
  runtime_class TEXT NOT NULL,
  running_milliseconds BIGINT NOT NULL DEFAULT 0 CHECK(running_milliseconds>=0),
  standby_milliseconds BIGINT NOT NULL DEFAULT 0 CHECK(standby_milliseconds>=0),
  command_duration_milliseconds BIGINT NOT NULL DEFAULT 0 CHECK(command_duration_milliseconds>=0),
  command_exit_class TEXT NOT NULL DEFAULT '',
  file_ingress_bytes BIGINT NOT NULL DEFAULT 0 CHECK(file_ingress_bytes>=0),
  file_egress_bytes BIGINT NOT NULL DEFAULT 0 CHECK(file_egress_bytes>=0),
  preview_requests BIGINT NOT NULL DEFAULT 0 CHECK(preview_requests>=0),
  metadata_version INTEGER NOT NULL CHECK(metadata_version>0),
  UNIQUE(tenant_id,sandbox_id,provider_operation_id,event_type)
);

CREATE INDEX sandbox_usage_events_tenant_time_idx
  ON sandbox_usage_events(tenant_id,occurred_at DESC);
CREATE INDEX sandbox_usage_events_sandbox_time_idx
  ON sandbox_usage_events(tenant_id,sandbox_id,occurred_at DESC);
