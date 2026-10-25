-- +goose Up
-- +goose StatementBegin

-- VideoMachine / agente Infoproducto: desglose de un video de referencia.
-- Se cachea por huella de la fuente (sha256 del archivo o de la URL) para no
-- pagar dos veces el analisis del mismo video. Solo se guarda el desglose,
-- nunca el video: los medios descargados son temporales.
CREATE TABLE IF NOT EXISTS video_analyses (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_key  TEXT NOT NULL,                    -- "file:<sha256>" | "url:<sha256>"
    source_url  TEXT NOT NULL DEFAULT '',
    model       TEXT NOT NULL,
    meta        JSONB NOT NULL DEFAULT '{}',      -- duracion, resolucion, fps, cortes medidos
    breakdown   JSONB NOT NULL DEFAULT '{}',      -- desglose estructurado del cerebro
    summary     TEXT NOT NULL DEFAULT '',
    user_id     BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_video_analyses_source ON video_analyses (source_key);

-- Grupo de presets "formats": un .md por formato de video (UGC, desglose de
-- partes, infografico, lifestyle, spot de TV...). Vacio a proposito: el
-- switch "Formato" del front se habilita solo cuando haya presets activos.
INSERT INTO preset_groups (name, slug, description)
VALUES ('Formatos de video', 'formats', 'Formatos de video reutilizables (VideoMachine). El campo prompt guarda el .md del formato.')
ON CONFLICT (slug) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS video_analyses;
-- +goose StatementEnd
