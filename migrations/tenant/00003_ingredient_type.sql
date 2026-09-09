-- +goose Up
-- +goose StatementBegin

-- Ingredient kind: character (talent), location or prop.
ALTER TABLE ingredients
    ADD COLUMN type VARCHAR(31) NOT NULL DEFAULT 'character';

CREATE INDEX idx_ingredients_type ON ingredients (type);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_ingredients_type;
ALTER TABLE ingredients DROP COLUMN IF EXISTS type;

-- +goose StatementEnd
