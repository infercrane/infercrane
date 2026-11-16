-- Aggregate optimization campaign state changes caused by candidate
-- transitions. A decision can be cleaned up seconds after it is made, so the
-- campaign row alone cannot tell a later reader that a campaign was rejected or
-- failed. Approval and cancellation stay on the campaign row only.
CREATE TABLE optimization_campaign_transitions (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    campaign_id TEXT NOT NULL REFERENCES optimization_campaigns(id) ON DELETE CASCADE,
    from_state TEXT NOT NULL,
    to_state TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (from_state <> to_state)
);

CREATE INDEX optimization_campaign_transitions_tenant_time_idx
    ON optimization_campaign_transitions(tenant_id,created_at);

-- Workspace notification destinations. The destination URL can itself be a
-- bearer credential (a Slack incoming webhook is one), so it is sealed like the
-- signing secret. Only a host-level hint is stored in the clear.
CREATE TABLE notification_subscriptions (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
    destination TEXT NOT NULL CHECK (destination IN ('webhook','slack')),
    url_hint TEXT NOT NULL,
    url_ciphertext BYTEA NOT NULL,
    url_nonce BYTEA NOT NULL,
    secret_ciphertext BYTEA NOT NULL,
    secret_nonce BYTEA NOT NULL,
    key_reference TEXT NOT NULL,
    events_json JSONB NOT NULL CHECK (jsonb_typeof(events_json)='array' AND jsonb_array_length(events_json) BETWEEN 1 AND 16),
    enabled BOOLEAN NOT NULL,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id,id)
);

CREATE INDEX notification_subscriptions_enabled_idx
    ON notification_subscriptions(tenant_id) WHERE enabled;

-- One row per event per destination. The body is stored exactly as signed so
-- every retry sends identical bytes.
CREATE TABLE notification_deliveries (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    subscription_id TEXT NOT NULL,
    event_id TEXT NOT NULL CHECK (char_length(event_id) BETWEEN 1 AND 200),
    event_type TEXT NOT NULL,
    campaign_id TEXT,
    body TEXT NOT NULL CHECK (octet_length(body) <= 65536),
    body_digest TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending','delivered','failed')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INTEGER NOT NULL CHECK (max_attempts BETWEEN 1 AND 10),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    claimed_until TIMESTAMPTZ,
    response_status INTEGER,
    error_code TEXT,
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    delivered_at TIMESTAMPTZ,
    UNIQUE (subscription_id,event_id),
    FOREIGN KEY (tenant_id,subscription_id)
        REFERENCES notification_subscriptions(tenant_id,id) ON DELETE CASCADE
);

CREATE INDEX notification_deliveries_due_idx
    ON notification_deliveries(next_attempt_at) WHERE status='pending';

CREATE INDEX notification_deliveries_tenant_time_idx
    ON notification_deliveries(tenant_id,created_at);
