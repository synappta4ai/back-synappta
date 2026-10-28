-- +goose Up
-- +goose StatementBegin

-- ─── Master recommendation tags + metadata ─────────────────────
-- La marca de recomendación pasa de boolean a una o varias etiquetas
-- (p.ej. 'eficiente', 'barato') y cada fila suma metadata libre
-- clave-valor (p.ej. costo, notas internas). Las filas marcadas con el
-- boolean viejo heredan la etiqueta 'recomendado' para no perder la
-- curaduría existente.
ALTER TABLE model_master_prefs
  ADD COLUMN IF NOT EXISTS recommend_tags TEXT[] NOT NULL DEFAULT '{}',
  ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}';

UPDATE model_master_prefs
  SET recommend_tags = ARRAY['recomendado']
  WHERE recommended = TRUE AND recommend_tags = '{}';

ALTER TABLE model_master_prefs DROP COLUMN IF EXISTS recommended;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE model_master_prefs
  ADD COLUMN IF NOT EXISTS recommended BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE model_master_prefs
  SET recommended = TRUE
  WHERE recommend_tags <> '{}';

ALTER TABLE model_master_prefs DROP COLUMN IF EXISTS recommend_tags;
ALTER TABLE model_master_prefs DROP COLUMN IF EXISTS metadata;

-- +goose StatementEnd
