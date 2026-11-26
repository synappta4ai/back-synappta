-- +goose Up
-- +goose StatementBegin

-- ─── Cancelaciones de descarga pedidas desde el panel ──────────
-- El admin cancela (DELETE /gpu/workers/:id/models/:key/download), el worker lo
-- recoge en el heartbeat (requested_download_cancels) y confirmala cuando el
-- gRPC local abortó la transferencia (completed_download_cancels). Sin esta
-- cola, pedir el modelo equivocado en una GPU emparejada obligaba a esperar
-- decenas de GB: no había forma de parar la descarga desde el panel.
ALTER TABLE worker_registry
    ADD COLUMN IF NOT EXISTS requested_download_cancels JSONB NOT NULL DEFAULT '[]'::jsonb;

-- +goose StatementEnd

-- +goose Down
ALTER TABLE worker_registry DROP COLUMN IF EXISTS requested_download_cancels;
