-- +goose Up
-- +goose StatementBegin

-- ─── GPU providers (inference workers emparejados) ────────────
-- System schema (not per-tenant): el worker se autentica con su propio
-- token — sin JWT y sin claim de tenant — así que token → tenant debe
-- resolverse con un solo lookup indexado. El aislamiento se logra
-- filtrando siempre por tenant_id en cada query; nada acá es legible
-- de forma cross-tenant.

CREATE TABLE worker_registry (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name           VARCHAR(127) NOT NULL DEFAULT '',
    token_hash     VARCHAR(64) NOT NULL,
    gpu_name       VARCHAR(127) NOT NULL DEFAULT '',
    os             VARCHAR(63) NOT NULL DEFAULT '',
    version        VARCHAR(31) NOT NULL DEFAULT '',
    vram_total_mb  INT NOT NULL DEFAULT 0,
    vram_free_mb   INT NOT NULL DEFAULT 0,
    jobs_active    INT NOT NULL DEFAULT 0,
    -- Snapshot del catálogo propio (ListModels del worker), enviado en el
     -- register/heartbeat: el merge multi-worker (Fase C) lo lee en vivo.
    models         JSONB NOT NULL DEFAULT '[]'::jsonb,
    last_heartbeat TIMESTAMP WITH TIME ZONE,
    created_at     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX uq_worker_registry_token ON worker_registry (token_hash);
CREATE INDEX idx_worker_registry_tenant ON worker_registry (tenant_id);

-- ─── Pairing codes (cortos: canjeados por un token de larga duración) ──
-- Se guarda solo el hash del código: un volcado de la DB no permite
-- emparejar. TTL corto (minutos); canje único (consumed_at).
CREATE TABLE worker_pairing_codes (
    code_hash   VARCHAR(64) PRIMARY KEY,
    tenant_id   BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    created_by  BIGINT,
    expires_at  TIMESTAMP WITH TIME ZONE NOT NULL,
    consumed_at TIMESTAMP WITH TIME ZONE,
    created_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_worker_pairing_codes_tenant ON worker_pairing_codes (tenant_id);

-- ─── Job queue (la cola vive en el back; 1 job a la vez por worker) ──
-- El payload es la petición de generación completa (JSON): el worker la
-- ejecuta contra su servidor gRPC local y reporta progreso/artefacto acá.
CREATE TABLE worker_jobs (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    task_id          VARCHAR(63) NOT NULL,
    worker_id        UUID REFERENCES worker_registry(id) ON DELETE SET NULL,
    payload          JSONB NOT NULL,
    status           VARCHAR(15) NOT NULL DEFAULT 'queued', -- queued|running|completed|failed|cancelled
    progress         REAL NOT NULL DEFAULT 0,
    stream_status    VARCHAR(31) NOT NULL DEFAULT 'SUBMITTED',
    error_message    TEXT NOT NULL DEFAULT '',
    artifact_name    VARCHAR(255) NOT NULL DEFAULT '',
    artifact_url     TEXT NOT NULL DEFAULT '',
    cancel_requested BOOLEAN NOT NULL DEFAULT FALSE,
    created_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX uq_worker_jobs_task ON worker_jobs (task_id);
-- Claim (FOR UPDATE SKIP LOCKED) sobre la cola del tenant.
CREATE INDEX idx_worker_jobs_claim ON worker_jobs (tenant_id, created_at)
    WHERE status = 'queued';
-- Jobs activos de un worker (cancelaciones + reconciliación).
CREATE INDEX idx_worker_jobs_running ON worker_jobs (worker_id)
    WHERE status = 'running';

-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS worker_jobs;
DROP TABLE IF EXISTS worker_pairing_codes;
DROP TABLE IF EXISTS worker_registry;
