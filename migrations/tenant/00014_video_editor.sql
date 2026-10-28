-- +goose Up
-- +goose StatementBegin

-- ─── Editor de video: proyectos y jobs de render ────────────────
-- Solo tablas nuevas y sin claves foraneas hacia tablas existentes: una
-- migracion fallida aborta el arranque, asi que esta es deliberadamente
-- aislada (los ids de archivo se guardan como UUID plano).
CREATE TABLE IF NOT EXISTS editor_projects (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      BIGINT NOT NULL,
    name         TEXT NOT NULL DEFAULT '',
    project_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at   TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_editor_projects_user
    ON editor_projects (user_id, updated_at DESC) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS render_jobs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       BIGINT NOT NULL,
    project_id    UUID,
    status        VARCHAR(16) NOT NULL DEFAULT 'queued',
    progress      INTEGER NOT NULL DEFAULT 0,
    error_message TEXT NOT NULL DEFAULT '',
    project_json  JSONB NOT NULL DEFAULT '{}'::jsonb,
    file_id       UUID,
    output_url    TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_render_jobs_user
    ON render_jobs (user_id, created_at DESC);

-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS render_jobs;
DROP TABLE IF EXISTS editor_projects;
