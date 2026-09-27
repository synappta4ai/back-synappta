-- +goose Up
-- +goose StatementBegin
-- Centralización de recursos por proyecto (evento): un recurso de la
-- biblioteca (files) puede pertenecer a cero o muchos proyectos. Los recursos
-- sin asignación quedan como biblioteca global; los generados Studio siguen
-- autoprovisionando su proyecto "Studio".

CREATE TABLE event_files (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id   UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    file_id    UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(event_id, file_id)
);

CREATE INDEX idx_event_files_event ON event_files(event_id);
CREATE INDEX idx_event_files_file ON event_files(file_id);

-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS event_files CASCADE;
