-- +goose Up
-- +goose StatementBegin

-- ─── Descargas de modelos pedidas desde el panel ───────────────
-- El admin encola (POST /gpu/workers/:id/models/:key/download) y el worker
-- las recoge en el heartbeat (requested_models) y avisa al terminar
-- (completed_downloads). Sin esto el panel solo podía gestionar pesos del
-- worker central.
ALTER TABLE worker_registry
    ADD COLUMN IF NOT EXISTS requested_downloads JSONB NOT NULL DEFAULT '[]'::jsonb;

-- +goose StatementEnd

-- +goose Down
ALTER TABLE worker_registry DROP COLUMN IF EXISTS requested_downloads;
