-- +goose Up
-- +goose StatementBegin

-- ─── Tenants ──────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS tenants (
    id         BIGSERIAL PRIMARY KEY,
    slug       VARCHAR(64) UNIQUE NOT NULL,
    name       VARCHAR(255) NOT NULL,
    active     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- ─── Platform Users (global identities, not tenant-scoped) ───
CREATE TABLE IF NOT EXISTS users (
    id            BIGSERIAL PRIMARY KEY,
    username      VARCHAR(255) UNIQUE NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    name          VARCHAR(255) NOT NULL DEFAULT '',
    surname       VARCHAR(255) NOT NULL DEFAULT '',
    user_name     VARCHAR(255) NOT NULL DEFAULT '',
    email         VARCHAR(255) NOT NULL DEFAULT '',
    platform_role INT NOT NULL DEFAULT 3, -- 0 SUPER_ADMIN, 1 ADMIN, 2 DIRECTOR, 3 USER
    active        BOOLEAN NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMP WITH TIME ZONE DEFAULT NULL,
    preferences   JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS idx_users_platform_role ON users(platform_role);

-- ─── Tenant Memberships (user ↔ tenant ↔ role) ───────────────
CREATE TABLE IF NOT EXISTS tenant_memberships (
    id         BIGSERIAL PRIMARY KEY,
    tenant_id  BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role_level INT NOT NULL DEFAULT 3,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_memberships_user ON tenant_memberships(user_id);

-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS tenant_memberships CASCADE;
DROP TABLE IF EXISTS users CASCADE;
DROP TABLE IF EXISTS tenants CASCADE;
