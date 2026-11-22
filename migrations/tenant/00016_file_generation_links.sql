-- +goose Up
-- +goose StatementBegin

-- Enlace entre un recurso de la biblioteca y la generación que lo produjo.
-- Permite que "Mis generaciones" muestre el proyecto al que el recurso fue
-- asignado después de guardarlo desde Herramientas.
CREATE TABLE file_generations (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    file_id    UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    task_id    VARCHAR(255) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (file_id, task_id)
);

CREATE INDEX idx_file_generations_task ON file_generations(task_id);
CREATE INDEX idx_file_generations_file ON file_generations(file_id);

-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS file_generations CASCADE;
