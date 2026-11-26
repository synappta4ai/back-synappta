-- +goose Up
-- +goose StatementBegin

-- ─── Borrados de pesos pedidos desde el panel ──────────────────
-- El admin encola (DELETE /gpu/workers/:id/models/:key) y el worker lo recoge
-- en el heartbeat (requested_deletions) y avisa al terminar
-- (completed_deletions). Sin esto no había forma de liberar disco de una
-- máquina vista por el bridge: el back sólo sabía pedir descargas.
ALTER TABLE worker_registry
    ADD COLUMN IF NOT EXISTS requested_deletions JSONB NOT NULL DEFAULT '[]'::jsonb;

-- +goose StatementEnd

-- +goose Down
ALTER TABLE worker_registry DROP COLUMN IF EXISTS requested_deletions;
