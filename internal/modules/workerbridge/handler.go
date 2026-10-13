package workerbridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"synapta/internal/tenancy"
	"synapta/internal/utils"
)

// ctxKey para el worker autenticado en el request context.
type ctxKey int

const workerKey ctxKey = iota

// BridgeStore es lo que el handler necesita del store (interfaz de consumo:
// el handler no depende del SQL, y los tests usan un fake en memoria).
type BridgeStore interface {
	ListWorkers(ctx context.Context, tenantID int64) ([]Worker, error)
	CreatePairingCode(ctx context.Context, tenantID, createdBy int64) (string, time.Time, error)
	DeleteWorker(ctx context.Context, tenantID int64, workerID string) error
	ConsumePairingCode(ctx context.Context, code string) (int64, error)
	RegisterWorker(ctx context.Context, tenantID int64, name, gpu, goos, version string,
		vramTotal, vramFree int, models json.RawMessage) (*Worker, string, error)
	WorkerByToken(ctx context.Context, token string) (*Worker, error)
	Heartbeat(ctx context.Context, workerID string,
		gpu, version string, vramFree, jobsActive int, models json.RawMessage) ([]string, error)
	Claim(ctx context.Context, workerID string, tenantID int64) (*Job, error)
	ReportProgress(ctx context.Context, workerID, taskID, status string,
		progress float64, streamStatus, errMsg string) error
	AttachArtifact(ctx context.Context, workerID, taskID, artifactName, artifactURL string) error
}

// Module expone el bridge de workers: emparejamiento (admin) y el canal
// saliente del worker (register/heartbeat/claim/report/artifacts).
type Module struct{ store BridgeStore }

// NewModule construye el module sobre el store del schema de sistema.
func NewModule(store BridgeStore) *Module { return &Module{store: store} }

func (m *Module) Name() string { return "workerbridge" }

// Register monta los dos grupos:
//   - /api/v1/gpu: admin del tenant (JWT + tenant + adminMw) — listar GPUs,
//     generar código de emparejamiento, revocar.
//   - /api/v1/internal/workers: el worker saliente, autenticado por su
//     propio token (X-Worker-Token), sin JWT ni tenant middleware: el
//     tenant se resuelve desde el token.
func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, adminMw gin.HandlerFunc) {
	admin := rg.Group("/gpu")
	admin.Use(authMw, tenantMw, adminMw)
	{
		admin.GET("/workers", m.AdminListWorkers)
		admin.POST("/pairing-code", m.AdminPairingCode)
		admin.DELETE("/workers/:id", m.AdminRevokeWorker)
	}

	w := rg.Group("/internal/workers")
	{
		// register canjea el código de emparejamiento: no lleva token aún.
		w.POST("/register", m.WorkerRegister)
		authed := w.Group("", m.WorkerAuth())
		{
			authed.POST("/heartbeat", m.WorkerHeartbeat)
			authed.POST("/claim", m.WorkerClaim)
			authed.POST("/report", m.WorkerReport)
			authed.POST("/artifacts", m.WorkerArtifact)
		}
	}
}

// ─── Admin (tenant) ────────────────────────────────────────────

// AdminListWorkers handles GET /api/v1/gpu/workers — las GPUs del tenant.
func (m *Module) AdminListWorkers(c *gin.Context) {
	t, ok := tenancy.RequireTenant(c)
	if !ok {
		return
	}
	workers, err := m.store.ListWorkers(c.Request.Context(), t.ID)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, workers)
}

// pairingCodeResponse es lo que ve el admin en la app.
type pairingCodeResponse struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

// AdminPairingCode handles POST /api/v1/gpu/pairing-code — emite un código
// de 6 dígitos con TTL corto. El admin lo ve en pantalla y lo pega en la
// app de la máquina con la GPU.
func (m *Module) AdminPairingCode(c *gin.Context) {
	t, ok := tenancy.RequireTenant(c)
	if !ok {
		return
	}
	code, expires, err := m.store.CreatePairingCode(c.Request.Context(), t.ID, utils.UserIDFromContext(c))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Created(c, pairingCodeResponse{Code: code, ExpiresAt: expires})
}

// AdminRevokeWorker handles DELETE /api/v1/gpu/workers/:id — revoca el token
// de una GPU (la máquina queda fuera hasta volver a emparejar).
func (m *Module) AdminRevokeWorker(c *gin.Context) {
	t, ok := tenancy.RequireTenant(c)
	if !ok {
		return
	}
	id := c.Param("id")
	if !uuidHasFormat(id) {
		utils.BadRequest(c, "invalid worker id")
		return
	}
	if err := m.store.DeleteWorker(c.Request.Context(), t.ID, id); err != nil {
		if errors.Is(err, ErrWorkerAuth) {
			utils.NotFound(c, "worker not found")
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "worker revoked")
}

// ─── Worker (token propio) ─────────────────────────────────────

// WorkerAuth resuelve el worker desde el header X-Worker-Token. El tenant
// del request queda implícito en el worker: nunca se confía en headers del
// caller para el scope.
func (m *Module) WorkerAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("X-Worker-Token")
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized,
				gin.H{"success": false, "message": "missing X-Worker-Token"})
			return
		}
		w, err := m.store.WorkerByToken(c.Request.Context(), token)
		if err != nil {
			if errors.Is(err, ErrWorkerAuth) {
				c.AbortWithStatusJSON(http.StatusUnauthorized,
					gin.H{"success": false, "message": "invalid worker token"})
				return
			}
			utils.InternalError(c, err.Error())
			return
		}
		ctx := context.WithValue(c.Request.Context(), workerKey, w)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// workerFrom recupera el worker autenticado (garantizado por WorkerAuth).
func workerFrom(c *gin.Context) *Worker {
	w, _ := c.Request.Context().Value(workerKey).(*Worker)
	return w
}

// registerRequest es lo que la app de la máquina manda al emparejar.
type registerRequest struct {
	Code        string `json:"code" binding:"required"`
	Name        string `json:"name"`
	GPUName     string `json:"gpu_name"`
	OS          string `json:"os"`
	Version     string `json:"version"`
	VRAMTotalMB int    `json:"vram_total_mb"`
	VRAMFreeMB  int    `json:"vram_free_mb"`
	// RawMessage (y no []byte): el worker manda un array de objetos y
	// []byte interpretaría JSON como base64 → 400 al emparejar.
	Models json.RawMessage `json:"models"`
}

// registerResponse devuelve el token de larga duración (una sola vez).
type registerResponse struct {
	WorkerID string `json:"worker_id"`
	Token    string `json:"token"`
	Tenant   string `json:"tenant_name"`
}

// WorkerRegister handles POST /api/v1/internal/workers/register — canjea el
// código de emparejamiento por un token. Un solo ganador por código (consumo
// atómico en el store).
func (m *Module) WorkerRegister(c *gin.Context) {
	var req registerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	tenantID, err := m.store.ConsumePairingCode(c.Request.Context(), req.Code)
	if err != nil {
		switch {
		case errors.Is(err, ErrCodeNotFound), errors.Is(err, ErrCodeExpired), errors.Is(err, ErrCodeUsed):
			utils.Unauthorized(c, err.Error())
		default:
			utils.InternalError(c, err.Error())
		}
		return
	}
	name := req.Name
	if name == "" {
		name = req.GPUName
	}
	if name == "" {
		name = "GPU " + req.OS
	}
	w, token, err := m.store.RegisterWorker(c.Request.Context(), tenantID,
		name, req.GPUName, req.OS, req.Version,
		req.VRAMTotalMB, req.VRAMFreeMB, req.Models)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	log.Printf("[workerbridge] worker %s paired for tenant %d (%s, %s)",
		shortID(w.ID), tenantID, w.GPUName, w.OS)
	utils.Created(c, registerResponse{WorkerID: w.ID, Token: token})
}

// heartbeatRequest mantiene vivo el estado del worker.
type heartbeatRequest struct {
	GPUName    string          `json:"gpu_name"`
	Version    string          `json:"version"`
	VRAMFreeMB int             `json:"vram_free_mb"`
	JobsActive int             `json:"jobs_active"`
	Models     json.RawMessage `json:"models"`
}

// WorkerHeartbeat handles POST /api/v1/internal/workers/heartbeat — refresh
// + cancelaciones pendientes (viajan en la respuesta para que el agente
// local aborte su generación en curso).
func (m *Module) WorkerHeartbeat(c *gin.Context) {
	w := workerFrom(c)
	var req heartbeatRequest
	_ = c.ShouldBindJSON(&req) // body opcional
	cancelIDs, err := m.store.Heartbeat(c.Request.Context(), w.ID,
		req.GPUName, req.Version, req.VRAMFreeMB, req.JobsActive, req.Models)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if cancelIDs == nil {
		cancelIDs = []string{}
	}
	utils.Success(c, gin.H{"cancel_task_ids": cancelIDs})
}

// WorkerClaim handles POST /api/v1/internal/workers/claim — long-poll de la
// cola: responde apenas hay un job (o al agotarse el wait, con job=null).
// El worker reintenta en bucle: sin jobs, la conexión casi no consume.
func (m *Module) WorkerClaim(c *gin.Context) {
	w := workerFrom(c)
	wait := 25 * time.Second
	if s := c.Query("wait"); s != "" {
		if secs, err := strconv.Atoi(s); err == nil && secs > 0 && secs <= 60 {
			wait = time.Duration(secs) * time.Second
		}
	}
	deadline := time.Now().Add(wait)
	for {
		job, err := m.store.Claim(c.Request.Context(), w.ID, w.TenantID)
		if err != nil {
			utils.InternalError(c, err.Error())
			return
		}
		if job != nil {
			utils.Success(c, job)
			return
		}
		if time.Now().After(deadline) {
			utils.Success(c, gin.H{"job": nil})
			return
		}
		select {
		case <-c.Request.Context().Done():
			return
		case <-time.After(1 * time.Second):
		}
	}
}

// reportRequest es el progreso del job en ejecución.
type reportRequest struct {
	TaskID       string  `json:"task_id" binding:"required"`
	Status       string  `json:"status"` // queued|running|failed|cancelled
	Progress     float64 `json:"progress"`
	StreamStatus string  `json:"stream_status"`
	ErrorMessage string  `json:"error_message"`
}

// WorkerReport handles POST /api/v1/internal/workers/report — progreso y
// estados terminales (failed/cancelled). El completed llega junto al
// artefacto (AttachArtifact), nunca solo.
func (m *Module) WorkerReport(c *gin.Context) {
	w := workerFrom(c)
	var req reportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	if req.Status == "completed" {
		utils.BadRequest(c, "completed requires the artifact upload")
		return
	}
	if err := m.store.ReportProgress(c.Request.Context(), w.ID, req.TaskID,
		req.Status, req.Progress, req.StreamStatus, req.ErrorMessage); err != nil {
		if errors.Is(err, ErrJobNotFound) {
			utils.NotFound(c, "job not found for this worker")
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "reported")
}

// WorkerArtifact handles POST /api/v1/internal/workers/artifacts — subida
// multipart del output terminado. El back es el dueño del archivo: lo guarda
// en su outputs/ y linkea el job. Idempotente por task_id.
func (m *Module) WorkerArtifact(c *gin.Context) {
	w := workerFrom(c)
	taskID := c.PostForm("task_id")
	if taskID == "" {
		utils.BadRequest(c, "task_id is required")
		return
	}
	file, err := c.FormFile("file")
	if err != nil {
		utils.BadRequest(c, "file is required: "+err.Error())
		return
	}
	f, err := file.Open()
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	defer f.Close()
	// io.ReadAll en vez de un Read único: un solo Read puede devolver menos
	// bytes que file.Size y dejar el artefacto truncado.
	data, err := io.ReadAll(f)
	if err != nil {
		utils.InternalError(c, "read upload: "+err.Error())
		return
	}
	if len(data) == 0 {
		utils.BadRequest(c, "empty artifact")
		return
	}
	localURL, err := utils.SaveBinaryOutput(data, file.Filename)
	if err != nil {
		utils.InternalError(c, "store artifact: "+err.Error())
		return
	}
	if err := m.store.AttachArtifact(c.Request.Context(), w.ID, taskID,
		file.Filename, localURL); err != nil {
		if errors.Is(err, ErrJobNotFound) {
			utils.NotFound(c, "job not found for this worker")
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	log.Printf("[workerbridge] worker %s uploaded artifact for %s: %s", shortID(w.ID), taskID, localURL)
	utils.Success(c, gin.H{"artifact_url": localURL})
}

// ─── Reconciler ────────────────────────────────────────────────

// ReconcileLoop barre jobs huérfanos (worker murió a mitad de generación)
// para que nada quede "running" para siempre. Corre en main con el stop
// channel del shutdown.
func ReconcileLoop(store *Store, stop <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			n, err := store.FailOrphans(ctx)
			cancel()
			if err != nil {
				log.Printf("[workerbridge] reconcile: %v", err)
			} else if n > 0 {
				log.Printf("[workerbridge] reconcile: %d orphaned job(s) marked failed", n)
			}
		}
	}
}

// Store accessor para wiring externo (reconciler arranca desde main).
func (m *Module) Store() BridgeStore { return m.store }

// ensure sql import es usado (ErrNoRows en el scan de codigos via database/sql).
var _ = sql.ErrNoRows
