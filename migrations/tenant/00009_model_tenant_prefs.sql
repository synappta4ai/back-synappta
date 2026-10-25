-- +goose Up
-- +goose StatementBegin

-- ─── Model Tenant Prefs (curaduría del tenant) ────────────────
-- Cada tenant hereda lo que el maestro deja activo y ajusta lo suyo: puede
-- desactivar más modelos y marcar sus propios favoritos (favorite_rank > 0,
-- varios permitidos). No puede reactivar lo que el maestro desactivó: eso se
-- resuelve al listar (el veto del maestro gana siempre).
CREATE TABLE IF NOT EXISTS model_tenant_prefs (
    model_name    TEXT NOT NULL PRIMARY KEY,
    enabled       BOOLEAN NOT NULL DEFAULT TRUE,
    favorite_rank INTEGER NOT NULL DEFAULT 0 CHECK (favorite_rank >= 0),
    updated_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS model_tenant_prefs;
-- +goose StatementEnd
