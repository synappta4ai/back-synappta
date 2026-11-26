-- +goose Up
-- +goose StatementBegin

-- ─── Actualización remota pedida desde el panel ────────────────
-- El admin encola la actualización (POST /gpu/workers/:id/update) con la URL
-- del payload, el worker la recoge en el heartbeat (requested_update) y la
-- confirma recién cuando el proceso nuevo late con la versión nueva
-- (completed_update). Antes una GPU emparejada sólo se podía actualizar
-- abriendo el instalador en su propia máquina.
ALTER TABLE worker_registry
    ADD COLUMN IF NOT EXISTS requested_update JSONB;

-- +goose StatementEnd

-- +goose Down
ALTER TABLE worker_registry DROP COLUMN IF EXISTS requested_update;
