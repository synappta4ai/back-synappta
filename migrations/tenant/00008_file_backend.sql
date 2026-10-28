-- +goose Up
-- +goose StatementBegin

-- Where the bytes of each file live. Rows created before this column (and
-- tenants without a bucket) stay on 'local' under UPLOAD_DIR; files uploaded
-- while the tenant had its own bucket are 's3' and keep resolving there even
-- if the tenant later changes or removes the configuration.
ALTER TABLE files ADD COLUMN backend VARCHAR(16) NOT NULL DEFAULT 'local';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE files DROP COLUMN IF EXISTS backend;
-- +goose StatementEnd
