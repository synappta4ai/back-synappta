package workerbridge

import (
	"context"
	"database/sql"
	"encoding/json"
)

// ─── Persistencia del monitor de salud ─────────────────────────

// ActiveTenantIDs lista los tenants activos: el monitor recorre todos porque
// las GPUs de cualquier equipo pueden caerse sin que nadie esté mirando.
func (s *Store) ActiveTenantIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM tenants WHERE active ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AlertRecipients son los admins activos del tenant: los que pueden hacer algo
// con una GPU caída (el resto del equipo sólo ve el trabajo en cola).
func (s *Store) AlertRecipients(ctx context.Context, tenantID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id
		FROM tenant_memberships m
		JOIN users u ON u.id = m.user_id
		WHERE m.tenant_id = $1
		  AND m.role_level <= 1
		  AND u.active
		  AND u.deleted_at IS NULL
		ORDER BY u.id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AlertCandidates devuelve el estado de las GPUs del tenant (una fila por
// máquina) con lo que el monitor necesita para decidir si algo está roto.
func (s *Store) AlertCandidates(ctx context.Context, tenantID int64) ([]WorkerHealth, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT w.id, w.name, COALESCE(w.gpu_name, ''), COALESCE(w.version, ''),
		       w.last_heartbeat, COALESCE(w.disk_free_mb, 0),
		       w.update_failure::text, w.paused,
		       COALESCE((
		           SELECT COUNT(*) = $2 AND bool_and(j.status = 'failed')
		           FROM (SELECT status FROM worker_jobs
		                 WHERE worker_id = w.id
		                   AND status IN ('completed', 'failed', 'cancelled')
		                 ORDER BY updated_at DESC
		                 LIMIT $2) j), FALSE) AS failed_streak,
		       COALESCE((
		           SELECT error_message FROM worker_jobs
		           WHERE worker_id = w.id AND status = 'failed'
		           ORDER BY updated_at DESC
		           LIMIT 1), '') AS last_job_error
		FROM worker_registry w
		WHERE w.tenant_id = $1
		ORDER BY w.created_at`, tenantID, failedJobStreak)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkerHealth{}
	for rows.Next() {
		var w WorkerHealth
		var heartbeat sql.NullTime
		var failureJSON *string
		if err := rows.Scan(&w.ID, &w.Name, &w.GPUName, &w.Version, &heartbeat,
			&w.DiskFreeMB, &failureJSON, &w.Paused, &w.FailedStreak, &w.LastJobError); err != nil {
			return nil, err
		}
		if heartbeat.Valid {
			w.LastHeartbeat = heartbeat.Time
		}
		if failureJSON != nil && *failureJSON != "null" {
			var failure UpdateFailure
			if err := json.Unmarshal([]byte(*failureJSON), &failure); err == nil && failure.ID != "" {
				w.UpdateFailure = &failure
			}
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// OpenAlertEpisode abre el episodio de esa alerta y devuelve true sólo si es
// nuevo: con la misma clave la alerta ya se mandó y no se repite. La clave
// cambia cuando es otro evento (otro id de actualización fallida).
func (s *Store) OpenAlertEpisode(ctx context.Context, workerID, kind, key string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO worker_alert_state (worker_id, kind, episode_key, opened_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (worker_id, kind) DO UPDATE
		    SET episode_key = EXCLUDED.episode_key, opened_at = NOW()
		    WHERE worker_alert_state.episode_key IS DISTINCT FROM EXCLUDED.episode_key`,
		workerID, kind, key)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CloseAlertEpisode marca la alerta como resuelta: la GPU volvió a estar sana.
// El próximo fallo abre un episodio nuevo y vuelve a avisar.
func (s *Store) CloseAlertEpisode(ctx context.Context, workerID, kind string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM worker_alert_state WHERE worker_id = $1 AND kind = $2`,
		workerID, kind)
	return err
}
