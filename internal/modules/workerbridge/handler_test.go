package workerbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"synapta/internal/tenancy"
)

// ─── Fake store (interfaz de consumo BridgeStore) ──────────────

// fakeStore implementa BridgeStore en memoria: reproduce exactamente las
// semánticas que el handler asume (consumo atómico de código, token→worker,
// claim FIFO) sin tocar la DB.
type fakeStore struct {
	codes      map[string]codeRow // code hash → row
	workers    map[string]*Worker // token → worker
	jobs       []*Job
	claimErr   error
	claims     int
	heartbeats []string                  // task ids devueltos como cancelación
	requested  map[string][]string       // workerID → descargas pedidas
	cancels    map[string][]string       // workerID → cancelaciones de descarga pedidas
	deletions  map[string][]string       // workerID → borrados pedidos
	updates    map[string]*UpdateRequest // workerID → actualización pedida
	failures   map[string]*UpdateFailure // workerID → último fallo reportado
	vramTotal  int                       // último vram_total_mb recibido en el heartbeat
}

type codeRow struct {
	tenantID  int64
	expiresAt time.Time
	consumed  bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		codes:   map[string]codeRow{},
		workers: map[string]*Worker{},
	}
}

func (f *fakeStore) ListWorkers(_ context.Context, tenantID int64) ([]Worker, error) {
	out := []Worker{}
	for _, w := range f.workers {
		if w.TenantID == tenantID {
			copy := *w
			copy.RequestedUpdate = f.updates[w.ID]
			copy.UpdateFailure = f.failures[w.ID]
			out = append(out, copy)
		}
	}
	return out, nil
}

func (f *fakeStore) CreatePairingCode(_ context.Context, tenantID, _ int64) (string, time.Time, error) {
	code := "123456"
	for {
		if _, taken := f.codes[hashToken(code)]; !taken {
			break
		}
		// Colisión: siguiente número (el real reintenta con crypto/rand).
		code = "654321"
		if _, taken := f.codes[hashToken(code)]; !taken {
			break
		}
		return "", time.Time{}, errors.New("fake exhausted")
	}
	expires := time.Now().Add(pairingCodeTTL)
	f.codes[hashToken(code)] = codeRow{tenantID: tenantID, expiresAt: expires}
	return code, expires, nil
}

func (f *fakeStore) UpdateWorkerSettings(_ context.Context, tenantID int64, workerID string,
	name, label *string, paused *bool,
) error {
	for _, w := range f.workers {
		if w.ID == workerID && w.TenantID == tenantID {
			if name != nil {
				w.Name = *name
			}
			if label != nil {
				w.Label = *label
			}
			if paused != nil {
				w.Paused = *paused
			}
			return nil
		}
	}
	return ErrWorkerAuth
}

func (f *fakeStore) DeleteWorker(_ context.Context, tenantID int64, workerID string) error {
	for token, w := range f.workers {
		if w.ID == workerID && w.TenantID == tenantID {
			delete(f.workers, token)
			return nil
		}
	}
	return ErrWorkerAuth
}

func (f *fakeStore) ConsumePairingCode(_ context.Context, code string) (int64, error) {
	row, ok := f.codes[hashToken(code)]
	if !ok {
		return 0, ErrCodeNotFound
	}
	if row.consumed {
		return 0, ErrCodeUsed
	}
	if time.Now().After(row.expiresAt) {
		return 0, ErrCodeExpired
	}
	row.consumed = true
	f.codes[hashToken(code)] = row
	return row.tenantID, nil
}

func (f *fakeStore) RegisterWorker(_ context.Context, tenantID int64, name, gpu, goos, version string,
	vramTotal, vramFree int, diskFree int64, models json.RawMessage,
) (*Worker, string, error) {
	token := "tok_" + name
	if models == nil {
		models = json.RawMessage(`[]`)
	}
	w := &Worker{
		ID: uuid.NewString(), TenantID: tenantID, Name: name, GPUName: gpu,
		OS: goos, Version: version, VRAMTotalMB: vramTotal, VRAMFreeMB: vramFree,
		Models: models, LastHeartbeat: time.Now(), CreatedAt: time.Now(), Online: true,
		SelfUpdate: versionAtLeast(version, minUpdateReportVersion),
	}
	f.workers[token] = w
	return w, token, nil
}

func (f *fakeStore) WorkerByToken(_ context.Context, token string) (*Worker, error) {
	w, ok := f.workers[token]
	if !ok {
		return nil, ErrWorkerAuth
	}
	return w, nil
}

func (f *fakeStore) Heartbeat(_ context.Context, workerID string,
	_ string, _ string, vramTotal, _, _ int, _ int64, _ json.RawMessage,
) ([]string, error) {
	f.vramTotal = vramTotal
	return f.heartbeats, nil
}

func (f *fakeStore) RequestDownload(_ context.Context, tenantID int64, workerID, key string) error {
	found := false
	for _, w := range f.workers {
		if w.ID == workerID && w.TenantID == tenantID {
			found = true
			break
		}
	}
	if !found {
		return ErrWorkerAuth
	}
	if f.requested == nil {
		f.requested = map[string][]string{}
	}
	for _, k := range f.requested[workerID] {
		if k == key {
			return nil // idempotente
		}
	}
	f.requested[workerID] = append(f.requested[workerID], key)
	return nil
}

func (f *fakeStore) PendingDownloads(_ context.Context, workerID string) ([]string, error) {
	out := append([]string{}, f.requested[workerID]...)
	if out == nil {
		out = []string{}
	}
	return out, nil
}

func (f *fakeStore) AckDownloads(_ context.Context, workerID string, keys []string) error {
	keep := []string{}
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	for _, k := range f.requested[workerID] {
		if !drop[k] {
			keep = append(keep, k)
		}
	}
	if f.requested == nil {
		f.requested = map[string][]string{}
	}
	f.requested[workerID] = keep
	return nil
}

func (f *fakeStore) RequestDeletion(_ context.Context, tenantID int64, workerID, key string) error {
	for _, w := range f.workers {
		if w.ID == workerID && w.TenantID == tenantID {
			if f.deletions == nil {
				f.deletions = map[string][]string{}
			}
			for _, k := range f.deletions[workerID] {
				if k == key {
					return nil // idempotente
				}
			}
			f.deletions[workerID] = append(f.deletions[workerID], key)
			return nil
		}
	}
	return ErrWorkerAuth
}

func (f *fakeStore) PendingDeletions(_ context.Context, workerID string) ([]string, error) {
	out := append([]string{}, f.deletions[workerID]...)
	if out == nil {
		out = []string{}
	}
	return out, nil
}

func (f *fakeStore) AckDeletions(_ context.Context, workerID string, keys []string) error {
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	if f.deletions == nil {
		f.deletions = map[string][]string{}
	}
	keep := []string{}
	for _, k := range f.deletions[workerID] {
		if !drop[k] {
			keep = append(keep, k)
		}
	}
	f.deletions[workerID] = keep
	return nil
}

func (f *fakeStore) RequestDownloadCancel(_ context.Context, tenantID int64, workerID, key string) error {
	found := false
	for _, w := range f.workers {
		if w.ID == workerID && w.TenantID == tenantID {
			found = true
			break
		}
	}
	if !found {
		return ErrWorkerAuth
	}
	// Pedido contradictorio: la descarga que todavía no arrancó no debe arrancar.
	if f.requested == nil {
		f.requested = map[string][]string{}
	}
	keep := []string{}
	for _, k := range f.requested[workerID] {
		if k != key {
			keep = append(keep, k)
		}
	}
	f.requested[workerID] = keep
	if f.cancels == nil {
		f.cancels = map[string][]string{}
	}
	for _, k := range f.cancels[workerID] {
		if k == key {
			return nil // idempotente
		}
	}
	f.cancels[workerID] = append(f.cancels[workerID], key)
	return nil
}

func (f *fakeStore) PendingDownloadCancels(_ context.Context, workerID string) ([]string, error) {
	out := append([]string{}, f.cancels[workerID]...)
	if out == nil {
		out = []string{}
	}
	return out, nil
}

func (f *fakeStore) AckDownloadCancels(_ context.Context, workerID string, keys []string) error {
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	if f.cancels == nil {
		f.cancels = map[string][]string{}
	}
	keep := []string{}
	for _, k := range f.cancels[workerID] {
		if !drop[k] {
			keep = append(keep, k)
		}
	}
	f.cancels[workerID] = keep
	return nil
}

func (f *fakeStore) RequestUpdate(_ context.Context, tenantID int64, workerID, u, sha string) error {
	for _, w := range f.workers {
		if w.ID == workerID && w.TenantID == tenantID {
			if f.updates == nil {
				f.updates = map[string]*UpdateRequest{}
			}
			// Re-encolar reemplaza el pedido: es el reintento del panel cuando
			// la actualización anterior no se aplicó (y borra el fallo viejo).
			f.updates[workerID] = &UpdateRequest{ID: uuid.NewString(), URL: u, SHA256: sha,
				RequestedAt: time.Now()}
			delete(f.failures, workerID)
			return nil
		}
	}
	return ErrWorkerAuth
}

func (f *fakeStore) PendingUpdate(_ context.Context, workerID string) (*UpdateRequest, error) {
	return f.updates[workerID], nil
}

func (f *fakeStore) AckUpdate(_ context.Context, workerID, id string) error {
	if req := f.updates[workerID]; req != nil && req.ID == id {
		delete(f.updates, workerID)
		delete(f.failures, workerID)
	}
	return nil
}

func (f *fakeStore) StartUpdate(_ context.Context, workerID, id string) error {
	if req := f.updates[workerID]; req != nil && req.ID == id {
		now := time.Now()
		req.StartedAt = &now
	}
	return nil
}

func (f *fakeStore) FailUpdate(_ context.Context, workerID, id, reason string) error {
	req := f.updates[workerID]
	if req == nil || req.ID != id {
		return nil // pedido reemplazado: un fallo atrasado no pisa el nuevo
	}
	delete(f.updates, workerID)
	if f.failures == nil {
		f.failures = map[string]*UpdateFailure{}
	}
	f.failures[workerID] = &UpdateFailure{ID: id, Error: reason, At: time.Now()}
	return nil
}

func (f *fakeStore) ExpireUpdates(_ context.Context, tenantID int64, timeout time.Duration) (int64, error) {
	var n int64
	for _, w := range f.workers {
		if w.TenantID != tenantID {
			continue
		}
		req := f.updates[w.ID]
		if req == nil || req.StartedAt == nil || time.Since(*req.StartedAt) < timeout {
			continue
		}
		delete(f.updates, w.ID)
		if f.failures == nil {
			f.failures = map[string]*UpdateFailure{}
		}
		f.failures[w.ID] = &UpdateFailure{
			ID: req.ID, Error: "el worker no volvió después de reiniciarse", At: time.Now(),
		}
		n++
	}
	return n, nil
}

func (f *fakeStore) WorkerVersion(_ context.Context, tenantID int64, workerID string) (string, error) {
	for _, w := range f.workers {
		if w.ID == workerID && w.TenantID == tenantID {
			return w.Version, nil
		}
	}
	return "", ErrWorkerAuth
}

func (f *fakeStore) Claim(_ context.Context, workerID string, tenantID int64) (*Job, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	// En pausa no se reclama nada (drenaje).
	for _, w := range f.workers {
		if w.ID == workerID && w.Paused {
			return nil, nil
		}
	}
	f.claims++
	for _, j := range f.jobs {
		if j.Status == "queued" && j.TenantID == tenantID {
			j.Status = "running"
			j.WorkerID = workerID
			return j, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) ReportProgress(_ context.Context, workerID, taskID, status string,
	progress float64, streamStatus, errMsg string,
) error {
	for _, j := range f.jobs {
		if j.TaskID == taskID && j.WorkerID == workerID {
			j.Status, j.Progress, j.StreamStatus, j.ErrorMessage = status, progress, streamStatus, errMsg
			return nil
		}
	}
	return ErrJobNotFound
}

func (f *fakeStore) AttachArtifact(_ context.Context, workerID, taskID, name, u string) error {
	for _, j := range f.jobs {
		if j.TaskID == taskID && j.WorkerID == workerID {
			j.Status, j.Progress, j.ArtifactName, j.ArtifactURL = "completed", 100, name, u
			return nil
		}
	}
	return ErrJobNotFound
}

// ─── Router de prueba ──────────────────────────────────────────

// testRouter monta el module con middlewares de mentira: el adminMw deja
// pasar cualquier cosa y el tenant middleware inyecta un tenant fijo.
func testRouter(t *testing.T, f *fakeStore) *gin.Engine {
	t.Helper()
	return testRouterFor(t, f, 1)
}

// testRouterFor monta el module sobre cualquier BridgeStore (el fake o el
// store real contra Postgres) fijando el tenant de los requests admin.
func testRouterFor(t *testing.T, store BridgeStore, tenantID int64) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	m := NewModule(store)
	r := gin.New()
	v1 := r.Group("/api/v1")
	pass := func(c *gin.Context) { c.Next() }
	m.Register(v1, pass, func(c *gin.Context) {
		ctx := tenancy.Context(c.Request.Context(), &tenancy.Tenant{ID: tenantID, Slug: "test", Active: true})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, pass)
	return r
}

func doJSON(t *testing.T, r *gin.Engine, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Worker-Token", token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ─── Admin: emparejamiento ─────────────────────────────────────

func TestPairingCodeIssuedAndListed(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)

	w := doJSON(t, r, http.MethodPost, "/api/v1/gpu/pairing-code", "", nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("pairing-code status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Code      string    `json:"code"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"data"`
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !resp.Success || len(resp.Data.Code) != 6 {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
	if !resp.Data.ExpiresAt.After(time.Now()) {
		t.Fatalf("expires_at should be in the future: %v", resp.Data.ExpiresAt)
	}
}

func TestAdminListWorkersEmpty(t *testing.T) {
	r := testRouter(t, newFakeStore())
	w := doJSON(t, r, http.MethodGet, "/api/v1/gpu/workers", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `[]`) {
		t.Fatalf("expected empty list, got %s", w.Body.String())
	}
}

// ─── Register: canje del código ────────────────────────────────

func TestRegisterWithValidCode(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)

	code, _, err := f.CreatePairingCode(context.Background(), 1, 42)
	if err != nil {
		t.Fatalf("create code: %v", err)
	}
	w := doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/register", "", gin.H{
		"code": code, "name": "RTX-PC", "gpu_name": "RTX 4090",
		"os": "windows", "version": "0.1.0", "vram_total_mb": 24564,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("register status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			WorkerID string `json:"worker_id"`
			Token    string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Data.WorkerID == "" || resp.Data.Token == "" {
		t.Fatalf("register must return worker_id and token: %s", w.Body.String())
	}
	// El token autentica los endpoints internos.
	w = doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/heartbeat", resp.Data.Token, gin.H{})
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat with fresh token = %d body=%s", w.Code, w.Body.String())
	}
}

func TestRegisterRejectsBadOrReusedCode(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	code, _, _ := f.CreatePairingCode(context.Background(), 1, 42)

	cases := []struct {
		name string
		code string
	}{
		{"unknown", "000000"},
		{"reused", code},
	}
	// El primer caso válido consume el código: el segundo falla como "used".
	if w := doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/register", "",
		gin.H{"code": code, "name": "pc"}); w.Code != http.StatusCreated {
		t.Fatalf("first register = %d", w.Code)
	}
	for _, tc := range cases {
		w := doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/register", "",
			gin.H{"code": tc.code, "name": "pc"})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d body=%s", tc.name, w.Code, w.Body.String())
		}
	}
}

// ─── Token auth ────────────────────────────────────────────────

func TestInternalEndpointsRequireToken(t *testing.T) {
	r := testRouter(t, newFakeStore())
	for _, path := range []string{
		"/api/v1/internal/workers/heartbeat",
		"/api/v1/internal/workers/claim",
		"/api/v1/internal/workers/report",
	} {
		w := doJSON(t, r, http.MethodPost, path, "", gin.H{"task_id": "syn_1"})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s without token = %d, want 401", path, w.Code)
		}
	}
	// Token inventado también entra por 401.
	w := doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/heartbeat", "bad-token", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token = %d, want 401", w.Code)
	}
}

// ─── Claim ─────────────────────────────────────────────────────

func TestClaimDeliversQueuedJob(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	f.workers["tok_w"] = &Worker{ID: "w1", TenantID: 1, LastHeartbeat: time.Now()}
	f.jobs = append(f.jobs, &Job{
		ID: "j1", TenantID: 1, TaskID: "syn_99",
		Payload: json.RawMessage(`{"model_name":"Wan2.1"}`), Status: "queued",
	})

	w := doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/claim?wait=2", "tok_w", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("claim = %d body=%s", w.Code, w.Body.String())
	}
	// El contrato que lee el agente es data.job (igual que el `job: null` de
	// la cola vacía). Si el job viaja pelado en `data`, el agente lo descarta
	// sin error y el back lo deja 'running' para siempre.
	var env struct {
		Data struct {
			Job *Job `json:"job"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("claim body no es JSON: %v (%s)", err, w.Body.String())
	}
	if env.Data.Job == nil || env.Data.Job.TaskID != "syn_99" {
		t.Fatalf("claim debe entregar data.job con el trabajo: %s", w.Body.String())
	}
	if f.jobs[0].Status != "running" || f.jobs[0].WorkerID != "w1" {
		t.Fatalf("claim must mark the job running for the worker: %+v", f.jobs[0])
	}
}

func TestClaimEmptyQueueReturnsNullJob(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	f.workers["tok_w"] = &Worker{ID: "w1", TenantID: 1, LastHeartbeat: time.Now()}

	start := time.Now()
	w := doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/claim?wait=1", "tok_w", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("claim = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"job":null`) {
		t.Fatalf("empty queue must answer job=null: %s", w.Body.String())
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("claim returned too fast for a 1s long-poll: %v", time.Since(start))
	}
}

// ─── Report y artefactos ───────────────────────────────────────

func TestReportUnknownJobIsNotFound(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	f.workers["tok_w"] = &Worker{ID: "w1", TenantID: 1, LastHeartbeat: time.Now()}

	w := doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/report", "tok_w",
		gin.H{"task_id": "syn_nope", "status": "running", "progress": 10})
	if w.Code != http.StatusNotFound {
		t.Fatalf("report unknown job = %d, want 404", w.Code)
	}
}

func TestReportCompletedRequiresArtifact(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	f.workers["tok_w"] = &Worker{ID: "w1", TenantID: 1, LastHeartbeat: time.Now()}
	f.jobs = append(f.jobs, &Job{ID: "j1", TenantID: 1, TaskID: "syn_1", WorkerID: "w1", Status: "running"})

	w := doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/report", "tok_w",
		gin.H{"task_id": "syn_1", "status": "completed", "progress": 100})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("completed via report = %d, want 400 (must upload artifact)", w.Code)
	}
}

func TestArtifactUploadCompletesJob(t *testing.T) {
	t.Chdir(t.TempDir()) // SaveBinaryOutput escribe en ./outputs

	f := newFakeStore()
	r := testRouter(t, f)
	f.workers["tok_w"] = &Worker{ID: "w1", TenantID: 1, LastHeartbeat: time.Now()}
	f.jobs = append(f.jobs, &Job{ID: "j1", TenantID: 1, TaskID: "syn_7", WorkerID: "w1", Status: "running"})

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := mw.WriteField("task_id", "syn_7"); err != nil {
		t.Fatalf("field: %v", err)
	}
	part, err := mw.CreateFormFile("file", "clip.mp4")
	if err != nil {
		t.Fatalf("file part: %v", err)
	}
	if _, err := part.Write([]byte("fake-mp4")); err != nil {
		t.Fatalf("write part: %v", err)
	}
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/workers/artifacts", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Worker-Token", "tok_w")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("artifact upload = %d body=%s", w.Code, w.Body.String())
	}
	if f.jobs[0].Status != "completed" || f.jobs[0].ArtifactURL == "" {
		t.Fatalf("upload must complete the job with an URL: %+v", f.jobs[0])
	}
	if !strings.HasSuffix(f.jobs[0].ArtifactURL, "clip.mp4") {
		t.Fatalf("artifact url = %q, want clip.mp4", f.jobs[0].ArtifactURL)
	}
}

// ─── Revocación ────────────────────────────────────────────────

func TestRevokeWorkerKillsToken(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	wkr, token, err := f.RegisterWorker(context.Background(), 1, "pc", "GPU", "win", "0.1", 8000, 7000, 0, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	w := doJSON(t, r, http.MethodDelete, "/api/v1/gpu/workers/"+wkr.ID, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke = %d body=%s", w.Code, w.Body.String())
	}
	w = doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/heartbeat", token, gin.H{})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token must be 401, got %d", w.Code)
	}
}

// ─── Descargas pedidas desde el panel ──────────────────────────

func TestAdminRequestDownloadQueuesAndDelivers(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	ctx := context.Background()
	wkr, token, err := f.RegisterWorker(ctx, 1, "pc", "GPU", "win", "0.1", 8000, 7000, 0, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Admin encola la descarga (idempotente: dos pedidos, una entrada).
	path := "/api/v1/gpu/workers/" + wkr.ID + "/models/Wan2.1-T2V-1.3B/download"
	for i := 0; i < 2; i++ {
		w := doJSON(t, r, http.MethodPost, path, "", nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("request download = %d body=%s", w.Code, w.Body.String())
		}
	}
	if len(f.requested[wkr.ID]) != 1 {
		t.Fatalf("expected 1 queued download, got %v", f.requested[wkr.ID])
	}

	// Worker de otro tenant no se ve.
	if w := doJSON(t, r, http.MethodPost,
		"/api/v1/gpu/workers/00000000-0000-0000-0000-000000000000/models/X/download",
		"", nil); w.Code != http.StatusNotFound {
		t.Fatalf("foreign worker = %d, want 404", w.Code)
	}

	// El heartbeat la entrega y confirma el ack en el siguiente.
	w := doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/heartbeat", token,
		gin.H{"completed_downloads": []string{}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Wan2.1-T2V-1.3B") {
		t.Fatalf("heartbeat should deliver requested_models: %s", w.Body.String())
	}
	w = doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/heartbeat", token,
		gin.H{"completed_downloads": []string{"Wan2.1-T2V-1.3B"}})
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "Wan2.1-T2V-1.3B") {
		t.Fatalf("ack must clear the queue: %s", w.Body.String())
	}
}

func TestAdminRequestDeletionQueuesAndDelivers(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	ctx := context.Background()
	wkr, token, err := f.RegisterWorker(ctx, 1, "pc", "GPU", "win", "0.2.21", 8000, 7000, 0, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	path := "/api/v1/gpu/workers/" + wkr.ID + "/models/SDXL-Turbo"
	w := doJSON(t, r, http.MethodDelete, path, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d, want 200: %s", w.Code, w.Body.String())
	}
	// Idempotente: dos pedidos, una sola entrada en la cola.
	if w = doJSON(t, r, http.MethodDelete, path, "", nil); w.Code != http.StatusOK {
		t.Fatalf("re-delete = %d, want 200", w.Code)
	}

	w = doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/heartbeat", token,
		gin.H{"completed_deletions": []string{}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "\"requested_deletions\":[\"SDXL-Turbo\"]") {
		t.Fatalf("heartbeat should deliver requested_deletions: %s", w.Body.String())
	}
	// El ack la saca de la cola.
	w = doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/heartbeat", token,
		gin.H{"completed_deletions": []string{"SDXL-Turbo"}})
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "SDXL-Turbo") {
		t.Fatalf("ack must clear the deletion queue: %s", w.Body.String())
	}
}

func TestAdminCancelDownloadQueuesAndDropsPendingDownload(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	ctx := context.Background()
	wkr, token, err := f.RegisterWorker(ctx, 1, "pc", "GPU", "win", "0.2.23", 8000, 7000, 0, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// El panel pide el modelo, se arrepiente y lo cancela antes del heartbeat.
	if w := doJSON(t, r, http.MethodPost,
		"/api/v1/gpu/workers/"+wkr.ID+"/models/SDXL-Turbo/download", "", nil); w.Code != http.StatusCreated {
		t.Fatalf("download = %d, want 201", w.Code)
	}
	path := "/api/v1/gpu/workers/" + wkr.ID + "/models/SDXL-Turbo/download"
	w := doJSON(t, r, http.MethodDelete, path, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel = %d, want 200: %s", w.Code, w.Body.String())
	}
	// Idempotente: dos pedidos, una sola entrada en la cola de cancelaciones.
	if w = doJSON(t, r, http.MethodDelete, path, "", nil); w.Code != http.StatusOK {
		t.Fatalf("re-cancel = %d, want 200", w.Code)
	}

	w = doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/heartbeat", token,
		gin.H{"completed_download_cancels": []string{}})
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "\"requested_download_cancels\":[\"SDXL-Turbo\"]") {
		t.Fatalf("heartbeat should deliver requested_download_cancels: %s", body)
	}
	// La descarga pendiente ya no viaja: cancelar antes del heartbeat no la arranca.
	if !strings.Contains(body, "\"requested_models\":[]") {
		t.Fatalf("cancel must drop the pending download: %s", body)
	}
	// El ack la saca de la cola.
	w = doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/heartbeat", token,
		gin.H{"completed_download_cancels": []string{"SDXL-Turbo"}})
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "\"requested_download_cancels\":[\"SDXL-Turbo\"]") {
		t.Fatalf("ack must clear the cancel queue: %s", w.Body.String())
	}
}

// El heartbeat tiene que llevar la VRAM total: es el único dato con el que el
// panel dibuja «Memoria» y la columna «En tu GPU». Sin esto el registro se
// quedaba en 0 y el panel mostraba «—» en toda la columna.
func TestHeartbeatCarriesVRAMTotal(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	_, token, err := f.RegisterWorker(context.Background(), 1, "pc", "GPU", "win", "0.2.25", 0, 0, 0, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	w := doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/heartbeat", token,
		gin.H{"vram_total_mb": 24576, "vram_free_mb": 20000})
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d: %s", w.Code, w.Body.String())
	}
	if f.vramTotal != 24576 {
		t.Fatalf("store received vram_total_mb = %d, want 24576", f.vramTotal)
	}
}

// Actualización remota: el panel la encola con la URL del payload, el
// heartbeat la entrega, re-encolarla reemplaza el pedido (reintento) y el ack
// del pedido vigente la saca de la cola.
func TestAdminRequestUpdateQueuesAndDelivers(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	ctx := context.Background()
	wkr, token, err := f.RegisterWorker(ctx, 1, "pc", "GPU", "win", "0.2.27", 8000, 7000, 0, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	path := "/api/v1/gpu/workers/" + wkr.ID + "/update"
	payload := "https://front.example/assets/worker/synapta-worker.zip"
	hash := strings.Repeat("ab", 32)
	w := doJSON(t, r, http.MethodPost, path, "", gin.H{"url": payload, "sha256": hash})
	if w.Code != http.StatusOK {
		t.Fatalf("update = %d, want 200: %s", w.Code, w.Body.String())
	}
	first := f.updates[wkr.ID]
	if first == nil || first.ID == "" {
		t.Fatalf("pending update = %+v, want an id", first)
	}
	if first.SHA256 != hash {
		t.Fatalf("pending update sha256 = %q, want %q", first.SHA256, hash)
	}
	// El hash viaja al agente en el mismo pedido: sin él no puede verificar lo
	// que bajó antes de detener el worker.
	w = doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/heartbeat", token,
		gin.H{"completed_update": ""})
	if !strings.Contains(w.Body.String(), hash) {
		t.Fatalf("heartbeat should deliver the payload sha256: %s", w.Body.String())
	}

	// Re-encolar (reintento del panel) reemplaza el pedido y el heartbeat
	// entrega el nuevo, no el viejo.
	retry := "https://front.example/assets/worker/synapta-worker-retry.zip"
	if w = doJSON(t, r, http.MethodPost, path, "", gin.H{"url": retry}); w.Code != http.StatusOK {
		t.Fatalf("re-update = %d, want 200", w.Code)
	}
	w = doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/heartbeat", token,
		gin.H{"completed_update": ""})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), retry) {
		t.Fatalf("heartbeat should deliver requested_update: %s", w.Body.String())
	}
	pending := f.updates[wkr.ID]
	if pending == nil || pending.ID == "" || pending.ID == first.ID {
		t.Fatalf("pending update = %+v, want a new id", pending)
	}

	// Un ack con el id reemplazado no borra el pendiente.
	w = doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/heartbeat", token,
		gin.H{"completed_update": first.ID})
	if !strings.Contains(w.Body.String(), retry) {
		t.Fatalf("stale ack must keep the update: %s", w.Body.String())
	}
	// El ack del proceso nuevo la saca de la cola.
	w = doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/heartbeat", token,
		gin.H{"completed_update": pending.ID})
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), retry) {
		t.Fatalf("ack must clear the update: %s", w.Body.String())
	}
}

// El agente reporta el fallo con el motivo real: el panel lo muestra sin
// deducirlo del reloj y el pedido sale de la cola.
func TestHeartbeatReportsUpdateFailure(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	ctx := context.Background()
	wkr, token, err := f.RegisterWorker(ctx, 1, "pc", "GPU", "win", "0.2.27", 8000, 7000, 0, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	path := "/api/v1/gpu/workers/" + wkr.ID + "/update"
	if w := doJSON(t, r, http.MethodPost, path, "", gin.H{"url": "https://front/x.zip"}); w.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", w.Code, w.Body.String())
	}
	pending := f.updates[wkr.ID]
	if pending == nil {
		t.Fatal("no encoló la actualización")
	}

	// El proceso que vuelve (el nuevo, o el viejo relanzado por el updater)
	// manda el motivo: la descarga, el hash, la extracción o el arranque.
	w := doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/heartbeat", token,
		gin.H{"failed_update": gin.H{"id": pending.ID, "error": "descarga falló: timeout"}})
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d: %s", w.Code, w.Body.String())
	}
	if f.updates[wkr.ID] != nil {
		t.Fatalf("el fallo debe sacar el pedido de la cola: %+v", f.updates[wkr.ID])
	}

	// Y queda visible en la lista, que es lo que lee el panel.
	w = doJSON(t, r, http.MethodGet, "/api/v1/gpu/workers", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "descarga falló: timeout") {
		t.Fatalf("la lista debe traer el motivo del fallo: %s", w.Body.String())
	}

	// Un fallo atrasado del pedido reemplazado no pisa el vigente.
	if w = doJSON(t, r, http.MethodPost, path, "", gin.H{"url": "https://front/x.zip"}); w.Code != http.StatusOK {
		t.Fatalf("re-update = %d: %s", w.Code, w.Body.String())
	}
	if w = doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/heartbeat", token,
		gin.H{"failed_update": gin.H{"id": "pedido-viejo", "error": "otra cosa"}}); w.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d: %s", w.Code, w.Body.String())
	}
	if f.updates[wkr.ID] == nil {
		t.Fatal("un fallo de otro pedido no debe borrar el vigente")
	}
}

// El agente avisa que empezó a aplicar la actualización (el worker deja de
// latir); si no vuelve en 10 min el back lo marca fallido, sin confundir la
// espera de un job largo (que ocurre ANTES de empezar) con un fallo.
func TestListExpiresUpdateThatNeverCameBack(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	ctx := context.Background()
	wkr, token, err := f.RegisterWorker(ctx, 1, "pc", "GPU", "win", "0.2.27", 8000, 7000, 0, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if w := doJSON(t, r, http.MethodPost, "/api/v1/gpu/workers/"+wkr.ID+"/update", "",
		gin.H{"url": "https://front/x.zip"}); w.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", w.Code, w.Body.String())
	}
	pending := f.updates[wkr.ID]
	if pending == nil {
		t.Fatal("no encoló la actualización")
	}
	if w := doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/heartbeat", token,
		gin.H{"started_update": pending.ID}); w.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d: %s", w.Code, w.Body.String())
	}
	if f.updates[wkr.ID] == nil || f.updates[wkr.ID].StartedAt == nil {
		t.Fatalf("el heartbeat debe marcar el inicio: %+v", f.updates[wkr.ID])
	}

	// Reinicio en curso: listar no la da por fallida.
	if w := doJSON(t, r, http.MethodGet, "/api/v1/gpu/workers", "", nil); strings.Contains(w.Body.String(), "no volvió") {
		t.Fatalf("no debe fallar antes del timeout: %s", w.Body.String())
	}

	// Pasado el timeout, la máquina que no volvió se reporta como fallo.
	old := time.Now().Add(-updateRestartTimeout - time.Minute)
	f.updates[wkr.ID].StartedAt = &old
	w := doJSON(t, r, http.MethodGet, "/api/v1/gpu/workers", "", nil)
	if !strings.Contains(w.Body.String(), "no volvió") {
		t.Fatalf("la lista debe reportar el reinicio que no volvió: %s", w.Body.String())
	}
	if f.updates[wkr.ID] != nil {
		t.Fatalf("el pedido debe salir de la cola: %+v", f.updates[wkr.ID])
	}
}

// El hash es del payload de esa URL: sin URL no hay hash, y un valor que no es
// SHA-256 se rechaza en vez de llegarle al agente como si fuera válido.
func TestAdminRequestUpdateValidatesHash(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	wkr, _, err := f.RegisterWorker(context.Background(), 1, "pc", "GPU", "win", "0.2.27", 0, 0, 0, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	path := "/api/v1/gpu/workers/" + wkr.ID + "/update"
	if w := doJSON(t, r, http.MethodPost, path, "",
		gin.H{"url": "https://front/x.zip", "sha256": "no-es-un-hash"}); w.Code != http.StatusBadRequest {
		t.Fatalf("hash inválido = %d, want 400", w.Code)
	}
	if w := doJSON(t, r, http.MethodPost, path, "",
		gin.H{"sha256": strings.Repeat("a", 64)}); w.Code != http.StatusBadRequest {
		t.Fatalf("hash sin url = %d, want 400", w.Code)
	}
	if f.updates[wkr.ID] != nil {
		t.Fatalf("se encoló una actualización con hash inválido: %+v", f.updates[wkr.ID])
	}
}

// La lista marca qué GPU puede actualizarse sola: el panel usa ese dato para
// mostrar «sólo instalador» en vez de encolar un pedido que el agente viejo
// ignora (y que terminaba pareciendo un fallo).
func TestListWorkersFlagsSelfUpdate(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	ctx := context.Background()
	if _, _, err := f.RegisterWorker(ctx, 1, "vieja", "GPU", "win", "0.2.9", 0, 0, 0, nil); err != nil {
		t.Fatalf("register vieja: %v", err)
	}
	if _, _, err := f.RegisterWorker(ctx, 1, "nueva", "GPU", "win", minUpdateReportVersion, 0, 0, 0, nil); err != nil {
		t.Fatalf("register nueva: %v", err)
	}
	w := doJSON(t, r, http.MethodGet, "/api/v1/gpu/workers", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"self_update":false`) || !strings.Contains(body, `"self_update":true`) {
		t.Fatalf("list should flag both kinds of worker: %s", body)
	}
}

// Un worker que ignora `requested_update` (versión vieja) no puede quedarse con
// el pedido encolado: se le pide la pasada manual del instalador.
func TestAdminRequestUpdateRejectsWorkerThatCannotReport(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	wkr, _, err := f.RegisterWorker(context.Background(), 1, "pc", "GPU", "win", "0.2.26", 0, 0, 0, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	w := doJSON(t, r, http.MethodPost, "/api/v1/gpu/workers/"+wkr.ID+"/update", "",
		gin.H{"url": "https://front/x.zip"})
	if w.Code != http.StatusConflict {
		t.Fatalf("worker viejo = %d, want 409: %s", w.Code, w.Body.String())
	}
	if f.updates[wkr.ID] != nil {
		t.Fatalf("no debe encolar un pedido que la máquina va a ignorar: %+v", f.updates[wkr.ID])
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		cur, min string
		want     bool
	}{
		{"0.2.27", "0.2.27", true},
		{"0.2.9", "0.2.27", false}, // numérico, no alfabético
		{"0.2.26", "0.2.27", false},
		{"0.3.0", "0.2.27", true},
		{"1.0.0", "0.2.27", true},
		{"v0.2.27", "0.2.27", true},
		{"0.2", "0.2.27", false},
		{"", "0.2.27", false},
		{"desconocida", "0.2.27", false},
	}
	for _, tc := range cases {
		if got := versionAtLeast(tc.cur, tc.min); got != tc.want {
			t.Fatalf("versionAtLeast(%q, %q) = %v, want %v", tc.cur, tc.min, got, tc.want)
		}
	}
}

// Pausar deja la GPU en drenaje sin revocarla: sigue emparejada y latiendo,
// pero no reclama jobs nuevos; al reanudarla vuelve a tomarlos. El nombre y la
// etiqueta se editan desde el mismo PATCH.
func TestAdminUpdateWorkerPausesAndRenames(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	ctx := context.Background()
	wkr, token, err := f.RegisterWorker(ctx, 1, "pc", "GPU", "win", "0.2.27", 8000, 7000, 0, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Un job esperando en la cola abierta para cualquier GPU.
	f.jobs = append(f.jobs, &Job{ID: uuid.NewString(), TenantID: 1, TaskID: "t-1",
		Status: "queued"})
	path := "/api/v1/gpu/workers/" + wkr.ID

	w := doJSON(t, r, http.MethodPatch, path, "",
		gin.H{"paused": true, "name": "PC-Ana", "label": "Sala 2"})
	if w.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", w.Code, w.Body.String())
	}
	var got *Worker
	for _, wk := range f.workers {
		if wk.ID == wkr.ID {
			got = wk
		}
	}
	if got == nil || !got.Paused || got.Name != "PC-Ana" || got.Label != "Sala 2" {
		t.Fatalf("worker = %+v, want pausado y renombrado", got)
	}

	// En pausa el claim no entrega nada y el job sigue en la cola.
	w = doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/claim?wait=1", token, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "\"job\":null") {
		t.Fatalf("claim en pausa = %d: %s", w.Code, w.Body.String())
	}
	if f.jobs[0].Status != "queued" {
		t.Fatalf("el job se reclamó en pausa: %+v", f.jobs[0])
	}

	// Reanudar vuelve a habilitar el claim.
	if w = doJSON(t, r, http.MethodPatch, path, "", gin.H{"paused": false}); w.Code != http.StatusOK {
		t.Fatalf("resume = %d: %s", w.Code, w.Body.String())
	}
	w = doJSON(t, r, http.MethodPost, "/api/v1/internal/workers/claim?wait=1", token, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "t-1") {
		t.Fatalf("claim al reanudar = %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminUpdateWorkerValidates(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	wkr, _, err := f.RegisterWorker(context.Background(), 1, "pc", "GPU", "win", "0.2.27", 0, 0, 0, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	path := "/api/v1/gpu/workers/" + wkr.ID
	if w := doJSON(t, r, http.MethodPatch, path, "", gin.H{}); w.Code != http.StatusBadRequest {
		t.Fatalf("patch vacío = %d, want 400", w.Code)
	}
	if w := doJSON(t, r, http.MethodPatch, path, "", gin.H{"name": "   "}); w.Code != http.StatusBadRequest {
		t.Fatalf("nombre vacío = %d, want 400", w.Code)
	}
	if w := doJSON(t, r, http.MethodPatch, path, "",
		gin.H{"label": strings.Repeat("x", 64)}); w.Code != http.StatusBadRequest {
		t.Fatalf("etiqueta larga = %d, want 400", w.Code)
	}
	if w := doJSON(t, r, http.MethodPatch, "/api/v1/gpu/workers/not-a-uuid", "",
		gin.H{"paused": true}); w.Code != http.StatusBadRequest {
		t.Fatalf("id inválido = %d, want 400", w.Code)
	}
	if w := doJSON(t, r, http.MethodPatch, "/api/v1/gpu/workers/"+uuid.NewString(), "",
		gin.H{"paused": true}); w.Code != http.StatusNotFound {
		t.Fatalf("worker inexistente = %d, want 404", w.Code)
	}
}

func TestAdminRequestUpdateRejectsForeignURL(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	wkr, _, err := f.RegisterWorker(context.Background(), 1, "pc", "GPU", "win", "0.2.27", 0, 0, 0, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	w := doJSON(t, r, http.MethodPost, "/api/v1/gpu/workers/"+wkr.ID+"/update", "",
		gin.H{"url": "file:///etc/passwd"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("file url = %d, want 400: %s", w.Code, w.Body.String())
	}
	w = doJSON(t, r, http.MethodPost, "/api/v1/gpu/workers/"+uuid.NewString()+"/update", "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown worker = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestAdminCancelDownloadUnknownWorker(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	w := doJSON(t, r, http.MethodDelete,
		"/api/v1/gpu/workers/"+uuid.NewString()+"/models/SD-1.5/download", "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown worker = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestAdminRequestDeletionUnknownWorker(t *testing.T) {
	f := newFakeStore()
	r := testRouter(t, f)
	// Worker inexistente del tenant: 404, no 500.
	w := doJSON(t, r, http.MethodDelete,
		"/api/v1/gpu/workers/"+uuid.NewString()+"/models/SD-1.5", "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown worker = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestAdminRequestDeletionInvalidID(t *testing.T) {
	r := testRouter(t, newFakeStore())
	w := doJSON(t, r, http.MethodDelete, "/api/v1/gpu/workers/not-a-uuid/models/SD-1.5", "", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid id = %d, want 400", w.Code)
	}
}

func TestRevokeInvalidIDIsBadRequest(t *testing.T) {
	r := testRouter(t, newFakeStore())
	w := doJSON(t, r, http.MethodDelete, "/api/v1/gpu/workers/not-a-uuid", "", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid id = %d, want 400", w.Code)
	}
}

// ─── Semánticas del store que el handler asume ─────────────────

// TestStoreConsumePairingCodeAtomic reproduce la carrera del canje real:
// UPDATE condicional, un solo ganador. Se verifica contra el fake porque la
// semántica atómica vive en el SQL (cubierta por el E2E con DB real).
func TestStoreConsumePairingCodeAtomic(t *testing.T) {
	f := newFakeStore()
	code, _, _ := f.CreatePairingCode(context.Background(), 7, 1)

	first, err := f.ConsumePairingCode(context.Background(), code)
	if err != nil || first != 7 {
		t.Fatalf("first consume = (%d, %v), want (7, nil)", first, err)
	}
	if _, err := f.ConsumePairingCode(context.Background(), code); !errors.Is(err, ErrCodeUsed) {
		t.Fatalf("second consume = %v, want ErrCodeUsed", err)
	}
}

func TestNewPairingCodeFormat(t *testing.T) {
	for i := 0; i < 50; i++ {
		code, err := newPairingCode()
		if err != nil {
			t.Fatalf("newPairingCode: %v", err)
		}
		if len(code) != 6 {
			t.Fatalf("code %q is not 6 digits", code)
		}
		if _, err := url.ParseQuery("c=" + code); err != nil {
			t.Fatalf("code not URL-safe: %v", err)
		}
	}
}

func TestNewTokenUniqueAndHashed(t *testing.T) {
	a, err := newToken()
	if err != nil {
		t.Fatalf("newToken: %v", err)
	}
	b, _ := newToken()
	if a == b || len(a) < 40 {
		t.Fatalf("tokens must be unique and long: %q %q", a, b)
	}
	if hashToken(a) == hashToken(b) || hashToken(a) == a {
		t.Fatal("hashToken must be a non-trivial digest")
	}
}
