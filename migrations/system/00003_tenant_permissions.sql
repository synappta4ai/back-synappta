-- +goose Up
-- +goose StatementBegin
-- Permisos base de la empresa: se aplican a TODOS sus usuarios (piso común).
-- Los permisos por membresía (00002) actúan como extras encima de estos.
ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS permissions TEXT[] NOT NULL DEFAULT '{}';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE tenants
    DROP COLUMN IF EXISTS permissions;
-- +goose StatementEnd
