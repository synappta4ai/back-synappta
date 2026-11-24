-- +goose Up
-- +goose StatementBegin

-- ─── Estado de las alertas de salud de GPUs ────────────────────
-- Una fila = un episodio abierto (worker + tipo de alerta). El monitor avisa
-- sólo cuando abre la fila; cuando la condición se cura la borra, así el
-- próximo fallo vuelve a avisar. episode_key distingue eventos del mismo tipo
-- (por ejemplo, dos actualizaciones fallidas distintas).
CREATE TABLE IF NOT EXISTS worker_alert_state (
    worker_id   UUID NOT NULL REFERENCES worker_registry(id) ON DELETE CASCADE,
    kind        VARCHAR(32) NOT NULL,
    episode_key VARCHAR(64) NOT NULL DEFAULT '',
    opened_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (worker_id, kind)
);

-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS worker_alert_state;
