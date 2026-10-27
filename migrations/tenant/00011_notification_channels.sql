-- +goose Up
-- +goose StatementBegin

-- ─── Notification channels (aviso de video listo) ─────────────
-- Destinos externos del tenant (Slack, Telegram, Discord, Gotify, ...) a
-- los que se postea cuando un video termina. La URL shoutrrr lleva el
-- secreto, así que se guarda cifrada (AES-256-GCM) y la API solo devuelve
-- máscara; el PUT preserva la guardada cuando viene vacía.
CREATE TABLE IF NOT EXISTS notification_channels (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       VARCHAR(128) NOT NULL DEFAULT '',
    service    VARCHAR(32) NOT NULL DEFAULT '',
    url        TEXT NOT NULL DEFAULT '',
    enabled    BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_notification_channels_enabled
    ON notification_channels(enabled);

-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS notification_channels;
