-- +goose Up
-- +goose StatementBegin

-- ─── Disco libre del worker ───────────────────────────────────
-- El heartbeat manda cuánto espacio queda donde viven los pesos. El panel lo
-- muestra y bloquea descargas cuando no entra el modelo: antes el error
-- llegaba recién al final de una descarga de 9 GB.
ALTER TABLE worker_registry
    ADD COLUMN IF NOT EXISTS disk_free_mb BIGINT NOT NULL DEFAULT 0;

-- +goose StatementEnd

-- +goose Down
ALTER TABLE worker_registry DROP COLUMN IF EXISTS disk_free_mb;
