CREATE TABLE infercrane_users (
  id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  disabled BOOLEAN NOT NULL DEFAULT FALSE,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE external_user_identities (
  provider TEXT NOT NULL,
  external_user_id TEXT NOT NULL,
  user_id TEXT NOT NULL REFERENCES infercrane_users(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY(provider, external_user_id),
  UNIQUE(provider, user_id)
);

CREATE TABLE external_organization_identities (
  provider TEXT NOT NULL,
  external_organization_id TEXT NOT NULL,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY(provider, external_organization_id),
  UNIQUE(provider, tenant_id)
);

CREATE TABLE organization_memberships (
  user_id TEXT NOT NULL REFERENCES infercrane_users(id) ON DELETE CASCADE,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  role TEXT NOT NULL CHECK(role IN ('viewer','operator','admin')),
  scopes_json JSONB NOT NULL DEFAULT '[]' CHECK(jsonb_typeof(scopes_json) = 'array'),
  status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','suspended')),
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY(user_id, tenant_id)
);

CREATE TABLE entitlements (
  id TEXT PRIMARY KEY,
  tenant_id TEXT REFERENCES tenants(id) ON DELETE CASCADE,
  user_id TEXT REFERENCES infercrane_users(id) ON DELETE CASCADE,
  entitlement_key TEXT NOT NULL,
  revoked_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  CHECK((tenant_id IS NOT NULL)::integer + (user_id IS NOT NULL)::integer = 1)
);

CREATE UNIQUE INDEX entitlements_tenant_key
  ON entitlements(tenant_id, entitlement_key)
  WHERE tenant_id IS NOT NULL;
CREATE UNIQUE INDEX entitlements_user_key
  ON entitlements(user_id, entitlement_key)
  WHERE user_id IS NOT NULL;
CREATE INDEX memberships_tenant_status ON organization_memberships(tenant_id, status);
