package model

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"synapta/internal/modules/workerbridge"
	"synapta/internal/tenancy"
	"synapta/internal/worker"
	inferencev1 "synapta/internal/worker/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

func (f *fakeWorker) DownloadModel(_ context.Context, req *inferencev1.DownloadModelRequest) (*inferencev1.ModelOperationResponse, error) {
	switch req.Key {
	case "missing":
		return nil, status.Error(codes.NotFound, `unknown model "missing"`)
	case "busy":
		if !req.Cancel {
			return nil, status.Error(codes.FailedPrecondition, "download already in progress for \"busy\"")
		}
		return &inferencev1.ModelOperationResponse{Key: "busy", State: "cancelled", Progress: 12.5}, nil
	case "down":
		return nil, status.Error(codes.Unavailable, "worker restarting")
	}
	return &inferencev1.ModelOperationResponse{
		Key: req.Key, State: "downloading", Progress: 42.5,
		BytesDownloaded: 420, TotalBytes: 1000,
	}, nil
}

func (f *fakeWorker) DeleteModel(_ context.Context, req *inferencev1.DeleteModelRequest) (*inferencev1.ModelOperationResponse, error) {
	switch req.Key {
	case "missing":
		return nil, status.Error(codes.NotFound, `unknown model "missing"`)
	case "busy":
		return nil, status.Error(codes.FailedPrecondition, "cannot delete \"busy\" while downloading")
	}
	return &inferencev1.ModelOperationResponse{Key: req.Key, State: "deleted"}, nil
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

// stubCredentials is an in-memory tenant credential view for handler tests.
type stubCredentials struct{ usable map[string]bool }

func (s stubCredentials) UsableProviders() (map[string]bool, error) { return s.usable, nil }

func testResolver(usable map[string]bool) CredentialResolver {
	return func(*gin.Context) (CredentialLister, bool) { return stubCredentials{usable: usable}, true }
}

func allUsableResolver() CredentialResolver {
	return testResolver(map[string]bool{
		string(ProviderBytePlus):   true,
		string(ProviderGemini):     true,
		string(ProviderAnthropic):  true,
		string(ProviderHiggsfield): true,
	})
}

func setup(t *testing.T) (m *Module, addr string) {
	t.Helper()
	f := &fakeWorker{
		device: "Tesla T4",
		cuda:   true,
		models: []*inferencev1.ModelInfo{
			{Key: "SD-1.5", Label: "Stable Diffusion 1.5", Mode: "image", Engine: "diffusers",
				Pipeline: "sd15", Repo: "stable-diffusion-v1-5/stable-diffusion-v1-5", Steps: 20, VramGb: 4,
				Family: "sd", Available: true, Downloaded: true, Downloading: true, DownloadPercent: 42.5,
				SizeBytes: 420, DownloadError: "",
				DownloadPath: "/home/u/.cache/huggingface/hub/models--stable-diffusion-v1-5--stable-diffusion-v1-5"},
			{Key: "Wan2.1-T2V-1.3B", Label: "Wan 2.1 T2V 1.3B", Mode: "video", Engine: "diffusers",
				Pipeline: "wan", Repo: "Wan-AI/Wan2.1-T2V-1.3B-Diffusers", Steps: 30, VramGb: 6,
				Family: "wan", Available: true},
			{Key: "Chatterbox-TTS", Label: "Chatterbox TTS", Mode: "tts", Engine: "mock",
				Pipeline: "tts", VramGb: 6, Family: "tts", Available: true},
			{Key: "Broken", Label: "Broken weights", Mode: "image", Engine: "diffusers",
				Pipeline: "broken", VramGb: 4, Family: "broken", Available: false,
				DownloadError: "weights not downloaded"},
		},
	}
	addr = startFakeWorker(t, f)
	SetWorkerClient(worker.NewClient())
	SetWorkerAddr(addr)
	return NewModule(allUsableResolver(), ""), addr
}

// fakeBridgeSource reuses ONE concrete type: atomic.Value panics if stores
// mix dynamic types, so every bridge test shares this struct.
type fakeBridgeSource struct {
	models []workerbridge.WorkerModel
	err    error
}

func (f fakeBridgeSource) SnapshotsForTenant(context.Context, int64) ([]workerbridge.WorkerModel, error) {
	return f.models, f.err
}

func withTenant(c *gin.Context, id int64) {
	c.Request = c.Request.WithContext(
		tenancy.Context(c.Request.Context(), &tenancy.Tenant{ID: id, Slug: "t", Active: true}))
}

func setBridge(t *testing.T, f fakeBridgeSource) {
	t.Helper()
	SetBridgeSource(f)
	t.Cleanup(func() { SetBridgeSource(fakeBridgeSource{}) })
}

func bridgeModels() []workerbridge.WorkerModel {
	return []workerbridge.WorkerModel{
		{Key: "Wan2.1-T2V-1.3B", Label: "Wan 2.1", Mode: "video", Engine: "diffusers",
			Pipeline: "wan", Repo: "Wan-AI/x", Steps: 30, VRAMGb: 6, Family: "wan",
			Available: true},
		{Key: "SD-1.5", Label: "SD 1.5", Mode: "image", Engine: "diffusers",
			Pipeline: "sd15", Family: "sd", Available: true},
	}
}

func killCentral(t *testing.T) {
	t.Helper()
	SetWorkerClient(worker.NewClient())
	SetWorkerAddr("127.0.0.1:1") // discard: refused fast, no network
	t.Cleanup(func() { SetWorkerAddr("") })
}

func TestWorkerModelsFallsBackToBridge(t *testing.T) {
	m := NewModule(allUsableResolver(), "")
	killCentral(t)
	setBridge(t, fakeBridgeSource{models: bridgeModels()})

	c, w := testCtx(t, "/api/v1/worker/models")
	withTenant(c, 7)
	m.WorkerModels(c)
	env, code := decode(t, w)
	if code != http.StatusOK || !env.Success {
		t.Fatalf("status=%d body=%s", code, w.Body.String())
	}
	// Same deterministic order as ListLive: modality (image < video), then name.
	if w.Header().Get("X-Worker-Source") != "bridge" {
		t.Fatalf("expected X-Worker-Source: bridge, got %q", w.Header().Get("X-Worker-Source"))
	}
	if len(env.Data) != 2 || env.Data[0].Name != "SD-1.5" || env.Data[1].Name != "Wan2.1-T2V-1.3B" {
		t.Fatalf("expected bridge snapshots sorted, got %+v", env.Data)
	}
	if env.Data[0].Type != TypeDownloaded || !env.Data[0].Downloaded.Available {
		t.Fatalf("expected read-only available entries, got %+v", env.Data[0])
	}
}

// La vista bridge le da al panel lo que necesita para descargar: a qué GPU
// pedirle el modelo y el estado real de pesos (el agente lo leyó de su gRPC
// local). Sin esto el panel mostraba todo "Disponible" y sin botón.
func TestWorkerModelsBridgeCarriesWorkerAndWeightState(t *testing.T) {
	m := NewModule(allUsableResolver(), "")
	killCentral(t)
	setBridge(t, fakeBridgeSource{models: []workerbridge.WorkerModel{{
		Key: "FLUX.1-dev", Label: "FLUX.1 dev", Mode: "image", Engine: "diffusers",
		Pipeline: "flux", Family: "flux", VRAMGb: 24,
		Available: false, Downloading: true, DownloadPercent: 17.5,
		SizeBytes: 9_014_119_167, DownloadError: "net down",
		WorkerID: "w-1", WorkerName: "PC-Casa", WorkerVersion: "0.2.5",
	}}})

	c, w := testCtx(t, "/api/v1/worker/models")
	withTenant(c, 7)
	m.WorkerModels(c)
	env, code := decode(t, w)
	if code != http.StatusOK || !env.Success || len(env.Data) != 1 {
		t.Fatalf("status=%d body=%s", code, w.Body.String())
	}
	got := env.Data[0]
	if got.WorkerID != "w-1" || got.WorkerName != "PC-Casa" || got.WorkerVersion != "0.2.5" {
		t.Fatalf("worker identity lost: %+v", got)
	}
	d := got.Downloaded
	if d == nil || d.Available || !d.Downloading || d.DownloadPercent != 17.5 ||
		d.SizeBytes != 9_014_119_167 || d.DownloadError != "net down" {
		t.Fatalf("weight telemetry lost: %+v", d)
	}
	// Los nombres JSON que consume el panel (rutas de descarga por GPU).
	body := w.Body.String()
	for _, want := range []string{
		`"worker_id":"w-1"`, `"worker_name":"PC-Casa"`,
		`"downloading":true`, `"size_bytes":9014119167`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
}

// La ruta de los pesos también viaja en el snapshot del bridge: es lo que el
// panel admin muestra en equipos emparejados, donde el back no tiene gRPC.
func TestWorkerModelsBridgePassesWeightsPath(t *testing.T) {
	m := NewModule(allUsableResolver(), "")
	killCentral(t)
	const path = "/home/u/.cache/huggingface/hub/models--black-forest-labs--FLUX.1-dev"
	setBridge(t, fakeBridgeSource{models: []workerbridge.WorkerModel{{
		Key: "FLUX.1-dev", Label: "FLUX.1 dev", Mode: "image", Engine: "diffusers",
		Pipeline: "flux", Family: "flux", Available: true, Downloaded: true, DownloadPath: path,
		WorkerID: "w-1", WorkerName: "PC-Casa",
	}}})

	c, w := testCtx(t, "/api/v1/worker/models")
	withTenant(c, 7)
	m.WorkerModels(c)
	env, code := decode(t, w)
	if code != http.StatusOK || len(env.Data) != 1 {
		t.Fatalf("status=%d body=%s", code, w.Body.String())
	}
	d := env.Data[0].Downloaded
	if d == nil || !d.Downloaded || d.DownloadPath != path {
		t.Fatalf("bridge weights path lost: %+v", d)
	}
	if !strings.Contains(w.Body.String(), `"download_path":"`+path+`"`) {
		t.Fatalf("missing download_path in %s", w.Body.String())
	}
}

// El worker es opcional: sin worker central y sin GPUs emparejadas la vista de
// gestión responde 200 con la lista vacía. Un 500 acá rompía el panel de un
// deploy donde el worker todavía no está instalado, y no hay forma de que el
// usuario lo arregle desde esa pantalla.
func TestWorkerModelsDegradesWithoutAnySource(t *testing.T) {
	m := NewModule(allUsableResolver(), "")
	killCentral(t)
	setBridge(t, fakeBridgeSource{})

	c, w := testCtx(t, "/api/v1/worker/models")
	withTenant(c, 7)
	m.WorkerModels(c)
	env, code := decode(t, w)
	if code != http.StatusOK || !env.Success {
		t.Fatalf("status=%d body=%s", code, w.Body.String())
	}
	if len(env.Data) != 0 {
		t.Fatalf("expected an empty catalog, got %+v", env.Data)
	}
	if got := w.Header().Get("X-Worker-Source"); got != "central" {
		t.Fatalf("X-Worker-Source=%q, want central (no bridge snapshots)", got)
	}
}

func TestWorkerModelsPrefersCentral(t *testing.T) {
	m, _ := setup(t)
	setBridge(t, fakeBridgeSource{models: bridgeModels()})

	c, w := testCtx(t, "/api/v1/worker/models")
	withTenant(c, 7)
	m.WorkerModels(c)
	env, code := decode(t, w)
	if code != http.StatusOK || !env.Success {
		t.Fatalf("status=%d body=%s", code, w.Body.String())
	}
	if w.Header().Get("X-Worker-Source") != "central" {
		t.Fatalf("expected X-Worker-Source: central, got %q", w.Header().Get("X-Worker-Source"))
	}
	// Central-only entry (Broken weights) proves central won over bridge.
	found := false
	for _, md := range env.Data {
		if md.Name == "Broken" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected central catalog, got %+v", env.Data)
	}
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

	m := NewModule(allUsableResolver(), "")
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

// bridgeCatalog: lo que reporta una GPU emparejada (con la identidad que
// agrega SnapshotsForTenant, que es a quien se le pide la descarga).
func bridgeCatalog() []workerbridge.WorkerModel {
	return []workerbridge.WorkerModel{
		{Key: "SDXL-Turbo", Label: "SDXL Turbo", Mode: "image", Engine: "diffusers",
			Pipeline: "sdxl", Repo: "stabilityai/sdxl-turbo", Steps: 1, Family: "sdxl",
			Available: true, SizeBytes: 6_900_000_000,
			WorkerID: "w-gpu", WorkerName: "PC-Casa", WorkerVersion: "0.2.5"},
		{Key: "Wan2.1-T2V-1.3B", Label: "Wan 2.1", Mode: "video", Engine: "diffusers",
			Pipeline: "wan", Repo: "Wan-AI/x", Steps: 30, VRAMGb: 6, Family: "wan",
			Available: true, WorkerID: "w-gpu", WorkerName: "PC-Casa"},
		{Key: "FLUX.1-dev", Label: "FLUX.1 dev", Mode: "image", Engine: "diffusers",
			Pipeline: "flux", Family: "flux", Available: false, WorkerID: "w-gpu", WorkerName: "PC-Casa"},
	}
}

// Las GPUs emparejadas son una fuente de modelos "downloaded" más: en modo
// bridge (worker central caído o inexistente) el listado de generación debe
// mostrar lo que las máquinas del tenant pueden correr, y nada más.
func TestListTypeDownloadedIncludesBridgeModels(t *testing.T) {
	m := NewModule(allUsableResolver(), "")
	killCentral(t)
	setBridge(t, fakeBridgeSource{models: bridgeCatalog()})

	c, w := testCtx(t, "/api/v1/models?type=downloaded")
	withTenant(c, 7)
	m.List(c)
	env, code := decode(t, w)
	if code != http.StatusOK || !env.Success {
		t.Fatalf("status=%d body=%s", code, w.Body.String())
	}
	if len(env.Data) != 2 || env.Data[0].Name != "SDXL-Turbo" || env.Data[1].Name != "Wan2.1-T2V-1.3B" {
		t.Fatalf("bridge listing = %+v", env.Data)
	}
	for _, got := range env.Data {
		if got.Type != TypeDownloaded || got.Downloaded == nil || !got.Downloaded.Available {
			t.Fatalf("entry is not runnable: %+v", got)
		}
		if got.WorkerID != "w-gpu" || got.WorkerName != "PC-Casa" {
			t.Fatalf("entry lost its GPU origin: %+v", got)
		}
	}
}

// El listado público no ofrece elegir un modelo cuyos pesos no están en la
// GPU: /worker/models sí lo muestra (ahí se descarga).
func TestListTypeDownloadedHidesBridgeModelWithoutWeights(t *testing.T) {
	m := NewModule(allUsableResolver(), "")
	killCentral(t)
	setBridge(t, fakeBridgeSource{models: bridgeCatalog()})

	c, w := testCtx(t, "/api/v1/models?type=downloaded")
	withTenant(c, 7)
	m.List(c)
	if body := w.Body.String(); strings.Contains(body, "FLUX.1-dev") {
		t.Fatalf("model without weights leaked into the generation catalog: %s", body)
	}
}

// Con el worker central vivo y la GPU reportando el mismo modelo, la entrada
// no se duplica: gana la del worker (tiene telemetría de pesos y es el camino
// que despacha hoy).
func TestListMergedDedupesModelsOfBothPaths(t *testing.T) {
	m, _ := setup(t)
	setBridge(t, fakeBridgeSource{models: []workerbridge.WorkerModel{
		{Key: "SD-1.5", Mode: "image", Family: "sd", Available: true,
			WorkerID: "w-gpu", WorkerName: "PC-Casa"},
		{Key: "SDXL-Turbo", Mode: "image", Family: "sdxl", Available: true,
			WorkerID: "w-gpu", WorkerName: "PC-Casa"},
	}})

	c, w := testCtx(t, "/api/v1/models?type=downloaded")
	withTenant(c, 7)
	m.List(c)
	env, code := decode(t, w)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, w.Body.String())
	}
	// Central: SD-1.5 (downloading), Wan, Chatterbox. Bridge aporta SDXL-Turbo.
	if len(env.Data) != 4 {
		t.Fatalf("merged listing = %+v", env.Data)
	}
	sd := workerModelByName(t, env.Data, "SD-1.5")
	if sd.WorkerID != "" || sd.Downloaded == nil || !sd.Downloaded.Downloading {
		t.Fatalf("central entry was shadowed by the GPU snapshot: %+v", sd)
	}
}

// Filtro por modalidad en la vista fusionada: el modelo de la GPU responde al
// modality que las listas de generación piden.
func TestListByModalityIncludesBridgeModel(t *testing.T) {
	m := NewModule(allUsableResolver(), "")
	killCentral(t)
	setBridge(t, fakeBridgeSource{models: bridgeCatalog()})

	c, w := testCtx(t, "/api/v1/models/image")
	c.Params = gin.Params{{Key: "modality", Value: "image"}}
	withTenant(c, 7)
	m.ListByModality(c)
	env, code := decode(t, w)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, w.Body.String())
	}
	// El catálogo API (con las credenciales del tenant) sigue en la lista; lo
	// que importa es que el modelo de la GPU aparezca entre las opciones de
	// esa modalidad, resuelto como downloaded y disponible.
	found := false
	for _, got := range env.Data {
		if got.Modality != ModalityImage {
			t.Fatalf("non-image entry in the image listing: %+v", got)
		}
		if got.Name == "SDXL-Turbo" {
			found = got.Type == TypeDownloaded && got.Downloaded != nil && got.Downloaded.Available
		}
	}
	if !found {
		t.Fatalf("bridge image model missing from the image listing: %+v", env.Data)
	}
}

// Sin ninguna fuente viva, type=downloaded responde 200 con catálogo vacío: el
// worker es opcional y pedir explícitamente esa fuente no cambia la regla. El
// filtro sigue siendo útil (nunca inventa modelos ni los mezcla con los api).
func TestListTypeDownloadedDegradesWithoutAnySource(t *testing.T) {
	m := NewModule(allUsableResolver(), "")
	killCentral(t)
	setBridge(t, fakeBridgeSource{})

	c, w := testCtx(t, "/api/v1/models?type=downloaded")
	withTenant(c, 7)
	m.List(c)
	env, code := decode(t, w)
	if code != http.StatusOK || !env.Success {
		t.Fatalf("status=%d body=%s", code, w.Body.String())
	}
	if len(env.Data) != 0 {
		t.Fatalf("expected no downloaded models, got %+v", env.Data)
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

	// Static image models (3 gemini + seedream + 7 higgsfield) + live Wan image.
	if code != http.StatusOK || len(env.Data) < 10 {
		t.Fatalf("image merged: want >= 10, got %d (status %d): %s", len(env.Data), code, w.Body.String())
	}
	hf := 0
	for _, mm := range env.Data {
		if strings.Contains(mm.Name, "higgsfield") {
			hf++
		}
	}
	if hf != 7 {
		t.Fatalf("image merged: want 7 higgsfield models, got %d", hf)
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

	m := NewModule(allUsableResolver(), "")
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

// operationEnvelope mirrors the utils.Success JSON shape for weight writes.
type operationEnvelope struct {
	Success bool           `json:"success"`
	Message string         `json:"message"`
	Data    ModelOperation `json:"data"`
}

func decodeOperation(t *testing.T, w *httptest.ResponseRecorder) (operationEnvelope, int) {
	t.Helper()
	var env operationEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode operation: %v\nbody: %s", err, w.Body.String())
	}
	return env, w.Code
}

func workerModelByName(t *testing.T, models []Model, name string) Model {
	t.Helper()
	for _, m := range models {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("model %q missing: %+v", name, models)
	return Model{}
}

func TestListTypeDownloadedMapsLiveState(t *testing.T) {
	m, _ := setup(t)

	c, w := testCtx(t, "/api/v1/models?type=downloaded")
	m.List(c)
	env, code := decode(t, w)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, w.Body.String())
	}

	for _, item := range env.Data {
		if item.Name == "Broken" {
			t.Fatalf("unavailable model leaked into the public listing: %s", w.Body.String())
		}
	}
	sd := workerModelByName(t, env.Data, "SD-1.5")
	if sd.Downloaded == nil || !sd.Downloaded.Downloading || sd.Downloaded.DownloadPercent != 42.5 || sd.Downloaded.SizeBytes != 420 {
		t.Fatalf("live download state not mapped: %+v", sd.Downloaded)
	}
	// La ruta de los pesos la reporta el worker y el back la reenvía tal cual:
	// es la máquina del worker, no el disco del back.
	if !sd.Downloaded.Downloaded ||
		sd.Downloaded.DownloadPath != "/home/u/.cache/huggingface/hub/models--stable-diffusion-v1-5--stable-diffusion-v1-5" {
		t.Fatalf("weights path not mapped: %+v", sd.Downloaded)
	}
}

func TestListTypeAPIRespectsCredentials(t *testing.T) {
	setup(t)
	m := NewModule(testResolver(map[string]bool{string(ProviderBytePlus): true}), "")

	want := 0
	for _, item := range catalog {
		if item.CredentialProvider == ProviderBytePlus {
			want++
		}
	}

	c, w := testCtx(t, "/api/v1/models?type=api")
	m.List(c)
	env, code := decode(t, w)
	if code != http.StatusOK || len(env.Data) != want {
		t.Fatalf("credentialed api listing: want %d, got %d (status %d): %s", want, len(env.Data), code, w.Body.String())
	}
	for _, item := range env.Data {
		if item.CredentialProvider != ProviderBytePlus {
			t.Errorf("%s leaked without a credential", item.Name)
		}
	}
}

func TestListWithoutTenantCredentialsFailsClosed(t *testing.T) {
	setup(t)
	m := NewModule(func(*gin.Context) (CredentialLister, bool) { return nil, false }, "")

	c, w := testCtx(t, "/api/v1/models")
	m.List(c)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("missing tenant credentials: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestFilterAvailable(t *testing.T) {
	in := []Model{
		{Name: "ready", Type: TypeDownloaded, Downloaded: &DownloadedModel{Available: true}},
		{Name: "broken", Type: TypeDownloaded, Downloaded: &DownloadedModel{Available: false}},
		{Name: "unknown", Type: TypeDownloaded},
		{Name: "api", Type: TypeAPI, CredentialProvider: ProviderBytePlus},
	}
	out := FilterAvailable(in)
	if len(out) != 2 || out[0].Name != "ready" || out[1].Name != "api" {
		t.Fatalf("FilterAvailable = %+v", out)
	}
}

func TestWorkerModelsIncludesUnavailable(t *testing.T) {
	m, _ := setup(t)

	c, w := testCtx(t, "/api/v1/worker/models")
	m.WorkerModels(c)
	env, code := decode(t, w)
	if code != http.StatusOK || len(env.Data) != 4 {
		t.Fatalf("management listing: want 4, got %d (status %d): %s", len(env.Data), code, w.Body.String())
	}
	broken := workerModelByName(t, env.Data, "Broken")
	if broken.Downloaded == nil || broken.Downloaded.Available || broken.Downloaded.DownloadError != "weights not downloaded" {
		t.Fatalf("management must expose the failure: %+v", broken.Downloaded)
	}
}

func TestWorkerDownloadModelStarts(t *testing.T) {
	m, _ := setup(t)

	c, w := testCtx(t, "/api/v1/worker/models/SD-1.5/download")
	c.Params = gin.Params{{Key: "key", Value: "SD-1.5"}}
	m.WorkerDownloadModel(c)
	env, code := decodeOperation(t, w)
	if code != http.StatusCreated || !env.Success || env.Data.State != "downloading" || env.Data.Progress != 42.5 {
		t.Fatalf("download start: status=%d body=%s", code, w.Body.String())
	}
}

func TestWorkerCancelDownload(t *testing.T) {
	m, _ := setup(t)

	c, w := testCtx(t, "/api/v1/worker/models/busy/cancel")
	c.Params = gin.Params{{Key: "key", Value: "busy"}}
	m.WorkerCancelDownload(c)
	env, code := decodeOperation(t, w)
	if code != http.StatusCreated || env.Data.State != "cancelled" {
		t.Fatalf("cancel: status=%d body=%s", code, w.Body.String())
	}
}

func TestWorkerDownloadMapsWorkerErrors(t *testing.T) {
	m, _ := setup(t)

	for _, tc := range []struct {
		name   string
		key    string
		cancel bool
		status int
	}{
		{name: "unknown key", key: "missing", status: http.StatusNotFound},
		{name: "conflicting download", key: "busy", status: http.StatusConflict},
		{name: "dead worker", key: "down", status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, w := testCtx(t, "/api/v1/worker/models/"+tc.key)
			c.Params = gin.Params{{Key: "key", Value: tc.key}}
			if tc.cancel {
				m.WorkerCancelDownload(c)
			} else {
				m.WorkerDownloadModel(c)
			}
			if w.Code != tc.status {
				t.Fatalf("status=%d want %d body=%s", w.Code, tc.status, w.Body.String())
			}
		})
	}
}

func TestWorkerDeleteModel(t *testing.T) {
	m, _ := setup(t)

	c, w := testCtx(t, "/api/v1/worker/models/SD-1.5")
	c.Params = gin.Params{{Key: "key", Value: "SD-1.5"}}
	m.WorkerDeleteModel(c)
	env, code := decodeOperation(t, w)
	if code != http.StatusOK || env.Data.State != "deleted" {
		t.Fatalf("delete: status=%d body=%s", code, w.Body.String())
	}

	c, w = testCtx(t, "/api/v1/worker/models/missing")
	c.Params = gin.Params{{Key: "key", Value: "missing"}}
	m.WorkerDeleteModel(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: status=%d body=%s", w.Code, w.Body.String())
	}

	c, w = testCtx(t, "/api/v1/worker/models/busy")
	c.Params = gin.Params{{Key: "key", Value: "busy"}}
	m.WorkerDeleteModel(c)
	if w.Code != http.StatusConflict {
		t.Fatalf("delete while downloading: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestWorkerDeleteAllModels(t *testing.T) {
	m, _ := setup(t)

	c, w := testCtx(t, "/api/v1/worker/models")
	m.WorkerDeleteAllModels(c)

	var env struct {
		Success bool `json:"success"`
		Data    struct {
			Deleted    []string          `json:"deleted"`
			Failed     map[string]string `json:"failed"`
			FreedBytes int64             `json:"freed_bytes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, w.Body.String())
	}
	if w.Code != http.StatusOK || !env.Success {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(env.Data.Failed) != 0 {
		t.Fatalf("unexpected failures: %+v", env.Data.Failed)
	}
	// SD-1.5 (420 bytes on disk) and Broken (error entry) are deleted;
	// models with nothing on disk are skipped, not "deleted".
	if len(env.Data.Deleted) != 2 || env.Data.FreedBytes != 420 {
		t.Fatalf("bulk delete: %+v", env.Data)
	}
}

func TestWorkerWriteRejectsEmptyKey(t *testing.T) {
	m, _ := setup(t)

	c, w := testCtx(t, "/api/v1/worker/models/%20/download")
	c.Params = gin.Params{{Key: "key", Value: "  "}}
	m.WorkerDownloadModel(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty key: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestWorkerWriteRejectsUnconfiguredWorker(t *testing.T) {
	m, addr := setup(t)
	SetWorkerAddr("")
	t.Cleanup(func() { SetWorkerAddr(addr) })

	c, w := testCtx(t, "/api/v1/worker/models/SD-1.5/download")
	c.Params = gin.Params{{Key: "key", Value: "SD-1.5"}}
	m.WorkerDownloadModel(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unconfigured worker: status=%d body=%s", w.Code, w.Body.String())
	}
}

// Un contexto ya cancelado no cuelga el handler ni se disfraza de catálogo:
// la lectura al worker muere rápido y degrada a lista vacía, igual que un
// worker apagado. El status sigue siendo 200 porque el cliente que canceló ya
// no está escuchando y el worker es opcional.
func TestCancelledContextDegradesInsteadOfHanging(t *testing.T) {
	m, _ := setup(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead: the live read must return, not hang

	c, w := testCtx(t, "/api/v1/models?type=downloaded")
	c.Request = c.Request.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		m.List(c)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler hung on a cancelled context")
	}

	env, code := decode(t, w)
	if code != http.StatusOK || !env.Success {
		t.Fatalf("cancelled context: status=%d body=%s", code, w.Body.String())
	}
	if len(env.Data) != 0 {
		t.Fatalf("cancelled context produced models: %+v", env.Data)
	}
}
