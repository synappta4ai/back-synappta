-- +goose Up
-- +goose StatementBegin

-- ─── User Consents (Ley 1581/2012: prueba de autorización) ──────
-- Cada aceptación de Términos o Política de Privacidad queda registrada
-- con quién, cuándo, cómo y qué versión aceptó. Trazable ante la SIC.
CREATE TABLE IF NOT EXISTS user_consents (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    doc_type    VARCHAR(16) NOT NULL CHECK (doc_type IN ('terms', 'privacy')),
    doc_version VARCHAR(32) NOT NULL,
    accepted_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    method      VARCHAR(16) NOT NULL DEFAULT 'first-login'
        CHECK (method IN ('first-login', 'admin-assisted', 'register')),
    ip          VARCHAR(64) NOT NULL DEFAULT '',
    user_agent  VARCHAR(512) NOT NULL DEFAULT '',
    UNIQUE (user_id, doc_type, doc_version)
);

CREATE INDEX IF NOT EXISTS idx_user_consents_user ON user_consents(user_id);

-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS user_consents CASCADE;
