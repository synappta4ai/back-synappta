-- +goose Up
-- +goose StatementBegin

-- ─── User VAPID keys (push por usuario) ─────────────────────────
-- Cada usuario tiene su propio par VAPID, generado automáticamente la
-- primera vez que configura push (GET /push/public-key lo crea si falta).
-- La privada se guarda cifrada (AES-256-GCM, misma llave que credenciales
-- y canales); el fallback global PUSH_VAPID_* sigue firmando los envíos a
-- suscripciones viejas creadas con la clave global.
CREATE TABLE IF NOT EXISTS user_vapid_keys (
    user_id            BIGINT PRIMARY KEY,
    public_key         TEXT NOT NULL DEFAULT '',
    private_key_sealed TEXT NOT NULL DEFAULT '',
    subject            TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS user_vapid_keys;
