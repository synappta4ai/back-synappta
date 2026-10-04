-- +goose Up
-- +goose StatementBegin
-- Sistema de calificacion "de dos checks" para videos generados:
--   rating_good  = "Buena toma"  (aprobada para uso)
--   rating_final = "Elegida final" (seleccionada para el corte/reel final)
-- Ambos independientes y persistentes por generacion.

ALTER TABLE generation_logs
    ADD COLUMN rating_good  BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN rating_final BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX idx_generation_logs_rating ON generation_logs(rating_final) WHERE rating_final;

-- +goose StatementEnd

-- +goose Down
DROP INDEX IF EXISTS idx_generation_logs_rating;
ALTER TABLE generation_logs
    DROP COLUMN IF EXISTS rating_final,
    DROP COLUMN IF EXISTS rating_good;
