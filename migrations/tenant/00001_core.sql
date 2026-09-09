-- +goose Up
-- +goose StatementBegin

-- Per-tenant roles ladder (same levels as dcs-back).
CREATE TABLE roles (
    id    INT PRIMARY KEY,
    name  VARCHAR(50) UNIQUE NOT NULL,
    level INT UNIQUE NOT NULL
);

-- ─── Files ────────────────────────────────────────────────────
CREATE TABLE files (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    filename    VARCHAR(255) NOT NULL,
    path        TEXT NOT NULL,
    size        BIGINT NOT NULL,
    mime_type   VARCHAR(127) NOT NULL,
    category    VARCHAR(31) NOT NULL,
    format      VARCHAR(15) NOT NULL,
    storage     VARCHAR(15) NOT NULL DEFAULT 'persistent',
    duration    DOUBLE PRECISION NOT NULL DEFAULT 0,
    trashed     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    deleted_at  TIMESTAMP WITH TIME ZONE DEFAULT NULL
);

CREATE INDEX idx_files_category ON files (category);
CREATE INDEX idx_files_storage ON files (storage);
CREATE INDEX idx_files_deleted_at ON files (deleted_at);
CREATE INDEX idx_files_trashed ON files (trashed);

-- ─── Ingredients (replaces characters: talent, props, brands) ─
CREATE TABLE ingredients (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        VARCHAR(255) NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    metadata    JSONB DEFAULT '{}',
    created_at  TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    deleted_at  TIMESTAMP WITH TIME ZONE DEFAULT NULL
);

CREATE INDEX idx_ingredients_name ON ingredients (name);
CREATE INDEX idx_ingredients_deleted_at ON ingredients (deleted_at);

CREATE TABLE ingredient_files (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ingredient_id UUID NOT NULL REFERENCES ingredients(id) ON DELETE CASCADE,
    file_id       UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    role          VARCHAR(63) NOT NULL DEFAULT 'reference',
    created_at    TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(ingredient_id, file_id, role)
);

CREATE INDEX idx_ingredient_files_ingredient ON ingredient_files (ingredient_id);
CREATE INDEX idx_ingredient_files_file ON ingredient_files (file_id);

-- ─── Events (replaces projects) ───────────────────────────────
CREATE TABLE events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        VARCHAR(250) NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    metadata    TEXT,
    venue       VARCHAR(255) NOT NULL DEFAULT '',
    starts_at   TIMESTAMP WITH TIME ZONE,
    ends_at     TIMESTAMP WITH TIME ZONE,
    status      VARCHAR(31) NOT NULL DEFAULT 'draft', -- draft | published | done
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    deleted_at  TIMESTAMP WITH TIME ZONE DEFAULT NULL
);

CREATE INDEX idx_events_status ON events(status);
CREATE INDEX idx_events_deleted_at ON events(deleted_at);

-- ─── Programs (replaces chapters: run-of-show blocks) ─────────
CREATE TABLE programs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id     UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    number       INT NOT NULL,
    name         VARCHAR(250) NOT NULL DEFAULT '',
    description  TEXT NOT NULL DEFAULT '',
    scheduled_at TIMESTAMP WITH TIME ZONE,
    sort_order   INT NOT NULL DEFAULT 0,
    active       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    deleted_at   TIMESTAMP WITH TIME ZONE DEFAULT NULL
);

CREATE INDEX idx_programs_event ON programs(event_id);
CREATE INDEX idx_programs_deleted_at ON programs(deleted_at);

-- ─── Pieces (merges scenes + shots: one deliverable unit) ─────
CREATE TABLE pieces (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id        UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    program_id      UUID REFERENCES programs(id) ON DELETE SET NULL,
    number          INT NOT NULL,
    piece_code      VARCHAR(63) NOT NULL DEFAULT '',
    name            VARCHAR(250) NOT NULL DEFAULT '',
    description     TEXT NOT NULL DEFAULT '',
    type            VARCHAR(63) NOT NULL DEFAULT 'custom', -- opening_video, flyer, lower_third, countdown, social_clip, custom...
    output_format   VARCHAR(31) NOT NULL DEFAULT '',
    duration        INT NOT NULL DEFAULT 0,
    aspect_ratio    VARCHAR(15) NOT NULL DEFAULT '',
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    deleted_at      TIMESTAMP WITH TIME ZONE DEFAULT NULL
);

CREATE INDEX idx_pieces_program ON pieces(program_id);
CREATE INDEX idx_pieces_event ON pieces(event_id);
CREATE INDEX idx_pieces_deleted_at ON pieces(deleted_at);

-- ─── Piece Generations (replaces takes) ───────────────────────
CREATE TABLE piece_generations (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    piece_id         UUID NOT NULL REFERENCES pieces(id) ON DELETE CASCADE,
    number           INT NOT NULL CHECK (number >= 1),
    video_url        TEXT NOT NULL DEFAULT '',
    video_local_url  TEXT NOT NULL DEFAULT '',
    status           VARCHAR(50) NOT NULL DEFAULT 'pending',
    active           BOOLEAN NOT NULL DEFAULT TRUE,
    final            BOOLEAN NOT NULL DEFAULT FALSE,
    finalized_at     TIMESTAMP WITH TIME ZONE,
    task_id          VARCHAR(255) NOT NULL DEFAULT '',
    rating           INT NOT NULL DEFAULT 0,
    created_at       TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at       TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    deleted_at       TIMESTAMP WITH TIME ZONE DEFAULT NULL
);

CREATE INDEX idx_piece_generations_piece ON piece_generations(piece_id);
CREATE INDEX idx_piece_generations_active ON piece_generations(active);
CREATE INDEX idx_piece_generations_deleted_at ON piece_generations(deleted_at);

-- Partial unique index: only one active generation per piece per number.
CREATE UNIQUE INDEX idx_piece_generations_active_unique
    ON piece_generations(piece_id, number)
    WHERE deleted_at IS NULL AND active = true;

-- ─── Agnostic assignments (polymorphic) ───────────────────────
-- One table for attaching any resource to any assignable element:
-- assignable_type: 'event' | 'program' | 'piece' (extensible)
-- resource_type:   'ingredient' | 'file' | 'preset' | 'skill' | 'model'
CREATE TABLE assignments (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    assignable_type VARCHAR(31) NOT NULL,
    assignable_id   UUID NOT NULL,
    resource_type   VARCHAR(31) NOT NULL,
    resource_id     UUID NOT NULL,
    slot            VARCHAR(63) NOT NULL DEFAULT '',
    created_by      BIGINT,
    created_at      TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (assignable_type, assignable_id, resource_type, resource_id, slot)
);

CREATE INDEX idx_assignments_target ON assignments (assignable_type, assignable_id);
CREATE INDEX idx_assignments_resource ON assignments (resource_type, resource_id);

-- ─── Presets & groups (event-oriented seed kept from dcs-back) ─
CREATE TABLE preset_groups (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(100) NOT NULL UNIQUE,
    slug VARCHAR(50) NOT NULL UNIQUE,
    description TEXT,
    active BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    deleted_at TIMESTAMPTZ DEFAULT NULL
);

CREATE TABLE presets (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    group_id UUID NOT NULL REFERENCES preset_groups(id),
    code VARCHAR(100) NOT NULL,
    label VARCHAR(200) NOT NULL,
    label_key VARCHAR(100) NOT NULL DEFAULT '',
    prompt TEXT NOT NULL,
    active BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    deleted_at TIMESTAMPTZ DEFAULT NULL,
    UNIQUE(group_id, code)
);

CREATE INDEX idx_presets_group ON presets(group_id);

-- ─── Skills (reusable system prompts) ──────────────────────────
CREATE TABLE skills (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    system_prompt TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMP WITH TIME ZONE
);

-- ─── Generation logs (event context) ──────────────────────────
CREATE TABLE generation_logs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id         VARCHAR(255) NOT NULL,
    model_name      VARCHAR(255) NOT NULL,
    user_id         BIGINT,
    event_id        UUID,
    program_id      UUID,
    piece_id        UUID,
    piece_code      VARCHAR(63) NOT NULL DEFAULT '',
    generation_number INT NOT NULL DEFAULT 0,
    request_payload TEXT,
    outputs         TEXT,
    status          VARCHAR(31) NOT NULL DEFAULT 'running',
    error_message   TEXT,
    resource_type   VARCHAR(31) NOT NULL DEFAULT '',
    content_types   VARCHAR(127) NOT NULL DEFAULT '',
    estimated_cost  DOUBLE PRECISION NOT NULL DEFAULT 0,
    cost_source     VARCHAR(31) NOT NULL DEFAULT '',
    created_at      TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    deleted_at      TIMESTAMP WITH TIME ZONE DEFAULT NULL
);

CREATE INDEX idx_generation_logs_task ON generation_logs(task_id);
CREATE INDEX idx_generation_logs_event ON generation_logs(event_id);
CREATE INDEX idx_generation_logs_piece ON generation_logs(piece_id);
CREATE INDEX idx_generation_logs_status ON generation_logs(status);
CREATE INDEX idx_generation_logs_deleted_at ON generation_logs(deleted_at);

-- ─── Server communications (external API traces) ──────────────
CREATE TABLE server_communications (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id       VARCHAR(255) NOT NULL DEFAULT '',
    model_name    VARCHAR(255) NOT NULL,
    endpoint      TEXT NOT NULL DEFAULT '',
    method        VARCHAR(15) NOT NULL DEFAULT 'POST',
    request_body  TEXT,
    response_body TEXT,
    status_code   INT NOT NULL DEFAULT 0,
    duration_ms   BIGINT NOT NULL DEFAULT 0,
    error_message TEXT,
    created_at    TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_server_comms_task ON server_communications(task_id);

-- ─── Generated assets ──────────────────────────────────────────
CREATE TABLE generated_assets (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id       VARCHAR(255) NOT NULL,
    model_name    VARCHAR(255) NOT NULL DEFAULT '',
    user_id       BIGINT,
    event_id      UUID,
    program_id    UUID,
    piece_id      UUID,
    piece_code    VARCHAR(63) NOT NULL DEFAULT '',
    generation_number INT NOT NULL DEFAULT 0,
    original_url  TEXT NOT NULL,
    local_path    TEXT,
    filename      TEXT,
    mime_type     TEXT,
    file_size     BIGINT NOT NULL DEFAULT 0,
    status        VARCHAR(31) NOT NULL DEFAULT 'pending',
    confirmed_at  TIMESTAMP WITH TIME ZONE,
    created_at    TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    deleted_at    TIMESTAMP WITH TIME ZONE DEFAULT NULL
);

CREATE INDEX idx_generated_assets_task ON generated_assets(task_id);
CREATE INDEX idx_generated_assets_piece ON generated_assets(piece_id);

-- ─── Model assets (BytePlus gallery sync) ──────────────────────
CREATE TABLE model_assets (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id      VARCHAR(255) NOT NULL,
    file_id       UUID NOT NULL,
    asset_id      VARCHAR(255) NOT NULL DEFAULT '',
    asset_group_id VARCHAR(255) NOT NULL DEFAULT '',
    status        VARCHAR(31) NOT NULL DEFAULT 'syncing',
    error_message TEXT,
    asset_url     TEXT,
    asset_type    VARCHAR(31) NOT NULL DEFAULT '',
    reference_uri TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_model_assets_model ON model_assets(model_id);
CREATE INDEX idx_model_assets_file ON model_assets(file_id);

-- ─── Push subscriptions (per tenant) ───────────────────────────
CREATE TABLE push_subscriptions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     BIGINT NOT NULL,
    endpoint    TEXT NOT NULL,
    p256dh      TEXT NOT NULL,
    auth        TEXT NOT NULL,
    user_agent  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, endpoint)
);

CREATE INDEX idx_push_subscriptions_user ON push_subscriptions(user_id);

-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS push_subscriptions CASCADE;
DROP TABLE IF EXISTS model_assets CASCADE;
DROP TABLE IF EXISTS generated_assets CASCADE;
DROP TABLE IF EXISTS server_communications CASCADE;
DROP TABLE IF EXISTS generation_logs CASCADE;
DROP TABLE IF EXISTS skills CASCADE;
DROP TABLE IF EXISTS presets CASCADE;
DROP TABLE IF EXISTS preset_groups CASCADE;
DROP TABLE IF EXISTS assignments CASCADE;
DROP TABLE IF EXISTS piece_generations CASCADE;
DROP TABLE IF EXISTS pieces CASCADE;
DROP TABLE IF EXISTS programs CASCADE;
DROP TABLE IF EXISTS events CASCADE;
DROP TABLE IF EXISTS ingredient_files CASCADE;
DROP TABLE IF EXISTS ingredients CASCADE;
DROP TABLE IF EXISTS files CASCADE;
DROP TABLE IF EXISTS roles CASCADE;
