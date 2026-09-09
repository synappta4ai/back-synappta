-- +goose Up
-- +goose StatementBegin

-- SHA-256 content hash for deduplication: re-uploading the same image returns
-- the existing file instead of creating a duplicate (unless forced).
-- '' = legacy rows uploaded before hashing; they never match.
ALTER TABLE files
    ADD COLUMN sha256 VARCHAR(64) NOT NULL DEFAULT '';

CREATE INDEX idx_files_sha256 ON files (sha256)
    WHERE sha256 <> '' AND deleted_at IS NULL AND trashed = FALSE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_files_sha256;
ALTER TABLE files DROP COLUMN IF EXISTS sha256;

-- +goose StatementEnd
