CREATE TABLE tenant_request_quota_windows (
 tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
 window_start TIMESTAMPTZ NOT NULL,
 reserved_requests INTEGER NOT NULL CHECK(reserved_requests >= 0),
 PRIMARY KEY(tenant_id, window_start)
);

CREATE INDEX tenant_request_quota_windows_expiry_idx
 ON tenant_request_quota_windows(window_start);
