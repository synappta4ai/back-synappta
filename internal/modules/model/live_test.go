package model

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"synapta/internal/worker"
	inferencev1 "synapta/internal/worker/proto"

	"google.golang.org/grpc"
)

// fakeWorker is an in-process gRPC worker: the live source of truth for the
// test, mirroring what the real Python worker reports.
type fakeWorker struct {
	inferencev1.UnimplementedInferenceServiceServer
	models []*inferencev1.ModelInfo
	device string
	cuda   bool
}

func (f *fakeWorker) ListModels(context.Context, *inferencev1.ListModelsRequest) (*inferencev1.ListModelsResponse, error) {
	return &inferencev1.ListModelsResponse{Models: f.models, DeviceName: f.device, CudaAvailable: f.cuda}, nil
}

func (f *fakeWorker) GetGpuTelemetry(context.Context, *inferencev1.GpuTelemetryRequest) (*inferencev1.GpuTelemetryResponse, error) {
	return &inferencev1.GpuTelemetryResponse{DeviceName: f.device, TotalVramMb: 15360, FreeVramMb: 10240}, nil
}

// startFakeWorker serves f on 127.0.0.1:0 and returns its address.
func startFakeWorker(t *testing.T, f *fakeWorker) string {
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

// testCtx builds a gin context for the model handlers.
func testCtx(t *testing.T, url string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, url, nil)
	return c, w
}

// decode parses the utils.Success envelope.
func decode(t *testing.T, w *httptest.ResponseRecorder) (ModelsEnvelope, int) {
	t.Helper()
	var env ModelsEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, w.Body.String())
	}
	return env, w.Code
}

// ModelsEnvelope mirrors the utils.Success JSON shape.
type ModelsEnvelope struct {
	Success bool    `json:"success"`
	Message string  `json:"message"`
	Data    []Model `json:"data"`
}

func setup(t *testing.T) (m *Module, addr string) {
	t.Helper()
	f := &fakeWorker{
		device: "Tesla T4",
		cuda:   true,
		models: []*inferencev1.ModelInfo{
			{Key: "SD-1.5", Label: "Stable Diffusion 1.5", Mode: "image", Engine: "diffusers",
				Pipeline: "sd15", Repo: "stable-diffusion-v1-5/stable-diffusion-v1-5", Steps: 20, VramGb: 4,
				Family: "sd", Available: true},
			{Key: "Wan2.1-T2V-1.3B", Label: "Wan 2.1 T2V 1.3B", Mode: "video", Engine: "diffusers",
				Pipeline: "wan", Repo: "Wan-AI/Wan2.1-T2V-1.3B-Diffusers", Steps: 30, VramGb: 6,
				Family: "wan", Available: true},
			{Key: "Chatterbox-TTS", Label: "Chatterbox TTS", Mode: "tts", Engine: "mock",
				Pipeline: "tts", VramGb: 6, Family: "tts", Available: true},
		},
	}
	addr = startFakeWorker(t, f)
	SetWorkerClient(worker.NewClient())
	SetWorkerAddr(addr)
	return NewModule(), addr
}

func TestListTypeDownloaded(t *testing.T) {
	m, _ := setup(t)

	c, w := testCtx(t, "/api/v1/models?type=downloaded")
	m.List(c)
	env, code := decode(t, w)

	if code != http.StatusOK || !env.Success {
		t.Fatalf("status=%d body=%s", code, w.Body.String())
	}
	if len(env.Data) != 3 {
		t.Fatalf("want 3 downloaded models, got %d: %s", len(env.Data), w.Body.String())
	}
	for _, m := range env.Data {
		if m.Type != TypeDownloaded {
			t.Errorf("%s: type=%q want downloaded", m.Name, m.Type)
		}
		if m.Downloaded == nil {
			t.Errorf("%s: downloaded details are nil", m.Name)
		}
	}
}

func TestDownloadedTTSMappedToEmptyModality(t *testing.T) {
	m, _ := setup(t)

	c, w := testCtx(t, "/api/v1/models?type=downloaded")
	m.List(c)
	env, _ := decode(t, w)

	var tts *Model
	for i := range env.Data {
		if env.Data[i].Name == "Chatterbox-TTS" {
			tts = &env.Data[i]
		}
	}
	if tts == nil {
		t.Fatal("Chatterbox-TTS missing from live listing")
	}
	// tts has no Synapta modality yet: must be listed with Modality "" and
	// raw mode preserved — never mislabeled, never dropped.
	if tts.Modality != "" || tts.Downloaded.Mode != "tts" {
		t.Errorf("tts model: modality=%q mode=%q, want \"\" + tts", tts.Modality, tts.Downloaded.Mode)
	}
}

func TestListTypeAPI(t *testing.T) {
	m, _ := setup(t)

	c, w := testCtx(t, "/api/v1/models?type=api")
	m.List(c)
	env, code := decode(t, w)

	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, w.Body.String())
	}
	if len(env.Data) != len(catalog) {
		t.Fatalf("want %d api models, got %d", len(catalog), len(env.Data))
	}
	for _, m := range env.Data {
		if m.Type != TypeAPI || m.Downloaded != nil {
			t.Errorf("%s: type=%q downloaded=%v, want api/nil", m.Name, m.Type, m.Downloaded)
		}
	}
}

func TestListMerged(t *testing.T) {
	m, _ := setup(t)

	c, w := testCtx(t, "/api/v1/models")
	m.List(c)
	env, code := decode(t, w)

	apiCount := len(catalog)
	if code != http.StatusOK || len(env.Data) != apiCount+3 {
		t.Fatalf("merged: want %d models, got %d (status %d)", apiCount+3, len(env.Data), code)
	}
}

func TestListMergedWorkerDown(t *testing.T) {
	// Wired client but unreachable address: API models must still be served.
	SetWorkerAddr("127.0.0.1:1")
	t.Cleanup(func() { SetWorkerAddr("") })

	m := NewModule()
	c, w := testCtx(t, "/api/v1/models")
	m.List(c)
	env, code := decode(t, w)

	if code != http.StatusOK || len(env.Data) != len(catalog) {
		t.Fatalf("degraded merge: want %d api models, got %d (status %d)", len(catalog), len(env.Data), code)
	}
	if w.Header().Get("X-Worker-Status") != "unreachable" {
		t.Errorf("X-Worker-Status header missing")
	}
}

func TestListFilterByModalityDownloaded(t *testing.T) {
	m, _ := setup(t)

	c, w := testCtx(t, "/api/v1/models?type=downloaded&modality=video")
	m.List(c)
	env, _ := decode(t, w)

	if len(env.Data) != 1 || env.Data[0].Name != "Wan2.1-T2V-1.3B" {
		t.Fatalf("video filter: got %s", w.Body.String())
	}
}

func TestListInvalidType(t *testing.T) {
	m, _ := setup(t)

	c, w := testCtx(t, "/api/v1/models?type=mock")
	m.List(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid type: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestListByModalityMerged(t *testing.T) {
	m, _ := setup(t)

	c, w := testCtx(t, "/api/v1/models/video")
	c.Params = gin.Params{{Key: "modality", Value: "video"}}
	m.ListByModality(c)
	env, code := decode(t, w)

	// Static video models (3 seedance + 7 higgsfield) + live Wan = 11.
	if code != http.StatusOK || len(env.Data) != 11 {
		t.Fatalf("video merged: want 11, got %d (status %d): %s", len(env.Data), code, w.Body.String())
	}
}

func TestListImageMerged(t *testing.T) {
	m, _ := setup(t)

	c, w := testCtx(t, "/api/v1/models/image")
	c.Params = gin.Params{{Key: "modality", Value: "image"}}
	m.ListByModality(c)
	env, code := decode(t, w)

	// Static image models (2 gemini + seedream + 3 higgsfield) + live Wan image.
	if code != http.StatusOK || len(env.Data) < 6 {
		t.Fatalf("image merged: want >= 6, got %d (status %d): %s", len(env.Data), code, w.Body.String())
	}
	hf := 0
	for _, mm := range env.Data {
		if strings.Contains(mm.Name, "higgsfield") {
			hf++
		}
	}
	if hf != 3 {
		t.Fatalf("image merged: want 3 higgsfield models, got %d", hf)
	}
}

func TestWorkerStatusEndpoint(t *testing.T) {
	m, _ := setup(t)

	c, w := testCtx(t, "/api/v1/worker/status")
	m.WorkerStatus(c)

	var env struct {
		Success bool         `json:"success"`
		Data    WorkerStatus `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, w.Body.String())
	}
	if !env.Success || !env.Data.Connected || env.Data.DeviceName != "Tesla T4" || env.Data.ModelCount != 3 {
		t.Fatalf("worker status: %+v", env.Data)
	}
	if !env.Data.CudaAvailable {
		t.Error("cuda_available should come from ListModelsResponse")
	}
}

func TestWorkerStatusUnconfigured(t *testing.T) {
	SetWorkerAddr("")
	t.Cleanup(func() { SetWorkerAddr("") })

	m := NewModule()
	c, w := testCtx(t, "/api/v1/worker/status")
	m.WorkerStatus(c)

	var env struct {
		Data WorkerStatus `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.Configured {
		t.Fatalf("expected configured=false, got %+v", env.Data)
	}
}

func TestContextCancellationPropagates(t *testing.T) {
	m, _ := setup(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead: the live read must fail, not hang

	c, w := testCtx(t, "/api/v1/models?type=downloaded")
	c.Request = c.Request.WithContext(ctx)
	m.List(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("cancelled context: status=%d body=%s", w.Code, w.Body.String())
	}
}
