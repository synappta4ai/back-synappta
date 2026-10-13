// Package workerbridge implements the outbound worker bridge (Fase A del
// plan de proveedor distribuido): las máquinas de los usuarios se emparejan
// con Synapta mediante un código corto y después consultan trabajos por HTTP
// long-poll saliente — sin puertos de entrada, sin túneles, detrás del
// NAT/firewall del hogar y por el mismo 443 que la API.
//
// Estado en las tablas del schema de sistema (worker_registry,
// worker_pairing_codes, worker_jobs): el worker se autentica con su propio
// token (no con un JWT), así que token → tenant se resuelve con un lookup
// indexado en el schema de sistema.
package workerbridge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
)

// onlineWindow: un worker se considera online si su último heartbeat tiene
// menos de este lapso. El worker envía heartbeats cada ~20 s.
const onlineWindow = 75 * time.Second

// pairingCodeTTL: vida del código de emparejamiento (el usuario tarda
// segundos en pegarlo en la app; 10 min es holgado).
const pairingCodeTTL = 10 * time.Minute

// Errors del store, comparables con errors.Is.
var (
	ErrCodeExpired   = errors.New("pairing code expired")
	ErrCodeUsed      = errors.New("pairing code already used")
	ErrCodeNotFound  = errors.New("pairing code not found")
	ErrWorkerAuth    = errors.New("invalid worker token")
	ErrJobNotFound   = errors.New("job not found")
	ErrWrongWorker   = errors.New("job does not belong to this worker")
)

// Worker es una GPU registrada (una fila por máquina emparejada).
type Worker struct {
	ID            string          `json:"id"`
	TenantID      int64           `json:"-"`
	Name          string          `json:"name"`
	GPUName       string          `json:"gpu_name"`
	OS            string          `json:"os"`
	Version       string          `json:"version"`
	VRAMTotalMB   int             `json:"vram_total_mb"`
	VRAMFreeMB    int             `json:"vram_free_mb"`
	JobsActive    int             `json:"jobs_active"`
	Models        json.RawMessage `json:"models"`
	LastHeartbeat time.Time       `json:"last_heartbeat"`
	CreatedAt     time.Time       `json:"created_at"`
	// Online se deriva del último heartbeat (nunca se persiste).
	Online bool `json:"online"`
}

// Job es un trabajo encolado para los workers del tenant.
type Job struct {
	ID             string          `json:"id"`
	TenantID       int64           `json:"-"`
	TaskID         string          `json:"task_id"`
	WorkerID       string          `json:"worker_id,omitempty"`
	Payload        json.RawMessage `json:"payload"`
	Status         string          `json:"status"`
	Progress       float64         `json:"progress"`
	StreamStatus   string          `json:"stream_status"`
	ErrorMessage   string          `json:"error_message"`
	ArtifactName   string          `json:"artifact_name"`
	ArtifactURL    string          `json:"artifact_url"`
	CancelRequested bool           `json:"cancel_requested"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// Store persiste el registry, los códigos y la cola en el schema de sistema.
type Store struct{ db *sql.DB }

// NewStore construye el store sobre el pool del schema de sistema.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// ─── Tokens y códigos ──────────────────────────────────────────

// newToken genera un token opaco de 256 bits en base64url.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken hashea un token (o un código de emparejamiento) con SHA-256 hex.
// Solo se persisten hashes: un volcado de la DB no permite autenticar.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// newPairingCode genera un código de 6 dígitos (crypto/rand, no guessable
// con math/rand).
func newPairingCode() (string, error) {
	max := big.NewInt(1_000_000)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// ─── Pairing codes ─────────────────────────────────────────────

// CreatePairingCode emite un código nuevo para el tenant. Se guarda solo su
// hash; el código en claro se devuelve una única vez al caller (el admin).
func (s *Store) CreatePairingCode(ctx context.Context, tenantID, createdBy int64) (code string, expiresAt time.Time, err error) {
	for attempt := 0; attempt < 5; attempt++ {
		code, err = newPairingCode()
		if err != nil {
			return "", time.Time{}, err
		}
		expiresAt = time.Now().Add(pairingCodeTTL)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO worker_pairing_codes (code_hash, tenant_id, created_by, expires_at)
		VALUES ($1, $2, $3, $4)`,
		hashToken(code), tenantID, nullableInt64(createdBy), expiresAt)
		if err == nil {
			return code, expiresAt, nil
		}
		// Colisión de hash (6 dígitos, improbable): reintenta con otro código.
	}
	return "", time.Time{}, fmt.Errorf("could not allocate a pairing code: %w", err)
}

// ConsumePairingCode valida y consume el código en una sola operación
// atómica: dos workers que canjeen el mismo código no pueden pasar (el
// UPDATE condicional deja exactamente un ganador).
func (s *Store) ConsumePairingCode(ctx context.Context, code string) (tenantID int64, err error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE worker_pairing_codes
		SET consumed_at = NOW()
		WHERE code_hash = $1 AND consumed_at IS NULL AND expires_at > NOW()`,
		hashToken(code))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		// Existe y se consumió: el tenant_id sale del mismo row.
		if err := s.db.QueryRowContext(ctx,
			`SELECT tenant_id FROM worker_pairing_codes WHERE code_hash = $1`,
			hashToken(code)).Scan(&tenantID); err != nil {
			return 0, err
		}
		return tenantID, nil
	}
	// No se consumió: distinguir "no existe", "ya usado" y "vencido" para un
	// mensaje claro en la app.
	var consumed sql.NullTime
	var expires time.Time
	err = s.db.QueryRowContext(ctx,
		`SELECT expires_at, consumed_at FROM worker_pairing_codes WHERE code_hash = $1`,
		hashToken(code)).Scan(&expires, &consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrCodeNotFound
	}
	if err != nil {
		return 0, err
	}
	if consumed.Valid {
		return 0, ErrCodeUsed
	}
	return 0, ErrCodeExpired
}

// ─── Registry ──────────────────────────────────────────────────

// RegisterWorker canjea un código por un worker nuevo con token de larga
// duración. Devuelve el token en claro una sola vez; en la DB queda solo el
// hash.
func (s *Store) RegisterWorker(ctx context.Context, tenantID int64, name, gpu, goos, version string,
	vramTotal, vramFree int, models json.RawMessage,
) (worker *Worker, token string, err error) {
	token, err = newToken()
	if err != nil {
		return nil, "", err
	}
	var modelsText string
	if len(models) == 0 {
		modelsText = "[]"
	} else {
		modelsText = string(models)
	}
	w := &Worker{}
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO worker_registry
			(tenant_id, name, token_hash, gpu_name, os, version,
			 vram_total_mb, vram_free_mb, models, last_heartbeat)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, NOW())
		RETURNING id, tenant_id, name, gpu_name, os, version,
		          vram_total_mb, vram_free_mb, jobs_active, models::text,
		          last_heartbeat, created_at`,
		tenantID, name, hashToken(token), gpu, goos, version,
		vramTotal, vramFree, modelsText,
	).Scan(&w.ID, &w.TenantID, &w.Name, &w.GPUName, &w.OS, &w.Version,
		&w.VRAMTotalMB, &w.VRAMFreeMB, &w.JobsActive, &w.Models,
		&w.LastHeartbeat, &w.CreatedAt)
	if err != nil {
		return nil, "", fmt.Errorf("register worker: %w", err)
	}
	w.Online = true
	return w, token, nil
}

// WorkerByToken resuelve el worker autenticado por su token. El hash es el
// índice único: una sola query, sin comparación de secretos en código.
func (s *Store) WorkerByToken(ctx context.Context, token string) (*Worker, error) {
	return s.scanWorker(s.db.QueryRowContext(ctx, workerSelect+`
		WHERE token_hash = $1`, hashToken(token)))
}

// ListWorkers del tenant, más recientes primero. Online se deriva del
// heartbeat al momento de leer (nunca se persiste un flag).
func (s *Store) ListWorkers(ctx context.Context, tenantID int64) ([]Worker, error) {
	rows, err := s.db.QueryContext(ctx, workerSelect+`
		WHERE tenant_id = $1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Worker{}
	for rows.Next() {
		w, err := scanWorkerRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

// DeleteWorker revoca la GPU (borra el registry row; la cola de jobs del
// worker queda huérfana y el reconciler la marca failed).
func (s *Store) DeleteWorker(ctx context.Context, tenantID int64, workerID string) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM worker_registry WHERE id = $1 AND tenant_id = $2`,
		workerID, tenantID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrWorkerAuth
	}
	return nil
}

// Heartbeat actualiza el estado vivo del worker y devuelve los ids de jobs
// cancelados desde la última consulta, para que el agente local aborte su
// generación en curso.
func (s *Store) Heartbeat(ctx context.Context, workerID string,
	gpu, version string, vramFree, jobsActive int, models json.RawMessage,
) ([]string, error) {
	var modelsText string
	if len(models) == 0 {
		modelsText = "[]"
	} else {
		modelsText = string(models)
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE worker_registry
		SET last_heartbeat = NOW(), gpu_name = $2, version = $3,
		    vram_free_mb = $4, jobs_active = $5, models = $6::jsonb
		WHERE id = $1`,
		workerID, gpu, version, vramFree, jobsActive, modelsText)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT task_id FROM worker_jobs
		WHERE worker_id = $1 AND status = 'running' AND cancel_requested`,
		workerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ─── Cola de jobs ──────────────────────────────────────────────

// Enqueue agrega un trabajo a la cola del tenant. task_id es el id del
// generator (syn_*), único por tenant: re-encolar el mismo task reemplaza
// el row (idempotente ante reintentos del reconciler).
func (s *Store) Enqueue(ctx context.Context, tenantID int64, taskID string, payload json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO worker_jobs (tenant_id, task_id, payload)
		VALUES ($1, $2, $3::jsonb)
		ON CONFLICT (task_id) DO UPDATE
		SET payload = EXCLUDED.payload, status = 'queued',
		    updated_at = NOW()`,
		tenantID, taskID, string(payload))
	return err
}

// Claim entrega el próximo job encolado al worker que lo pide.
//
// FOR UPDATE SKIP LOCKED es el patrón correcto para colas en Postgres: dos
// workers que reclamen a la vez nunca ven la misma fila y no se bloquean
// entre sí. Devuelve (nil, nil) cuando la cola está vacía (el long-poll del
// handler reintenta hasta su timeout).
func (s *Store) Claim(ctx context.Context, workerID string, tenantID int64) (*Job, error) {
	row := s.db.QueryRowContext(ctx, `
		UPDATE worker_jobs SET worker_id = $1, status = 'running',
		       stream_status = 'SUBMITTED', updated_at = NOW()
		WHERE id = (
			SELECT id FROM worker_jobs
			WHERE tenant_id = $2 AND status = 'queued'
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING `+jobColumns, workerID, tenantID)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return job, err
}

// ReportProgress actualiza progreso/estado reportado por el worker.
func (s *Store) ReportProgress(ctx context.Context, workerID, taskID string,
	status string, progress float64, streamStatus, errMsg string,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE worker_jobs
		SET status = $3, progress = $4, stream_status = $5,
		    error_message = $6, updated_at = NOW()
		WHERE task_id = $1 AND worker_id = $2`,
		taskID, workerID, status, progress, streamStatus, errMsg)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrJobNotFound
	}
	return nil
}

// AttachArtifact linkea el artefacto subido con el job y lo marca completed.
// Idempotente: re-subir el mismo artefacto no duplica ni pisa la asociación
// si el job ya estaba completed con el mismo nombre.
func (s *Store) AttachArtifact(ctx context.Context, workerID, taskID, artifactName, artifactURL string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE worker_jobs
		SET status = 'completed', progress = 100, stream_status = 'COMPLETED',
		    artifact_name = $3, artifact_url = $4, updated_at = NOW()
		WHERE task_id = $1 AND worker_id = $2`,
		taskID, workerID, artifactName, artifactURL)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrJobNotFound
	}
	return nil
}

// RequestCancel marca un job para cancelación; la señal viaja en la
// siguiente respuesta de heartbeat al worker dueño.
func (s *Store) RequestCancel(ctx context.Context, tenantID int64, taskID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE worker_jobs SET cancel_requested = TRUE, updated_at = NOW()
		WHERE task_id = $1 AND tenant_id = $2 AND status IN ('queued', 'running')`,
		taskID, tenantID)
	return err
}

// JobByTaskID devuelve el job para el polling de estado del generator.
func (s *Store) JobByTaskID(ctx context.Context, tenantID int64, taskID string) (*Job, error) {
	job, err := scanJob(s.db.QueryRowContext(ctx, `
		SELECT `+jobColumns+` FROM worker_jobs
		WHERE task_id = $1 AND tenant_id = $2`, taskID, tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrJobNotFound
	}
	return job, err
}

// HasOnlineWorker informa si el tenant tiene al menos una GPU viva ahora
// (heartbeat dentro de la ventana). Lo usan el generator y el catálogo.
func (s *Store) HasOnlineWorker(ctx context.Context, tenantID int64) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM worker_registry
			WHERE tenant_id = $1 AND last_heartbeat > NOW() - INTERVAL '75 seconds')`,
		tenantID).Scan(&exists)
	return exists, err
}

// WorkerModel es un modelo reportado por un worker (snapshot de su catálogo).
type WorkerModel struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Mode      string `json:"mode"`
	Engine    string `json:"engine"`
	Pipeline  string `json:"pipeline"`
	Repo      string `json:"repo"`
	Steps     int    `json:"steps"`
	VRAMGb    int    `json:"vram_gb"`
	Family    string `json:"family"`
	Available bool   `json:"available"`
	Notes     string `json:"notes"`
}

// SnapshotsForTenant mergea los catálogos de las GPUs online del tenant.
// Es la fuente del catálogo "downloaded" en modo bridge: sin workers vivos,
// la lista sale vacía (nunca se inventan modelos sin GPU).
func (s *Store) SnapshotsForTenant(ctx context.Context, tenantID int64) ([]WorkerModel, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT models::text FROM worker_registry
		WHERE tenant_id = $1 AND last_heartbeat > NOW() - INTERVAL '75 seconds'
		ORDER BY created_at`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	out := []WorkerModel{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var models []WorkerModel
		if err := json.Unmarshal([]byte(raw), &models); err != nil {
			continue // snapshot corrupto de un worker: se ignora, no rompe la lista
		}
		for _, m := range models {
			if m.Key == "" || seen[strings.ToLower(m.Key)] {
				continue
			}
			seen[strings.ToLower(m.Key)] = true
			out = append(out, m)
		}
	}
	return out, rows.Err()
}

// FailOrphan marca un job running cuyo worker murió (heartbeat vencido) como
// failed: nada queda "running" para siempre. Lo usa el reconciler.
func (s *Store) FailOrphans(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE worker_jobs SET status = 'failed',
		       error_message = 'worker went offline',
		       updated_at = NOW()
		WHERE status = 'running'
		  AND (worker_id IS NULL OR worker_id NOT IN (
		      SELECT id FROM worker_registry
		      WHERE last_heartbeat > NOW() - INTERVAL '75 seconds'))`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ─── Scan helpers ──────────────────────────────────────────────

const workerSelect = `
	SELECT id, tenant_id, name, gpu_name, os, version,
	       vram_total_mb, vram_free_mb, jobs_active, models::text,
	       last_heartbeat, created_at
	FROM worker_registry `

const jobColumns = `
	id, tenant_id, task_id, COALESCE(worker_id::text, ''), payload::text, status,
	progress, stream_status, error_message, artifact_name, artifact_url,
	cancel_requested, created_at, updated_at`

type rowScanner interface{ Scan(...interface{}) error }

func (s *Store) scanWorker(row rowScanner) (*Worker, error) {
	w, err := scanWorkerRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWorkerAuth
	}
	return w, err
}

func scanWorkerRow(row rowScanner) (*Worker, error) {
	w := &Worker{}
	err := row.Scan(&w.ID, &w.TenantID, &w.Name, &w.GPUName, &w.OS, &w.Version,
		&w.VRAMTotalMB, &w.VRAMFreeMB, &w.JobsActive, &w.Models,
		&w.LastHeartbeat, &w.CreatedAt)
	if err != nil {
		return nil, err
	}
	w.Online = time.Since(w.LastHeartbeat) < onlineWindow
	return w, nil
}

func scanJob(row rowScanner) (*Job, error) {
	j := &Job{}
	err := row.Scan(&j.ID, &j.TenantID, &j.TaskID, &j.WorkerID, &j.Payload,
		&j.Status, &j.Progress, &j.StreamStatus, &j.ErrorMessage,
		&j.ArtifactName, &j.ArtifactURL, &j.CancelRequested,
		&j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return j, nil
}

// nullableID convierte un id de usuario en sql.NullInt64 para las FK
// opcionales (el created_by del código puede ser 0 en tests).
func nullableInt64(id int64) interface{} {
	if id <= 0 {
		return nil
	}
	return id
}

// uuidHasFormat sanity check para ids que llegan por URL.
func uuidHasFormat(raw string) bool {
	_, err := uuid.Parse(raw)
	return err == nil
}

// shortID recorta un id para logs (sin fallar si es más corto).
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
