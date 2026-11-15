ALTER TABLE principals
  DROP CONSTRAINT principals_kind_check,
  ADD CONSTRAINT principals_kind_check
    CHECK(kind IN ('service_account','inference_token','cli_session'));

-- A browser-approved CLI sign-in. The CLI keeps the device code; only its
-- SHA-256 hash is stored. The short user code is shown in the terminal and in
-- the dashboard so the person approving can confirm both match. No credential
-- is stored here: the key is minted once, when the CLI exchanges an approved
-- request, and only its hash lands in principals.
CREATE TABLE cli_login_requests (
  id TEXT PRIMARY KEY,
  user_code TEXT NOT NULL UNIQUE
    CHECK(user_code ~ '^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$'),
  device_code_hash TEXT NOT NULL UNIQUE CHECK(device_code_hash ~ '^[0-9a-f]{64}$'),
  device_name TEXT NOT NULL CHECK(char_length(device_name) BETWEEN 1 AND 120),
  client_os TEXT NOT NULL CHECK(char_length(client_os) BETWEEN 1 AND 64),
  client_version TEXT NOT NULL CHECK(char_length(client_version) BETWEEN 1 AND 64),
  status TEXT NOT NULL CHECK(status IN ('pending','approved','denied','consumed')),
  tenant_id TEXT REFERENCES tenants(id) ON DELETE CASCADE,
  approver_id TEXT,
  approver_name TEXT,
  approver_role TEXT CHECK(approver_role IS NULL OR approver_role IN ('viewer','operator','admin')),
  approver_scopes_json JSONB,
  principal_id TEXT REFERENCES principals(id) ON DELETE SET NULL,
  created_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  decided_at TIMESTAMPTZ,
  consumed_at TIMESTAMPTZ,
  last_polled_at TIMESTAMPTZ,
  CHECK(expires_at > created_at),
  CHECK(status = 'pending' OR (decided_at IS NOT NULL AND approver_id IS NOT NULL)),
  CHECK(status NOT IN ('approved','consumed') OR (tenant_id IS NOT NULL AND approver_role IS NOT NULL AND approver_scopes_json IS NOT NULL))
);

CREATE INDEX cli_login_requests_pending_expiry_idx
  ON cli_login_requests(expires_at) WHERE status = 'pending';

CREATE INDEX cli_login_requests_created_idx
  ON cli_login_requests(created_at);

-- One row per key issued through a CLI sign-in. The key itself is the
-- principal; revoking the session disables that principal.
CREATE TABLE cli_sessions (
  principal_id TEXT PRIMARY KEY REFERENCES principals(id) ON DELETE CASCADE,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  owner_id TEXT NOT NULL,
  owner_name TEXT NOT NULL,
  device_name TEXT NOT NULL CHECK(char_length(device_name) BETWEEN 1 AND 120),
  client_os TEXT NOT NULL CHECK(char_length(client_os) BETWEEN 1 AND 64),
  client_version TEXT NOT NULL CHECK(char_length(client_version) BETWEEN 1 AND 64),
  created_at TIMESTAMPTZ NOT NULL,
  last_used_at TIMESTAMPTZ
);

CREATE INDEX cli_sessions_tenant_owner_idx
  ON cli_sessions(tenant_id, owner_id, created_at DESC);
