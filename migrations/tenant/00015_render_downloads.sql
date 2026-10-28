-- +goose Up
-- +goose StatementBegin

-- Exportaciones descargables: el archivo se deja en un temporal del servidor
-- solo hasta que el usuario lo descarga (no se registra en la biblioteca).
ALTER TABLE render_jobs ADD COLUMN IF NOT EXISTS output_path TEXT NOT NULL DEFAULT '';
ALTER TABLE render_jobs ADD COLUMN IF NOT EXISTS format TEXT NOT NULL DEFAULT 'mp4';

-- +goose StatementEnd

-- +goose Down
ALTER TABLE render_jobs DROP COLUMN IF EXISTS output_path;
ALTER TABLE render_jobs DROP COLUMN IF EXISTS format;
