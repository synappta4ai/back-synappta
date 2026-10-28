package workerbridge

// Ciclo completo del worker emparejado contra Postgres real, por los mismos
// handlers HTTP que usa el agente (register → heartbeat → claim → report →
// artifacts). Se saltea sin BM_TEST_DATABASE_URL (ver store_test.go).
import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// pairWorker canjea un código real (store) y registra una GPU por el endpoint
// que usa el agente, devolviendo su id y su token.
func pairWorker(t *testing.T, engine *gin.Engine, st *Store, tenantID int64, name, models string) (id, token string) {
	t.Helper()
	code, _, err := st.CreatePairingCode(context.Background(), tenantID, 0)
	if err != nil {
		t.Fatalf("pairing code: %v", err)
	}
	res := doJSON(t, engine, http.MethodPost, "/api/v1/internal/workers/register", "", map[string]any{
		"code": code, "name": name, "gpu_name": "RTX 4090", "os": "windows",
		"version": "0.2.5", "vram_total_mb": 24564, "vram_free_mb": 20000,
		"models": json.RawMessage(models),
	})
	if res.Code != http.StatusCreated {
		t.Fatalf("register %s = %d body=%s", name, res.Code, res.Body.String())
	}
	var env struct {
		Data struct {
			WorkerID string `json:"worker_id"`
			Token    string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	if env.Data.WorkerID == "" || env.Data.Token == "" {
		t.Fatalf("register %s returned %+v", name, env.Data)
	}
	return env.Data.WorkerID, env.Data.Token
}

// uploadArtifact manda el multipart del contrato del agente (task_id + file).
// El body se arma de nuevo en cada llamada: un bytes.Buffer se consume.
func uploadArtifact(t *testing.T, engine *gin.Engine, token, taskID, filename string) *httptest.ResponseRecorder {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := mw.WriteField("task_id", taskID); err != nil {
		t.Fatalf("field: %v", err)
	}
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("file part: %v", err)
	}
	if _, err := part.Write([]byte("fake-png-bytes")); err != nil {
		t.Fatalf("write part: %v", err)
	}
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/workers/artifacts", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Worker-Token", token)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// TestLifecyclePairedGpuJob recorre el camino real de un trabajo en la GPU del
// usuario: la generación se encola fijada a la máquina que tiene los pesos,
// esa máquina lo reclama, reporta progreso y sube el artefacto. Es el camino
// que consumen el front (status) y el agente (claim/report/artifacts).
func TestLifecyclePairedGpuJob(t *testing.T) {
	st, tenant := testStore(t)
	engine := testRouterFor(t, st, tenant)
	models := `[{"key":"SDXL-Turbo","mode":"image","family":"sdxl","available":true,"size_bytes":6900000000}]`

	ownerID, ownerToken := pairWorker(t, engine, st, tenant, "PC-Casa", models)
	otherID, otherToken := pairWorker(t, engine, st, tenant, "Notebook", `[{"key":"SD-1.5","mode":"image","available":true}]`)
	if ownerID == otherID {
		t.Fatal("workers must have distinct ids")
	}

	if w := doJSON(t, engine, http.MethodPost, "/api/v1/internal/workers/heartbeat", ownerToken,
		map[string]any{"gpu_name": "RTX 4090", "version": "0.2.5", "models": json.RawMessage(models)}); w.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d body=%s", w.Code, w.Body.String())
	}

	// La generación se encola fijada a la GPU que declara el modelo con pesos.
	enqueue(t, st, tenant, "syn_lifecycle", ownerID)

	// Otra máquina del tenant no puede tomarla.
	w := doJSON(t, engine, http.MethodPost, "/api/v1/internal/workers/claim?wait=1", otherToken, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("claim (other) = %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "syn_lifecycle") {
		t.Fatalf("una GPU sin los pesos recibió el trabajo: %s", w.Body.String())
	}

	// La dueña sí, y con el payload intacto (el agente lo parsea para
	// reconstruir la petición gRPC: un payload vacío deja el job sin modelo).
	w = doJSON(t, engine, http.MethodPost, "/api/v1/internal/workers/claim?wait=1", ownerToken, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("claim = %d body=%s", w.Code, w.Body.String())
	}
	var claimed struct {
		Data Job `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &claimed); err != nil {
		t.Fatalf("decode claim: %v (%s)", err, w.Body.String())
	}
	if claimed.Data.TaskID != "syn_lifecycle" || claimed.Data.WorkerID != ownerID || claimed.Data.Status != "running" {
		t.Fatalf("claimed %+v", claimed.Data)
	}
	var payload map[string]string
	if err := json.Unmarshal(claimed.Data.Payload, &payload); err != nil {
		t.Fatalf("payload no es JSON: %v (%q)", err, claimed.Data.Payload)
	}
	if payload["modelName"] != "SDXL-Turbo" || payload["jobId"] != "syn_lifecycle" {
		t.Fatalf("payload sin modelo/job: %v", payload)
	}

	// Progreso: sólo el worker dueño escribe el estado del job.
	if w := doJSON(t, engine, http.MethodPost, "/api/v1/internal/workers/report", otherToken,
		map[string]any{"task_id": "syn_lifecycle", "status": "running", "progress": 99.0}); w.Code != http.StatusNotFound {
		t.Fatalf("reporte de otra GPU = %d body=%s, want 404", w.Code, w.Body.String())
	}
	if w := doJSON(t, engine, http.MethodPost, "/api/v1/internal/workers/report", ownerToken,
		map[string]any{"task_id": "syn_lifecycle", "status": "running", "progress": 42.5, "stream_status": "GENERATING"}); w.Code != http.StatusOK {
		t.Fatalf("report = %d body=%s", w.Code, w.Body.String())
	}
	job := jobStatus(t, st, tenant, "syn_lifecycle")
	if job.Progress != 42.5 || job.StreamStatus != "GENERATING" {
		t.Fatalf("el progreso de la GPU no quedó guardado: %+v", job)
	}

	// Artefacto: sólo la dueña, y completa el job.
	if w := uploadArtifact(t, engine, otherToken, "syn_lifecycle", "bridge.png"); w.Code != http.StatusNotFound {
		t.Fatalf("artefacto de otra GPU = %d body=%s, want 404", w.Code, w.Body.String())
	}
	if w := uploadArtifact(t, engine, ownerToken, "syn_lifecycle", "bridge.png"); w.Code != http.StatusOK {
		t.Fatalf("artifact upload = %d body=%s", w.Code, w.Body.String())
	}

	done := jobStatus(t, st, tenant, "syn_lifecycle")
	if done.Status != "completed" || done.Progress != 100 || done.ArtifactURL == "" || done.ArtifactName != "bridge.png" {
		t.Fatalf("job final = %+v", done)
	}
}
