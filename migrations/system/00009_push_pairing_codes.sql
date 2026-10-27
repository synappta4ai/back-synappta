-- +goose Up
-- +goose StatementBegin

-- ─── Push pairing codes (QR subscribe) ────────────────────────
-- Códigos de un solo uso para vincular un dispositivo escaneando un QR:
-- el teléfono abre /pair?code=… y registra su push subscription sin login.
-- Tabla de sistema (el redeem es público y resuelve el pool por slug).
CREATE TABLE IF NOT EXISTS push_pairing_codes (
    code       TEXT PRIMARY KEY,
    tenant_slug VARCHAR(128) NOT NULL DEFAULT '',
    user_id    BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    used_at    TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_push_pairing_codes_expiry
    ON push_pairing_codes(expires_at);

-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS push_pairing_codes;
