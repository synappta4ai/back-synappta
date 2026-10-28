-- +goose Up
-- +goose StatementBegin

-- ─── User notifications (centro nativo de avisos) ─────────────
-- Registro persistente por usuario de los eventos que también salen por
-- push/canales (video listo/fallido): alimenta la campana nativa de la app
-- y respeta el mismo opt-in (si el usuario apaga los avisos, no se escribe).
CREATE TABLE IF NOT EXISTS user_notifications (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    BIGINT NOT NULL DEFAULT 0,
    type       VARCHAR(32) NOT NULL DEFAULT '',
    title      TEXT NOT NULL DEFAULT '',
    body       TEXT NOT NULL DEFAULT '',
    task_id    TEXT NOT NULL DEFAULT '',
    read_at    TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_user_notifications_user
    ON user_notifications(user_id, created_at DESC);

-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS user_notifications;
