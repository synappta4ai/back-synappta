-- +goose Up
-- +goose StatementBegin
-- Permisos por membresía: lista de claves (p.ej. 'events.manage', '*' = todos).
-- Se resuelven por tenant: un mismo usuario puede tener permisos distintos en
-- cada empresa a la que pertenece.
ALTER TABLE tenant_memberships
    ADD COLUMN IF NOT EXISTS permissions TEXT[] NOT NULL DEFAULT '{}';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE tenant_memberships
    DROP COLUMN IF EXISTS permissions;
-- +goose StatementEnd
