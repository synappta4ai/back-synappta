package workerbridge

// Store tests against a real Postgres. La semántica de la cola vive en SQL
// (pin por worker, FOR UPDATE SKIP LOCKED, ventana de heartbeat), y un fake no
// la puede probar: estos tests corren contra una base real y se saltean si no
// hay DSN.
//
//	BM_TEST_DATABASE_URL="$DATABASE_URL" go test ./internal/modules/workerbridge/ -run TestStore -v
//
// Cada test crea su propio tenant efímero (los FKs cascadean a su registry y
// su cola) y lo borra al terminar: no toca datos de otros tenants.
import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func testStore(t *testing.T) (*Store, int64) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("BM_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("BM_TEST_DATABASE_URL not set: skipping Postgres-backed store tests")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Fatalf("ping db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	slug := fmt.Sprintf("bridge-store-test-%d", time.Now().UnixNano())
	var tenantID int64
	if err := db.QueryRowContext(ctx,
		`INSERT INTO tenants (slug, name) VALUES ($1, $2) RETURNING id`,
		slug, "bridge store test").Scan(&tenantID); err != nil {
		t.Fatalf("create test tenant: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID); err != nil {
			t.Errorf("cleanup tenant %d: %v", tenantID, err)
		}
	})
	return NewStore(db), tenantID
}

// addWorker registra una GPU del tenant con el heartbeat desplazado en el
// tiempo (heartbeatAgo > 75s la deja offline sin esperar) y el snapshot de su
// catálogo tal como lo manda el agente.
func addWorker(t *testing.T, st *Store, tenantID int64, name string, heartbeatAgo time.Duration, models ...string) string {
	t.Helper()
	snapshot := "[]"
	if len(models) > 0 && strings.TrimSpace(models[0]) != "" {
		snapshot = models[0]
	}
	var id string
	err := st.db.QueryRow(`
		INSERT INTO worker_registry
			(tenant_id, name, token_hash, gpu_name, os, version, models, last_heartbeat)
		VALUES ($1, $2, $3, 'RTX 4090', 'windows', '0.2.5', $4::jsonb,
		        NOW() - ($5 * INTERVAL '1 second'))
		RETURNING id`,
		tenantID, name,
		hashToken(fmt.Sprintf("%s-%d", name, time.Now().UnixNano())),
		snapshot, int64(heartbeatAgo.Seconds())).Scan(&id)
	if err != nil {
		t.Fatalf("add worker %s: %v", name, err)
	}
	return id
}

func enqueue(t *testing.T, st *Store, tenantID int64, taskID, workerID string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"jobId": taskID, "modelName": "SDXL-Turbo"})
	if err := st.Enqueue(context.Background(), tenantID, taskID, workerID, payload); err != nil {
		t.Fatalf("enqueue %s: %v", taskID, err)
	}
}

// backdateOldJob mueve el reloj del job: la reconciliación de jobs "queued"
// espera 2 minutos de gracia antes de darlos por perdidos.
func backdateJob(t *testing.T, st *Store, taskID string, ago time.Duration) {
	t.Helper()
	if _, err := st.db.Exec(
		`UPDATE worker_jobs SET updated_at = NOW() - ($2 * INTERVAL '1 second') WHERE task_id = $1`,
		taskID, int64(ago.Seconds())); err != nil {
		t.Fatalf("backdate job %s: %v", taskID, err)
	}
}

func jobStatus(t *testing.T, st *Store, tenantID int64, taskID string) *Job {
	t.Helper()
	job, err := st.JobByTaskID(context.Background(), tenantID, taskID)
	if err != nil {
		t.Fatalf("job %s: %v", taskID, err)
	}
	return job
}

// El pin evita que otra GPU del tenant tome un trabajo para el que no tiene
// los pesos (dentro del worker fallaría sin el modelo).
func TestStoreClaimHonorsPin(t *testing.T) {
	st, tenant := testStore(t)
	withWeights := addWorker(t, st, tenant, "PC-Casa", 0)
	other := addWorker(t, st, tenant, "Notebook", 0)

	enqueue(t, st, tenant, "task-pinned", withWeights)

	if job, err := st.Claim(context.Background(), other, tenant); err != nil || job != nil {
		t.Fatalf("worker without the weights claimed a pinned job: %+v (%v)", job, err)
	}

	job, err := st.Claim(context.Background(), withWeights, tenant)
	if err != nil || job == nil {
		t.Fatalf("pinned worker got no job: %+v (%v)", job, err)
	}
	if job.TaskID != "task-pinned" || job.WorkerID != withWeights || job.Status != "running" {
		t.Fatalf("claimed %+v, want task-pinned running for %s", job, withWeights)
	}

	// Reclamado una vez, no se entrega de nuevo.
	if again, err := st.Claim(context.Background(), other, tenant); err != nil || again != nil {
		t.Fatalf("job claimed twice: %+v (%v)", again, err)
	}
}

// Un job sin pin (\"cualquier GPU del tenant\") sigue siendo reclamable.
func TestStoreClaimTakesUnpinnedJob(t *testing.T) {
	st, tenant := testStore(t)
	w := addWorker(t, st, tenant, "PC-Casa", 0)

	enqueue(t, st, tenant, "task-open", "")

	job, err := st.Claim(context.Background(), w, tenant)
	if err != nil || job == nil {
		t.Fatalf("unpinned job not claimed: %+v (%v)", job, err)
	}
	if job.TaskID != "task-open" {
		t.Fatalf("claimed %+v, want task-open", job)
	}
	// El payload viaja al worker tal cual se encoló: un scan que devuelva el
	// JSON vacío o roto deja a la GPU sin nada que ejecutar.
	var payload map[string]string
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v (%q)", err, job.Payload)
	}
	if payload["jobId"] != "task-open" || payload["modelName"] != "SDXL-Turbo" {
		t.Fatalf("payload round-trip lost fields: %v", payload)
	}
}

// Re-encolar el mismo task (reintento del reconciler) refresca el pin.
func TestStoreEnqueueRepinsOnRetry(t *testing.T) {
	st, tenant := testStore(t)
	first := addWorker(t, st, tenant, "PC-Casa", 0)
	second := addWorker(t, st, tenant, "Notebook", 0)

	enqueue(t, st, tenant, "task-x", first)
	enqueue(t, st, tenant, "task-x", second)

	if job, err := st.Claim(context.Background(), first, tenant); err != nil || job != nil {
		t.Fatalf("stale pin still claimed by the old worker: %+v (%v)", job, err)
	}
	if job, err := st.Claim(context.Background(), second, tenant); err != nil || job == nil {
		t.Fatalf("re-pinned job not claimed by the new worker: %+v (%v)", job, err)
	}
}

// Sólo el worker dueño del job puede reportar su progreso: el estado que llega
// a la app no se puede falsear desde otra máquina del tenant.
func TestStoreReportProgressIsOwnerScoped(t *testing.T) {
	st, tenant := testStore(t)
	owner := addWorker(t, st, tenant, "PC-Casa", 0)
	other := addWorker(t, st, tenant, "Notebook", 0)

	enqueue(t, st, tenant, "task-p", owner)
	if _, err := st.Claim(context.Background(), owner, tenant); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if err := st.ReportProgress(context.Background(), other, "task-p", "running", 99, "GENERATING", ""); err != ErrJobNotFound {
		t.Fatalf("progress from a foreign worker: err=%v, want ErrJobNotFound", err)
	}
	if err := st.ReportProgress(context.Background(), owner, "task-p", "running", 42.5, "GENERATING", ""); err != nil {
		t.Fatalf("owner report: %v", err)
	}

	job := jobStatus(t, st, tenant, "task-p")
	if job.Progress != 42.5 || job.StreamStatus != "GENERATING" || job.Status != "running" {
		t.Fatalf("progress not stored: %+v", job)
	}
}

// Varias GPUs del tenant pueden declarar el mismo modelo. El snapshot que ve
// la app (y con el que se fija el trabajo) debe ser el de la máquina que
// puede correrlo: si ganara la que sólo lo declara, el modelo quedaría oculto
// en las listas y la generación fallaría con "todavía no lo bajó".
func TestStoreSnapshotsPreferWorkerThatCanRunTheModel(t *testing.T) {
	st, tenant := testStore(t)
	// Registrada primero, sin pesos: es la que "ganaba" antes.
	declaresOnly := addWorker(t, st, tenant, "PC vieja", 5*time.Second,
		`[{"key":"SDXL-Turbo","mode":"image","family":"sdxl","available":false,"size_bytes":100}]`)
	hasWeights := addWorker(t, st, tenant, "PC nueva", 5*time.Second,
		`[{"key":"SDXL-Turbo","mode":"image","family":"sdxl","available":true,"size_bytes":6900000000}]`)
	// Sin pesos pero bajando: esa máquina es la que puede completar la descarga.
	downloading := addWorker(t, st, tenant, "Notebook", 5*time.Second,
		`[{"key":"FLUX.1-dev","mode":"image","family":"flux","available":false,"downloading":true,"download_percent":42}]`)

	snaps, err := st.SnapshotsForTenant(context.Background(), tenant)
	if err != nil {
		t.Fatalf("snapshots: %v", err)
	}
	byKey := map[string]WorkerModel{}
	for _, s := range snaps {
		byKey[s.Key] = s
	}
	if len(snaps) != 2 {
		t.Fatalf("merged snapshots = %+v, want one entry per model key", snaps)
	}
	sdxl, ok := byKey["SDXL-Turbo"]
	if !ok || !sdxl.Available || sdxl.WorkerID != hasWeights {
		t.Fatalf("SDXL-Turbo = %+v, want the GPU with the weights (%s)", sdxl, hasWeights)
	}
	if sdxl.WorkerID == declaresOnly {
		t.Fatalf("the GPU without weights shadowed the one that has them")
	}
	flux, ok := byKey["FLUX.1-dev"]
	if !ok || !flux.Downloading || flux.WorkerID != downloading {
		t.Fatalf("FLUX.1-dev = %+v, want the downloading GPU (%s)", flux, downloading)
	}

	// Y el trabajo se fija a esa misma máquina: es a la que se le pide ejecutar.
	enqueue(t, st, tenant, "task-avail", sdxl.WorkerID)
	if job, err := st.Claim(context.Background(), declaresOnly, tenant); err != nil || job != nil {
		t.Fatalf("job taken by the GPU without weights: %+v (%v)", job, err)
	}
	if job, err := st.Claim(context.Background(), hasWeights, tenant); err != nil || job == nil {
		t.Fatalf("job not taken by the GPU with weights: %+v (%v)", job, err)
	}
}

// La GPU que tenía los pesos se desconectó antes de tomar el job: la app no
// puede quedar esperando para siempre a una máquina que ya no está.
func TestStoreFailOrphansFailsQueuedJobPinnedToOfflineWorker(t *testing.T) {
	st, tenant := testStore(t)
	gone := addWorker(t, st, tenant, "PC-Casa", 10*time.Minute)

	enqueue(t, st, tenant, "task-stale", gone)
	backdateJob(t, st, "task-stale", 3*time.Minute)

	if _, err := st.FailOrphans(context.Background()); err != nil {
		t.Fatalf("FailOrphans: %v", err)
	}

	job := jobStatus(t, st, tenant, "task-stale")
	if job.Status != "failed" || !strings.Contains(job.ErrorMessage, "went offline") {
		t.Fatalf("stale pinned job = %+v, want failed with the GPU reason", job)
	}
}

// Con la GPU viva, un job encolado no se toca: la gracia de 2 minutos existe
// para no matar trabajos que están por reclamarse.
func TestStoreFailOrphansKeepsQueuedJobWithLiveWorker(t *testing.T) {
	st, tenant := testStore(t)
	alive := addWorker(t, st, tenant, "PC-Casa", 5*time.Second)

	enqueue(t, st, tenant, "task-waiting", alive)
	backdateJob(t, st, "task-waiting", 3*time.Minute)

	if _, err := st.FailOrphans(context.Background()); err != nil {
		t.Fatalf("FailOrphans: %v", err)
	}

	job := jobStatus(t, st, tenant, "task-waiting")
	if job.Status != "queued" {
		t.Fatalf("queued job with a live worker = %+v, want untouched", job)
	}
	// Y sigue siendo reclamable.
	if got, err := st.Claim(context.Background(), alive, tenant); err != nil || got == nil {
		t.Fatalf("queued job not claimable: %+v (%v)", got, err)
	}
}

// Un job running cuyo worker murió a mitad de la generación se marca failed
// (comportamiento previo, ahora conviviendo con la reconciliación de queued).
func TestStoreFailOrphansFailsRunningJobOfDeadWorker(t *testing.T) {
	st, tenant := testStore(t)
	dead := addWorker(t, st, tenant, "PC-Casa", 10*time.Minute)

	enqueue(t, st, tenant, "task-running", dead)
	if _, err := st.Claim(context.Background(), dead, tenant); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.FailOrphans(context.Background()); err != nil {
		t.Fatalf("FailOrphans: %v", err)
	}
	if job := jobStatus(t, st, tenant, "task-running"); job.Status != "failed" {
		t.Fatalf("orphan running job = %+v, want failed", job)
	}
}

// Las descargas pedidas desde el panel siguen el camino heartbeat: se encolan
// y se sacan cuando el agente las confirma.
func TestStorePendingDownloadsRoundTrip(t *testing.T) {
	st, tenant := testStore(t)
	w := addWorker(t, st, tenant, "PC-Casa", 0)
	ctx := context.Background()

	if err := st.RequestDownload(ctx, tenant, w, "SDXL-Turbo"); err != nil {
		t.Fatalf("request download: %v", err)
	}
	if err := st.RequestDownload(ctx, tenant, w, "SDXL-Turbo"); err != nil {
		t.Fatalf("re-request download: %v", err)
	}
	pending, err := st.PendingDownloads(ctx, w)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0] != "SDXL-Turbo" {
		t.Fatalf("pending = %v, want one SDXL-Turbo", pending)
	}
	if err := st.AckDownloads(ctx, w, pending); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if pending, _ := st.PendingDownloads(ctx, w); len(pending) != 0 {
		t.Fatalf("pending after ack = %v, want none", pending)
	}
}

// El borrado de pesos recorre el mismo camino que la descarga: el admin lo
// encola, el heartbeat lo entrega y el agente lo confirma al ejecutarlo. Sin
// esta cola una máquina emparejada no podía liberar disco desde el panel.
func TestStorePendingDeletionsRoundTrip(t *testing.T) {
	st, tenant := testStore(t)
	w := addWorker(t, st, tenant, "PC-Casa", 0)
	ctx := context.Background()

	if err := st.RequestDeletion(ctx, tenant, w, "SDXL-Turbo"); err != nil {
		t.Fatalf("request deletion: %v", err)
	}
	if err := st.RequestDeletion(ctx, tenant, w, "SDXL-Turbo"); err != nil {
		t.Fatalf("re-request deletion: %v", err)
	}
	pending, err := st.PendingDeletions(ctx, w)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0] != "SDXL-Turbo" {
		t.Fatalf("pending = %v, want one SDXL-Turbo", pending)
	}
	if err := st.AckDeletions(ctx, w, pending); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if pending, _ := st.PendingDeletions(ctx, w); len(pending) != 0 {
		t.Fatalf("pending after ack = %v, want none", pending)
	}
}

// La VRAM total tiene que persistirse en cada heartbeat: el panel la usa para
// «Memoria» y para la columna «En tu GPU», y antes sólo se guardaba al
// emparejar, así que el registro quedaba en 0 y toda la columna salía «—».
func TestStoreHeartbeatPersistsVRAMTotal(t *testing.T) {
	st, tenant := testStore(t)
	w := addWorker(t, st, tenant, "PC-Casa", 0)
	ctx := context.Background()

	if _, err := st.Heartbeat(ctx, w, "RTX 4090", "0.2.25", 24576, 20000, 0, 90_000, nil); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	workers, err := st.ListWorkers(ctx, tenant)
	if err != nil || len(workers) != 1 {
		t.Fatalf("list workers = %d (%v), want 1", len(workers), err)
	}
	if workers[0].VRAMTotalMB != 24576 {
		t.Fatalf("vram_total_mb = %d, want 24576", workers[0].VRAMTotalMB)
	}

	// Un worker sin telemetría legible manda 0: se conserva el último valor
	// conocido en vez de dejar la GPU sin memoria reportada otra vez.
	if _, err := st.Heartbeat(ctx, w, "RTX 4090", "0.2.25", 0, 19000, 0, 89_000, nil); err != nil {
		t.Fatalf("heartbeat sin total: %v", err)
	}
	workers, err = st.ListWorkers(ctx, tenant)
	if err != nil || len(workers) != 1 {
		t.Fatalf("list workers = %d (%v), want 1", len(workers), err)
	}
	if workers[0].VRAMTotalMB != 24576 {
		t.Fatalf("un 0 borró la vram total: %d, want 24576", workers[0].VRAMTotalMB)
	}
}

// Cancelar una descarga usa el mismo mecanismo: el admin lo encola, el
// heartbeat lo entrega y el agente confirma. Además saca el pedido de la cola
// de descargas: cancelar antes del heartbeat no debe arrancar la descarga.
func TestStorePendingDownloadCancelsRoundTrip(t *testing.T) {
	st, tenant := testStore(t)
	w := addWorker(t, st, tenant, "PC-Casa", 0)
	ctx := context.Background()

	if err := st.RequestDownload(ctx, tenant, w, "FLUX.1-dev"); err != nil {
		t.Fatalf("request download: %v", err)
	}
	if err := st.RequestDownloadCancel(ctx, tenant, w, "FLUX.1-dev"); err != nil {
		t.Fatalf("request cancel: %v", err)
	}
	if err := st.RequestDownloadCancel(ctx, tenant, w, "FLUX.1-dev"); err != nil {
		t.Fatalf("re-request cancel: %v", err)
	}

	downloads, _ := st.PendingDownloads(ctx, w)
	if len(downloads) != 0 {
		t.Fatalf("pending downloads = %v, want none after cancel", downloads)
	}
	pending, err := st.PendingDownloadCancels(ctx, w)
	if err != nil {
		t.Fatalf("pending cancels: %v", err)
	}
	if len(pending) != 1 || pending[0] != "FLUX.1-dev" {
		t.Fatalf("pending cancels = %v, want one FLUX.1-dev", pending)
	}
	if err := st.AckDownloadCancels(ctx, w, pending); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if pending, _ := st.PendingDownloadCancels(ctx, w); len(pending) != 0 {
		t.Fatalf("pending cancels after ack = %v, want none", pending)
	}
}

// La actualización remota se guarda como un pedido con id: el panel ve el
// pendiente en la lista, re-encolar lo reemplaza (reintento), un ack atrasado
// no borra un pedido nuevo y el que confirma el worker reiniciado sí lo limpia.
func TestStorePendingUpdateRoundTrip(t *testing.T) {
	st, tenant := testStore(t)
	w := addWorker(t, st, tenant, "PC-Casa", 0)
	ctx := context.Background()

	first := "https://front.example/assets/worker/synapta-worker.zip"
	hash := strings.Repeat("ab", 32)
	if err := st.RequestUpdate(ctx, tenant, w, first, hash); err != nil {
		t.Fatalf("request update: %v", err)
	}
	original, err := st.PendingUpdate(ctx, w)
	if err != nil || original == nil || original.ID == "" || original.URL != first {
		t.Fatalf("pending = %+v (%v), want the first request", original, err)
	}
	if original.SHA256 != hash {
		t.Fatalf("pending sha256 = %q, want %q", original.SHA256, hash)
	}

	// Re-encolar es el reintento del panel: reemplaza el pedido pendiente (id y
	// URL nuevos) para que un pedido que no se aplicó nunca lo deje trabado.
	retry := "https://otro.example/x.zip"
	if err := st.RequestUpdate(ctx, tenant, w, retry, ""); err != nil {
		t.Fatalf("re-request update: %v", err)
	}
	pending, err := st.PendingUpdate(ctx, w)
	if err != nil || pending == nil {
		t.Fatalf("pending = %+v (%v), want one", pending, err)
	}
	if pending.URL != retry || pending.ID == original.ID {
		t.Fatalf("pending = %+v, want the retried url and a new id", pending)
	}

	// El panel lo ve como «Actualizando…» en GET /gpu/workers.
	workers, err := st.ListWorkers(ctx, tenant)
	if err != nil || len(workers) != 1 || workers[0].RequestedUpdate == nil {
		t.Fatalf("list workers = %+v (%v), want requested_update", workers, err)
	}
	if workers[0].RequestedUpdate.ID != pending.ID {
		t.Fatalf("listed update id = %q, want %q", workers[0].RequestedUpdate.ID, pending.ID)
	}

	// Un ack con otro id (pedido reemplazado) no borra el actual.
	if err := st.AckUpdate(ctx, w, original.ID); err != nil {
		t.Fatalf("stale ack: %v", err)
	}
	if again, _ := st.PendingUpdate(ctx, w); again == nil {
		t.Fatalf("stale ack cleared the pending update")
	}
	if err := st.AckUpdate(ctx, w, pending.ID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if again, _ := st.PendingUpdate(ctx, w); again != nil {
		t.Fatalf("pending after ack = %+v, want none", again)
	}
}

// El resultado de la actualización también se persiste: el fallo reportado por
// el worker saca el pedido de la cola, queda visible en la lista (con su
// motivo) y lo borra el siguiente intento.
func TestStoreUpdateOutcomeRoundTrip(t *testing.T) {
	st, tenant := testStore(t)
	w := addWorker(t, st, tenant, "PC-Casa", 0)
	ctx := context.Background()

	if err := st.RequestUpdate(ctx, tenant, w, "https://front/x.zip", ""); err != nil {
		t.Fatalf("request update: %v", err)
	}
	pending, err := st.PendingUpdate(ctx, w)
	if err != nil || pending == nil {
		t.Fatalf("pending = %+v (%v), want one", pending, err)
	}
	// El agente avisa que empezó a aplicarla: es la marca que usa el watchdog.
	if err := st.StartUpdate(ctx, w, pending.ID); err != nil {
		t.Fatalf("start update: %v", err)
	}
	started, _ := st.PendingUpdate(ctx, w)
	if started == nil || started.StartedAt == nil {
		t.Fatalf("pending after start = %+v, want started_at", started)
	}

	// Un fallo de otro pedido no toca el vigente.
	if err := st.FailUpdate(ctx, w, "pedido-viejo", "no importa"); err != nil {
		t.Fatalf("foreign fail: %v", err)
	}
	if again, _ := st.PendingUpdate(ctx, w); again == nil {
		t.Fatal("un fallo ajeno borró el pedido vigente")
	}

	if err := st.FailUpdate(ctx, w, pending.ID, "descarga falló: timeout"); err != nil {
		t.Fatalf("fail update: %v", err)
	}
	if again, _ := st.PendingUpdate(ctx, w); again != nil {
		t.Fatalf("pending after fail = %+v, want none", again)
	}
	workers, err := st.ListWorkers(ctx, tenant)
	if err != nil || len(workers) != 1 || workers[0].UpdateFailure == nil {
		t.Fatalf("list workers = %+v (%v), want update_failure", workers, err)
	}
	if got := workers[0].UpdateFailure.Error; got != "descarga falló: timeout" {
		t.Fatalf("failure error = %q", got)
	}

	// Reintentar borra el fallo anterior: el panel deja de mostrar el viejo.
	if err := st.RequestUpdate(ctx, tenant, w, "https://front/x.zip", ""); err != nil {
		t.Fatalf("retry: %v", err)
	}
	workers, err = st.ListWorkers(ctx, tenant)
	if err != nil || len(workers) != 1 || workers[0].RequestedUpdate == nil {
		t.Fatalf("list after retry = %+v (%v)", workers, err)
	}
	if workers[0].UpdateFailure != nil {
		t.Fatalf("el reintento debe borrar el fallo anterior: %+v", workers[0].UpdateFailure)
	}
}

// Las dos colas son independientes: pedir una descarga no borra el borrado
// pendiente del mismo modelo ni al revés (el agente decide el orden).
func TestStoreDownloadAndDeletionQueuesAreIndependent(t *testing.T) {
	st, tenant := testStore(t)
	w := addWorker(t, st, tenant, "PC-Casa", 0)
	ctx := context.Background()

	if err := st.RequestDownload(ctx, tenant, w, "SD-1.5"); err != nil {
		t.Fatalf("request download: %v", err)
	}
	if err := st.RequestDeletion(ctx, tenant, w, "SD-1.5"); err != nil {
		t.Fatalf("request deletion: %v", err)
	}

	downloads, _ := st.PendingDownloads(ctx, w)
	deletions, _ := st.PendingDeletions(ctx, w)
	if len(downloads) != 1 || len(deletions) != 1 {
		t.Fatalf("downloads=%v deletions=%v, want one each", downloads, deletions)
	}
	// Confirmar el borrado no toca la descarga pendiente.
	if err := st.AckDeletions(ctx, w, deletions); err != nil {
		t.Fatalf("ack deletions: %v", err)
	}
	if downloads, _ := st.PendingDownloads(ctx, w); len(downloads) != 1 {
		t.Fatalf("downloads after deletion ack = %v, want one", downloads)
	}
}

// Un job reclamado por un worker al que se le perdió la respuesta (corte de
// red, deploy del back a mitad del long-poll) queda 'running' sin ejecutor: ni
// Claim ni FailOrphans lo tocaban y la toma se quedaba en 0 % para siempre. La
// reconciliación lo devuelve a la cola y el mismo worker puede reclamarlo.
func TestStoreRequeueStaleClaimedJob(t *testing.T) {
	st, tenant := testStore(t)
	worker := addWorker(t, st, tenant, "PC-Casa", 0)
	enqueue(t, st, tenant, "task-lost", worker)

	claimed, err := st.Claim(context.Background(), worker, tenant)
	if err != nil || claimed == nil {
		t.Fatalf("claim: job=%v err=%v", claimed, err)
	}
	// Recién reclamado: la gracia lo protege (el worker puede estar cargando
	// los pesos y todavía no mandó el primer evento).
	if n, err := st.RequeueStale(context.Background(), 2*time.Minute); err != nil || n != 0 {
		t.Fatalf("requeue recién reclamado: n=%d err=%v (want 0)", n, err)
	}
	backdateJob(t, st, "task-lost", 3*time.Minute)
	n, err := st.RequeueStale(context.Background(), 2*time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("requeue vencido: n=%d err=%v (want 1)", n, err)
	}
	job := jobStatus(t, st, tenant, "task-lost")
	if job.Status != "queued" || job.WorkerID != "" {
		t.Fatalf("job después del requeue: status=%q worker=%q, want queued sin pin", job.Status, job.WorkerID)
	}
	if again, err := st.Claim(context.Background(), worker, tenant); err != nil || again == nil {
		t.Fatalf("re-claim: job=%v err=%v", again, err)
	}
}

// Con la máquina ocupada (jobs_active > 0) la fila no se toca: puede ser un
// job en curso cuyo primer evento todavía no llegó.
func TestStoreRequeueSkipsBusyWorker(t *testing.T) {
	st, tenant := testStore(t)
	worker := addWorker(t, st, tenant, "PC-Casa", 0)
	enqueue(t, st, tenant, "task-busy", worker)
	if claimed, err := st.Claim(context.Background(), worker, tenant); err != nil || claimed == nil {
		t.Fatalf("claim: job=%v err=%v", claimed, err)
	}
	backdateJob(t, st, "task-busy", 5*time.Minute)
	if _, err := st.db.Exec(`UPDATE worker_registry SET jobs_active = 1 WHERE id = $1`, worker); err != nil {
		t.Fatalf("marcar worker ocupado: %v", err)
	}
	if n, err := st.RequeueStale(context.Background(), 2*time.Minute); err != nil || n != 0 {
		t.Fatalf("requeue con worker ocupado: n=%d err=%v (want 0)", n, err)
	}
	if job := jobStatus(t, st, tenant, "task-busy"); job.Status != "running" {
		t.Fatalf("status = %q, want running (no se toca)", job.Status)
	}
}
