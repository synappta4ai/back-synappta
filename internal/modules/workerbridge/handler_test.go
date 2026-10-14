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
	codes     map[string]codeRow // code hash → row
	workers   map[string]*Worker // token → worker
	jobs      []*Job
	claimErr  error
	claims    int
	heartbeats []string // task ids devueltos como cancelación
	requested  map[string][]string // workerID → descargas pedidas
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
			out = append(out, *w)
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
	vramTotal, vramFree int, models json.RawMessage,
) (*Worker, string, error) {
	token := "tok_" + name
	if models == nil {
		models = json.RawMessage(`[]`)
	}
	w := &Worker{
		ID: uuid.NewString(), TenantID: tenantID, Name: name, GPUName: gpu,
		OS: goos, Version: version, VRAMTotalMB: vramTotal, VRAMFreeMB: vramFree,
		Models: models, LastHeartbeat: time.Now(), CreatedAt: time.Now(), Online: true,
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
	_ string, _ string, _, _ int, _ json.RawMessage,
) ([]string, error) {
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

func (f *fakeStore) Claim(_ context.Context, workerID string, tenantID int64) (*Job, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
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
	gin.SetMode(gin.TestMode)
	m := NewModule(f)
	r := gin.New()
	v1 := r.Group("/api/v1")
	pass := func(c *gin.Context) { c.Next() }
	m.Register(v1, pass, func(c *gin.Context) {
		ctx := tenancy.Context(c.Request.Context(), &tenancy.Tenant{ID: 1, Slug: "test", Active: true})
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
	if !strings.Contains(w.Body.String(), "syn_99") {
		t.Fatalf("claim should deliver the queued job: %s", w.Body.String())
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
	wkr, token, err := f.RegisterWorker(context.Background(), 1, "pc", "GPU", "win", "0.1", 8000, 7000, nil)
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
	wkr, token, err := f.RegisterWorker(ctx, 1, "pc", "GPU", "win", "0.1", 8000, 7000, nil)
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
