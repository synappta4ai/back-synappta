package agencyvideo

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"synapta/config"
	"synapta/internal/modules/agency"
	"synapta/internal/modules/model"
	"synapta/internal/modules/workerbridge"
	"synapta/internal/worker"

	inferencev1 "synapta/internal/worker/proto"
)

// fakeStreamWorker is an in-process gRPC worker for generator tests: it
// streams LOADING_MODEL → GENERATING → COMPLETED and writes a small fake
// artifact where the real worker would write the MP4.
type fakeStreamWorker struct {
	inferencev1.UnimplementedInferenceServiceServer
	outputDir string
	holdOpen  chan struct{} // non-nil: streams stay open until closed

	mu       sync.Mutex
	lastMode string // último mode recibido (asserts de modalidad)
}

// mode reporta el último mode pedido por el generator.
func (f *fakeStreamWorker) mode() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastMode
}

func (f *fakeStreamWorker) GenerateMedia(req *inferencev1.MediaGenerationRequest,
	srv inferencev1.InferenceService_GenerateMediaServer,
) error {
	f.mu.Lock()
	f.lastMode = req.Mode
	f.mu.Unlock()

	artifact := filepath.Join(f.outputDir, req.JobId+".mp4")
	if err := os.WriteFile(artifact, []byte("fake-mp4-bytes"), 0o644); err != nil {
		return err
	}

	_ = srv.Send(&inferencev1.GenerationProgressResponse{JobId: req.JobId, Status: "LOADING_MODEL", Percentage: 2})
	_ = srv.Send(&inferencev1.GenerationProgressResponse{JobId: req.JobId, Status: "GENERATING", Percentage: 50, TotalSteps: 4, CurrentStep: 2})
	// Wait for test-controlled release so progress events land in the store
	// before the terminal event.
	if f.holdOpen != nil {
		select {
		case <-f.holdOpen:
		case <-srv.Context().Done():
			return srv.Context().Err()
		}
	}
	_ = srv.Send(&inferencev1.GenerationProgressResponse{JobId: req.JobId, Status: "COMPLETED", Percentage: 100, OutputFilePath: artifact})
	return nil
}

func (f *fakeStreamWorker) GetGpuTelemetry(context.Context, *inferencev1.GpuTelemetryRequest) (*inferencev1.GpuTelemetryResponse, error) {
	return &inferencev1.GpuTelemetryResponse{DeviceName: "Tesla T4"}, nil
}

func (f *fakeStreamWorker) CancelGeneration(ctx context.Context, req *inferencev1.CancelRequest) (*inferencev1.CancelResponse, error) {
	return &inferencev1.CancelResponse{JobId: req.JobId, Accepted: true}, nil
}

// startStreamWorker serves f locally and returns its address.
func startStreamWorker(t *testing.T, f *fakeStreamWorker) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	inferencev1.RegisterInferenceServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func genRequest(model string) *agency.GeneratorRequest {
	return &agency.GeneratorRequest{
		Model:    model,
		Content:  []agency.ContentItem{{Type: "text", Text: "a golden retriever astronaut"}},
		Duration: 2,
		Ratio:    "16:9",
	}
}

func TestDownloadedGenerateHappyPath(t *testing.T) {
	// Outputs are written relative to OUTPUTS_DIR; isolate in a temp cwd.
	t.Chdir(t.TempDir())

	f := &fakeStreamWorker{outputDir: t.TempDir()}
	g := NewDownloadedGenerator(worker.NewClient(), startStreamWorker(t, f), ".")

	res, err := g.Generate(genRequest("Wan2.1-T2V-1.3B"))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Status != "running" {
		t.Fatalf("submit status = %q, want running", res.Status)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := g.GetStatus(res.TaskID, "", "", "")
		if err != nil {
			t.Fatalf("GetStatus: %v", err)
		}
		switch st.Status {
		case "succeeded":
			if len(st.Outputs) != 1 || st.Outputs[0].LocalURL == "" {
				t.Fatalf("succeeded without outputs: %+v", st)
			}
			if filepath.Ext(st.Outputs[0].LocalURL) != ".mp4" {
				t.Fatalf("artifact name = %q, want .mp4", st.Outputs[0].LocalURL)
			}
			return
		case "failed":
			t.Fatalf("job failed: %s", st.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for job completion")
}

func TestDownloadedGenerateImageMode(t *testing.T) {
	t.Chdir(t.TempDir())

	f := &fakeStreamWorker{outputDir: t.TempDir()}
	g := NewDownloadedGenerator(worker.NewClient(), startStreamWorker(t, f), ".")

	// La modalidad la resuelve el core y viaja en el request: el generator
	// debe despachar mode="image" al worker (no el default de video).
	req := genRequest("SD-1.5")
	req.Modality = model.ModalityImage
	res, err := g.Generate(req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// El stream corre en una goroutine: el mode llega al worker enseguida
	// pero no antes de que Generate retorne.
	deadline := time.Now().Add(5 * time.Second)
	for f.mode() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.mode(); got != "image" {
		t.Fatalf("worker received mode=%q, want image", got)
	}

	for time.Now().Before(deadline) {
		st, _ := g.GetStatus(res.TaskID, "", "", "")
		if st.Status == "succeeded" || st.Status == "failed" {
			if st.Status == "failed" {
				t.Fatalf("job failed: %s", st.Error)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job never completed")
}

func TestDownloadedCancelTask(t *testing.T) {
	t.Chdir(t.TempDir())

	f := &fakeStreamWorker{outputDir: t.TempDir()}
	g := NewDownloadedGenerator(worker.NewClient(), startStreamWorker(t, f), ".")

	res, _ := g.Generate(genRequest("Wan2.1-T2V-1.3B"))
	if err := g.CancelTask(res.TaskID, "", "", ""); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
}

// fakeBridgeQueue implements BridgeQueue for the bridge-mode paths: the online
// check and the snapshots drive Validate; spy registra lo encolado (el pin y
// el payload) para los tests de despacho.
type fakeBridgeQueue struct {
	online bool
	models []workerbridge.WorkerModel
	spy    *bridgeSpy
}

// bridgeSpy acumula los encolados; el puntero sobrevive a las copias por
// valor del fake.
type bridgeSpy struct {
	mu       sync.Mutex
	enqueued []bridgeEnqueue
}

type bridgeEnqueue struct {
	TenantID int64
	TaskID   string
	WorkerID string
	Payload  json.RawMessage
}

func (s *bridgeSpy) add(e bridgeEnqueue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enqueued = append(s.enqueued, e)
}

func (s *bridgeSpy) last(t *testing.T) bridgeEnqueue {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.enqueued) == 0 {
		t.Fatal("no job was enqueued")
	}
	return s.enqueued[len(s.enqueued)-1]
}

func (f fakeBridgeQueue) Enqueue(_ context.Context, tenantID int64, taskID, workerID string, payload json.RawMessage) error {
	if f.spy != nil {
		f.spy.add(bridgeEnqueue{TenantID: tenantID, TaskID: taskID, WorkerID: workerID, Payload: payload})
	}
	return nil
}

func (f fakeBridgeQueue) JobByTaskID(context.Context, int64, string) (*workerbridge.Job, error) {
	return nil, nil
}

func (f fakeBridgeQueue) RequestCancel(context.Context, int64, string) error { return nil }

func (f fakeBridgeQueue) HasOnlineWorker(context.Context, int64) (bool, error) {
	return f.online, nil
}

func (f fakeBridgeQueue) SnapshotsForTenant(context.Context, int64) ([]workerbridge.WorkerModel, error) {
	return f.models, nil
}

func bridgeGenerator(t *testing.T, q fakeBridgeQueue) *DownloadedGenerator {
	t.Helper()
	g := NewDownloadedGenerator(worker.NewClient(), "", t.TempDir())
	g.SetBridge(q, 1)
	return g
}

// Un modelo declarado por la GPU pero sin pesos no debe encolarse: el job
// fallaría dentro del worker; el aviso correcto es descargarlo antes.
func TestDownloadedValidateBridgeRejectsModelWithoutWeights(t *testing.T) {
	g := bridgeGenerator(t, fakeBridgeQueue{online: true, models: []workerbridge.WorkerModel{
		{Key: "Wan2.1-T2V-1.3B", Available: false, SizeBytes: 1_200},
	}})
	err := g.Validate(genRequest("Wan2.1-T2V-1.3B"))
	if err == nil || !strings.Contains(err.Error(), "no bajó") {
		t.Fatalf("expected missing-weights error, got %v", err)
	}
}

func TestDownloadedValidateBridgeAcceptsDownloadedModel(t *testing.T) {
	g := bridgeGenerator(t, fakeBridgeQueue{online: true, models: []workerbridge.WorkerModel{
		{Key: "Wan2.1-T2V-1.3B", Available: true, SizeBytes: 28_935_661_355},
	}})
	if err := g.Validate(genRequest("Wan2.1-T2V-1.3B")); err != nil {
		t.Fatalf("expected valid request, got %v", err)
	}
}

func TestDownloadedValidateBridgeRejectsModelNotInSnapshot(t *testing.T) {
	g := bridgeGenerator(t, fakeBridgeQueue{online: true, models: []workerbridge.WorkerModel{
		{Key: "SD-1.5", Available: true},
	}})
	err := g.Validate(genRequest("Wan2.1-T2V-1.3B"))
	if err == nil || !strings.Contains(err.Error(), "no tiene el modelo") {
		t.Fatalf("expected unknown-model error, got %v", err)
	}
}

func TestDownloadedValidateFailsWithoutWorker(t *testing.T) {
	g := NewDownloadedGenerator(worker.NewClient(), "", t.TempDir())
	if err := g.Validate(&agency.GeneratorRequest{
		Model:   "X",
		Content: []agency.ContentItem{{Type: "text", Text: "p"}},
	}); err == nil {
		t.Fatal("expected validation failure with empty worker addr")
	}
}

// El job de una GPU emparejada queda fijado a la máquina que declara el
// modelo: con dos GPUs en el tenant, la que no tiene los pesos no debe
// tomarlo (fallaría dentro del worker). La modalidad resuelta viaja en el
// payload (una imagen no puede despacharse como video).
func TestDownloadedGenerateBridgePinsJobToWorkerWithWeights(t *testing.T) {
	spy := &bridgeSpy{}
	q := fakeBridgeQueue{online: true, spy: spy, models: []workerbridge.WorkerModel{
		{Key: "SDXL-Turbo", Mode: "image", Engine: "diffusers", Pipeline: "sdxl",
			Available: true, WorkerID: "worker-a", WorkerName: "PC-Casa"},
		{Key: "Wan2.1-T2V-1.3B", Mode: "video", Engine: "diffusers", Pipeline: "wan",
			Available: true, WorkerID: "worker-b", WorkerName: "Notebook"},
	}}
	g := bridgeGenerator(t, q)

	req := genRequest("SDXL-Turbo")
	req.Modality = model.ModalityImage
	res, err := g.Generate(req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Status != config.STATUS_RUNNING {
		t.Fatalf("status = %q, want running", res.Status)
	}

	got := spy.last(t)
	if got.WorkerID != "worker-a" {
		t.Fatalf("job pinned to %q, want worker-a (the GPU with the weights)", got.WorkerID)
	}
	if got.TaskID != res.TaskID || got.TenantID != 1 {
		t.Fatalf("enqueued %+v, want task %s for tenant 1", got, res.TaskID)
	}
	var payload struct {
		Mode      string `json:"mode"`
		ModelName string `json:"modelName"`
		NumFrames int    `json:"numFrames"`
	}
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, got.Payload)
	}
	if payload.Mode != "image" || payload.ModelName != "SDXL-Turbo" {
		t.Fatalf("payload mode/model = %q/%q, want image/SDXL-Turbo", payload.Mode, payload.ModelName)
	}
	if payload.NumFrames != 0 {
		t.Fatalf("image job carried numFrames=%d, want 0", payload.NumFrames)
	}
}

func TestDownloadedGenerateBridgeVideoKeepsFrames(t *testing.T) {
	spy := &bridgeSpy{}
	g := bridgeGenerator(t, fakeBridgeQueue{online: true, spy: spy, models: []workerbridge.WorkerModel{
		{Key: "Wan2.1-T2V-1.3B", Mode: "video", Available: true, WorkerID: "worker-b"},
	}})

	req := genRequest("Wan2.1-T2V-1.3B")
	req.Modality = model.ModalityVideo
	if _, err := g.Generate(req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var payload struct {
		Mode      string `json:"mode"`
		NumFrames int    `json:"numFrames"`
	}
	if err := json.Unmarshal(spy.last(t).Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if payload.Mode != "video" || payload.NumFrames == 0 {
		t.Fatalf("video payload mode/frames = %q/%d", payload.Mode, payload.NumFrames)
	}
}

// Sin GPU que declare el modelo, el job se encola abierto (""): la validación
// ya corrió antes y un pin inventado no acercaría el trabajo a ninguna parte.
func TestDownloadedGenerateBridgeLeavesJobOpenWithoutWorkerIdentity(t *testing.T) {
	spy := &bridgeSpy{}
	g := bridgeGenerator(t, fakeBridgeQueue{online: true, spy: spy, models: []workerbridge.WorkerModel{
		{Key: "Wan2.1-T2V-1.3B", Mode: "video", Available: true},
	}})

	if _, err := g.Generate(genRequest("Wan2.1-T2V-1.3B")); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got := spy.last(t); got.WorkerID != "" {
		t.Fatalf("job pinned to %q, want open", got.WorkerID)
	}
}

func TestStoreArtifactRemoteFallback(t *testing.T) {
	t.Setenv("BM_WORKER_ARTIFACT_BASE", "")
	// No shared disk + no artifact base → "" (task fails, never fake success).
	if got := storeArtifact(filepath.Join(t.TempDir(), "missing.mp4"), "job1"); got != "" {
		t.Fatalf("storeArtifact = %q, want \"\"", got)
	}
}
