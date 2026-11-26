-- +goose Up
-- +goose StatementBegin

-- ─── Pausa (drenaje) y etiqueta de una GPU emparejada ──────────
-- paused: la GPU termina el job en curso y deja de reclamar nuevos hasta que se
-- reanude, sin desemparejarla (antes la única forma de sacar una máquina de
-- servicio era revocarla, que borra la relación y obliga a repetir el
-- emparejamiento en esa máquina).
--
-- label: nombre libre para el panel («PC de Ana», «Sala 2»). `name` ya existe y
-- ahora también se puede editar desde el panel.
ALTER TABLE worker_registry
    ADD COLUMN IF NOT EXISTS paused BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS label VARCHAR(63) NOT NULL DEFAULT '';

-- +goose StatementEnd

-- +goose Down
ALTER TABLE worker_registry
    DROP COLUMN IF EXISTS paused,
    DROP COLUMN IF EXISTS label;
