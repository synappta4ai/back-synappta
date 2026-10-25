-- +goose Up
-- +goose StatementBegin

-- ─── Model Master Prefs (curaduría de plataforma) ──────────────
-- Un maestro (platform superadmin) decide qué modelos del catálogo ve cada
-- tenant: enabled=false los oculta en todos los listados y bloquea la
-- generación (el tenant no puede reactivarlos). recommended marca los
-- eficientes con badge; favorite_rank > 0 los sube al frente del listado de
-- su modalidad (admite varios favoritos, ordenados por rank y luego nombre).
-- Solo cubre nombres del catálogo conocido (curados + nubes); los downloaded
-- del worker son dinámicos por tenant y se regulan a nivel tenant.
CREATE TABLE IF NOT EXISTS model_master_prefs (
    model_name    TEXT NOT NULL PRIMARY KEY,
    enabled       BOOLEAN NOT NULL DEFAULT TRUE,
    recommended   BOOLEAN NOT NULL DEFAULT FALSE,
    favorite_rank INTEGER NOT NULL DEFAULT 0 CHECK (favorite_rank >= 0),
    updated_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS model_master_prefs;
