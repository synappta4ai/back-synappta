-- +goose Up
-- +goose StatementBegin

-- Estado del flujo /agency por (usuario, agente): el usuario retoma donde
-- quedó y cada agente recuerda lo suyo por separado. "state" guarda el
-- flujo completo: {step, brief, artifacts: {…}, conversation_id}.
-- Ver docs/plan-flujos-agentes.md §2.4.
CREATE TABLE agency_flows (
    id          TEXT PRIMARY KEY,
    user_id     BIGINT NOT NULL,
    agent       VARCHAR(31) NOT NULL,       -- commercial | real_estate | cinema
    project_id  UUID REFERENCES events(id) ON DELETE SET NULL,
    state       JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Un flujo activo por usuario y agente: el upsert del back reutiliza la
-- fila en vez de acumular historial (la lectura es siempre la última
-- versión del estado).
CREATE UNIQUE INDEX uq_agency_flows_user_agent ON agency_flows (user_id, agent);

-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS agency_flows;
