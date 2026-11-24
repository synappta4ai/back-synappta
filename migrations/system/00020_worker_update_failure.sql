-- +goose Up
-- +goose StatementBegin

-- ─── Resultado explícito de la actualización remota ────────────
-- El agente reporta con failed_update el motivo real (falló la descarga, el
-- hash, la extracción, las deps o el arranque) y el proceso reiniciado lo
-- manda en su primer heartbeat. Antes el panel deducía el fallo del reloj
-- (5 min desde el pedido), lo que marcaba «falló» mientras el worker esperaba
-- legítimamente a que terminara un job largo.
--
-- Guarda {id, error, at} hasta que el admin encola otra actualización (o el
-- watchdog del reinicio la marca cuando el worker no vuelve).
ALTER TABLE worker_registry
    ADD COLUMN IF NOT EXISTS update_failure JSONB;

-- +goose StatementEnd

-- +goose Down
ALTER TABLE worker_registry DROP COLUMN IF EXISTS update_failure;
