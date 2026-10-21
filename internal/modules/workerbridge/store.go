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
	ErrCodeExpired  = errors.New("pairing code expired")
	ErrCodeUsed     = errors.New("pairing code already used")
	ErrCodeNotFound = errors.New("pairing code not found")
	ErrWorkerAuth   = errors.New("invalid worker token")
	ErrJobNotFound  = errors.New("job not found")
	ErrWrongWorker  = errors.New("job does not belong to this worker")
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
	ID              string          `json:"id"`
	TenantID        int64           `json:"-"`
	TaskID          string          `json:"task_id"`
	WorkerID        string          `json:"worker_id,omitempty"`
	Payload         json.RawMessage `json:"payload"`
	Status          string          `json:"status"`
	Progress        float64         `json:"progress"`
	StreamStatus    string          `json:"stream_status"`
	ErrorMessage    string          `json:"error_message"`
	ArtifactName    string          `json:"artifact_name"`
	ArtifactURL     string          `json:"artifact_url"`
	CancelRequested bool            `json:"cancel_requested"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
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
	// models::text vuelve como string del driver: database/sql no sabe
	// escanearlo en *json.RawMessage (hace falta convertir a mano).
	var modelsJSON *string
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
		&w.VRAMTotalMB, &w.VRAMFreeMB, &w.JobsActive, &modelsJSON,
		&w.LastHeartbeat, &w.CreatedAt)
	if err != nil {
		return nil, "", fmt.Errorf("register worker: %w", err)
	}
	if modelsJSON != nil {
		w.Models = json.RawMessage(*modelsJSON)
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

// ─── Descargas de modelos (panel → worker) ─────────────────────

// RequestDownload encola la descarga de un modelo para el worker: el admin
// la pide y el agente la recoge en el próximo heartbeat. Idempotente por
// clave (re-pedir no duplica) y siempre con scope de tenant.
func (s *Store) RequestDownload(ctx context.Context, tenantID int64, workerID, key string) error {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 128 {
		return fmt.Errorf("invalid model key %q", key)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE worker_registry
		SET requested_downloads = (
			SELECT COALESCE(jsonb_agg(DISTINCT v), '[]'::jsonb)
			FROM (
				SELECT jsonb_array_elements_text(requested_downloads) AS v
				UNION SELECT $3
			) s
		), updated_at = NOW()
		WHERE id = $1 AND tenant_id = $2`,
		workerID, tenantID, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrWorkerAuth
	}
	return nil
}

// PendingDownloads lista las descargas pedidas aún no confirmadas.
func (s *Store) PendingDownloads(ctx context.Context, workerID string) ([]string, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(requested_downloads, '[]'::jsonb)::text FROM worker_registry WHERE id = $1`,
		workerID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return []string{}, nil
		}
		return nil, err
	}
	out := []string{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []string{}, nil // columna corrupta: no bloquear el heartbeat
	}
	return out, nil
}

// AckDownloads saca de la cola las descargas que el worker ya tomó.
func (s *Store) AckDownloads(ctx context.Context, workerID string, keys []string) error {
	clean := []string{}
	seen := map[string]bool{}
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		clean = append(clean, k)
	}
	if len(clean) == 0 {
		return nil
	}
	args := make([]interface{}, 0, len(clean)+1)
	args = append(args, workerID)
	holders := make([]string, 0, len(clean))
	for i, k := range clean {
		holders = append(holders, fmt.Sprintf("$%d", i+2))
		args = append(args, k)
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE worker_registry
		SET requested_downloads = (
			SELECT COALESCE(jsonb_agg(v), '[]'::jsonb)
			FROM jsonb_array_elements_text(requested_downloads) AS v
			WHERE v NOT IN (%s)
		), updated_at = NOW()
		WHERE id = $1`, strings.Join(holders, ",")), args...)
	return err
}

// ─── Cola de jobs ──────────────────────────────────────────────

// Enqueue agrega un trabajo a la cola del tenant. task_id es el id del
// generator (syn_*), único por tenant: re-encolar el mismo task reemplaza
// el row (idempotente ante reintentos del reconciler).
//
// workerID fija la GPU destino (la que declara el modelo con pesos): un job
// fijado sólo lo puede reclamar esa máquina. "" deja el job abierto a
// cualquier GPU del tenant.
func (s *Store) Enqueue(ctx context.Context, tenantID int64, taskID, workerID string, payload json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO worker_jobs (tenant_id, task_id, worker_id, payload)
		VALUES ($1, $2, NULLIF($3, '')::uuid, $4::jsonb)
		ON CONFLICT (task_id) DO UPDATE
		SET payload = EXCLUDED.payload, status = 'queued',
		    worker_id = EXCLUDED.worker_id, updated_at = NOW()`,
		tenantID, taskID, workerID, string(payload))
	return err
}

// Claim entrega el próximo job encolado al worker que lo pide. Sólo toma
// jobs sin dueño o fijados a ESE worker: un job fijado a la GPU que tiene los
// pesos no puede caer en otra máquina del tenant (fallaría sin el modelo).
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
			  AND (worker_id IS NULL OR worker_id = $1::uuid)
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
//
// El estado de pesos (available/downloading/download_percent/size_bytes) lo
// lee el agente de su gRPC local: viaja tal cual para que el panel sepa qué
// está realmente en disco y qué falta descargar. WorkerID/Name/Version no
// vienen en el JSON del agente: los agrega SnapshotsForTenant con la fila del
// registry que reportó el modelo, que es a quien se le pide la descarga.
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
	// Downloaded: los pesos completos están en el disco de ese worker. Ojo:
	// no es lo mismo que Available (un volumen desmontado o un motor caído
	// dejan el modelo no disponible con los pesos intactos).
	Downloaded bool `json:"downloaded,omitempty"`
	// Downloading: hay una descarga de pesos en curso para esta clave.
	Downloading bool `json:"downloading,omitempty"`
	// DownloadPercent: 0-100 del progreso de esa descarga.
	DownloadPercent float32 `json:"download_percent,omitempty"`
	// SizeBytes: tamaño de los pesos en disco (parcial mientras baja).
	SizeBytes int64 `json:"size_bytes,omitempty"`
	// DownloadError: último fallo de descarga, si hubo.
	DownloadError string `json:"download_error,omitempty"`
	// DownloadPath: ruta absoluta de los pesos en la máquina del worker (la
	// reporta su gRPC local). Vacía en agentes anteriores al campo o cuando el
	// modelo no tiene carpeta de pesos. Solo informativa: el back y el front
	// no la interpretan contra su propio disco.
	DownloadPath string `json:"download_path,omitempty"`
	// Identidad de la GPU que reportó el modelo (la completa el store).
	WorkerID      string `json:"worker_id,omitempty"`
	WorkerName    string `json:"worker_name,omitempty"`
	WorkerVersion string `json:"worker_version,omitempty"`
}

// SnapshotsForTenant mergea los catálogos de las GPUs online del tenant.
// Es la fuente del catálogo "downloaded" en modo bridge: sin workers vivos,
// la lista sale vacía (nunca se inventan modelos sin GPU). Cada modelo sale
// con la identidad de la GPU que lo reportó, que es la máquina a la que se le
// pide la descarga cuando falta y a la que se fija el trabajo al generar.
//
// Un mismo modelo puede estar declarado por varias máquinas del tenant. Gana
// la entrada más útil para el usuario, no la primera que llega: primero la que
// TIENE los pesos (si no, el modelo quedaba oculto por una GPU que sólo lo
// declara, y generar fallaba con "todavía no lo bajó"), después la que los
// está bajando (es la que puede completar la descarga) y, entre iguales, la
// primera por antigüedad — orden estable entre heartbeats.
func (s *Store) SnapshotsForTenant(ctx context.Context, tenantID int64) ([]WorkerModel, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, name, version, models::text FROM worker_registry
		WHERE tenant_id = $1 AND last_heartbeat > NOW() - INTERVAL '75 seconds'
		ORDER BY created_at`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	index := map[string]int{} // key (lower) → posición en out
	out := []WorkerModel{}
	for rows.Next() {
		var id, name, version, raw string
		if err := rows.Scan(&id, &name, &version, &raw); err != nil {
			return nil, err
		}
		var models []WorkerModel
		if err := json.Unmarshal([]byte(raw), &models); err != nil {
			continue // snapshot corrupto de un worker: se ignora, no rompe la lista
		}
		for _, m := range models {
			if m.Key == "" {
				continue
			}
			m.WorkerID, m.WorkerName, m.WorkerVersion = id, name, version
			key := strings.ToLower(m.Key)
			pos, seen := index[key]
			if !seen {
				index[key] = len(out)
				out = append(out, m)
				continue
			}
			if snapshotScore(m) > snapshotScore(out[pos]) {
				out[pos] = m // reemplaza al del worker anterior, conservando el orden
			}
		}
	}
	return out, rows.Err()
}

// snapshotScore prioriza, entre las máquinas que declaran el mismo modelo, la
// que realmente puede usarlo: pesos en disco > descarga en curso > indicios de
// haber intentado bajarlo > sólo declarado.
func snapshotScore(m WorkerModel) int {
	switch {
	case m.Available:
		return 3
	case m.Downloading:
		return 2
	case m.SizeBytes > 0 || m.DownloadError != "":
		return 1
	default:
		return 0
	}
}

// FailOrphans marca como failed los jobs que quedaron sin quien los ejecute:
// nada queda "running" para siempre. Lo usa el reconciler.
//
// Dos casos, ambos por heartbeat vencido (75 s):
//   - running: el worker murió a mitad de la generación.
//   - queued: ninguna GPU del tenant puede tomarlo — la máquina a la que está
//     fijado se desconectó antes del claim (o se revocó: la FK lo deja sin
//     dueño y no queda ninguna GPU viva). La gracia de 2 minutos evita fallar
//     un job que está por reclamarse (claim long-poll 25 s + heartbeat 20 s).
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
	total, _ := res.RowsAffected()

	// Queued sin ejecutor posible: el mensaje nombra la causa real (la GPU que
	// tenía el modelo no llegó a tomarlo), no un genérico "worker offline".
	res, err = s.db.ExecContext(ctx, `
		UPDATE worker_jobs SET status = 'failed',
		       error_message = 'the GPU that has this model went offline before taking the job',
		       updated_at = NOW()
		WHERE status = 'queued'
		  AND updated_at < NOW() - INTERVAL '2 minutes'
		  AND NOT EXISTS (
		      SELECT 1 FROM worker_registry w
		      WHERE w.tenant_id = worker_jobs.tenant_id
		        AND w.last_heartbeat > NOW() - INTERVAL '75 seconds'
		        AND (worker_jobs.worker_id IS NULL OR w.id = worker_jobs.worker_id))`)
	if err != nil {
		return total, err
	}
	n, _ := res.RowsAffected()
	return total + n, nil
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
	var modelsJSON *string // models::text → string del driver (ver RegisterWorker)
	err := row.Scan(&w.ID, &w.TenantID, &w.Name, &w.GPUName, &w.OS, &w.Version,
		&w.VRAMTotalMB, &w.VRAMFreeMB, &w.JobsActive, &modelsJSON,
		&w.LastHeartbeat, &w.CreatedAt)
	if err != nil {
		return nil, err
	}
	if modelsJSON != nil {
		w.Models = json.RawMessage(*modelsJSON)
	}
	w.Online = time.Since(w.LastHeartbeat) < onlineWindow
	return w, nil
}

func scanJob(row rowScanner) (*Job, error) {
	j := &Job{}
	// payload::text vuelve como string del driver: database/sql no sabe
	// escanearlo en *json.RawMessage ("unsupported Scan"), así que se convierte
	// a mano. Este detalle rompía TODOS los claims: el worker recibía un error
	// de scan en vez de un trabajo.
	var payloadJSON *string
	err := row.Scan(&j.ID, &j.TenantID, &j.TaskID, &j.WorkerID, &payloadJSON,
		&j.Status, &j.Progress, &j.StreamStatus, &j.ErrorMessage,
		&j.ArtifactName, &j.ArtifactURL, &j.CancelRequested,
		&j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if payloadJSON != nil {
		j.Payload = json.RawMessage(*payloadJSON)
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
