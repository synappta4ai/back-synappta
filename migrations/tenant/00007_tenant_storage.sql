-- +goose Up
-- +goose StatementBegin

-- Object storage (S3/MinIO) per tenant. One row per tenant (the singleton
-- constraint keeps it that way); without a row the tenant keeps using the
-- local disk under UPLOAD_DIR, so the back never depends on a central bucket.
-- Credentials are stored AES-256-GCM sealed with ENCRYPTION_KEY.
CREATE TABLE storage_config (
    singleton   BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    provider    VARCHAR(32) NOT NULL DEFAULT 's3',
    endpoint    TEXT NOT NULL DEFAULT '',
    bucket      VARCHAR(255) NOT NULL DEFAULT '',
    region      VARCHAR(64) NOT NULL DEFAULT 'us-east-1',
    access_key  TEXT NOT NULL DEFAULT '',   -- sealed
    secret_key  TEXT NOT NULL DEFAULT '',   -- sealed
    public_base TEXT NOT NULL DEFAULT '',
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS storage_config;
-- +goose StatementEnd
