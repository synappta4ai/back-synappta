-- +goose Up
-- +goose StatementBegin
-- Gasto de la API del proveedor (Higgsfield) por generacion:
--   cost_credits            = creditos cobrados/estimados por Higgsfield (su unidad de cobro)
--   provider_transaction_id = "Transaction ID" con el que la generacion aparece
--                             en la consola de Higgsfield (su request_id)
-- El monto en USD sigue viniendo en estimated_cost/cost_source
-- (cost_source = 'provider_estimate' cuando lo informa la API del proveedor).

ALTER TABLE generation_logs
    ADD COLUMN cost_credits DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN provider_transaction_id VARCHAR(255) NOT NULL DEFAULT '';

-- +goose StatementEnd

-- +goose Down
ALTER TABLE generation_logs
    DROP COLUMN IF EXISTS provider_transaction_id,
    DROP COLUMN IF EXISTS cost_credits;
