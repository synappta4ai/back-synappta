// Package agencyvideo: downloaded-model generator.
//
// Runs models whose weights live on the inference worker
// (type "downloaded" in the model catalog). Unlike the API generators, there
// is no external provider: Synapta dials the worker directly over gRPC.
//
// Flow (matches the async API generators from the core's point of view):
//
//	Generate()   → submits GenerateMedia, spawns a background stream consumer
//	               that updates the task store on every progress event and
//	               stores the artifact locally on COMPLETED.
//	GetStatus()  → answers from the task store (kept fresh by the stream).
//	CancelTask() → drops the local stream and calls CancelGeneration.
package agencyvideo

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"synapta/config"
	"synapta/internal/modules/agency"
	"synapta/internal/modules/model"
	"synapta/internal/modules/workerbridge"
	"synapta/internal/utils"
	"synapta/internal/worker"
	workerpb "synapta/internal/worker/proto"

	"google.golang.org/protobuf/encoding/protojson"
)

// streamTimeout bounds a whole generation stream. The inference worker can
// take a long time for heavy jobs, so this is generous — it only guards
// against a silently dead worker (no events, no EOF).
const streamTimeout = 6 * time.Hour

// BridgeQueue es la cola del bridge saliente (Fase A del plan distribuido):
// el back encola el trabajo y la GPU del usuario lo reclama por HTTP.
// workerbridge.Store la implementa; los tests usan un fake.
type BridgeQueue interface {
	// Enqueue encola el trabajo. workerID fija la GPU destino cuando se sabe
	// cuál tiene los pesos ("" = lo puede tomar cualquier GPU del tenant).
	Enqueue(ctx context.Context, tenantID int64, taskID, workerID string, payload json.RawMessage) error
	JobByTaskID(ctx context.Context, tenantID int64, taskID string) (*workerbridge.Job, error)
	RequestCancel(ctx context.Context, tenantID int64, taskID string) error
	HasOnlineWorker(ctx context.Context, tenantID int64) (bool, error)
	SnapshotsForTenant(ctx context.Context, tenantID int64) ([]workerbridge.WorkerModel, error)
}

// DownloadedGenerator serves every model type "downloaded": the worker owns
// the weights, Synapta only dispatches jobs.
//
// Dos caminos, elegidos por configuración:
//   - gRPC directo (BM_WORKER_ADDR): worker en la misma red (modo local o
//     VM alquilada con túnel). Streaming de progreso en vivo.
//   - bridge saliente (SetBridge): la GPU del usuario está detrás de su NAT;
//     el trabajo se encola en worker_jobs y la máquina lo reclama por HTTP.
//     El estado vive en la DB (sobrevive reinicios de back y de worker).
type DownloadedGenerator struct {
	client  *worker.Client
	addr    string // BM_WORKER_ADDR
	outputs string // Synapta outputs dir (informational; utils handles storage)
	store   *downloadedTaskStore
	label   string

	bridge   BridgeQueue // nil → solo gRPC directo
	tenantID int64       // dueño de la cola de bridge
}

// NewDownloadedGenerator builds the generator over the stateless worker
// client and the configured worker address.
func NewDownloadedGenerator(client *worker.Client, addr, outputsDir string) *DownloadedGenerator {
	return &DownloadedGenerator{
		client:  client,
		addr:    addr,
		outputs: outputsDir,
		store:   newDownloadedTaskStore(),
		label:   "downloaded",
	}
}

// SetBridge activa el camino saliente (GPU del usuario detrás de NAT) y
// fija el tenant dueño de la cola. Sin bridge, el generator vive solo del
// gRPC directo como siempre.
func (g *DownloadedGenerator) SetBridge(q BridgeQueue, tenantID int64) {
	g.bridge = q
	g.tenantID = tenantID
}

// hasBridge reports whether the tenant has paired GPUs (multi-usuario: cada
// usuario empareja su propia máquina y el store la aísla por tenant_id).
func (g *DownloadedGenerator) hasBridge() bool { return g.bridge != nil }

// bridgeMode reports whether jobs must flow through the outbound queue
// instead of the direct gRPC stream (legacy: bridge sin worker central).
// En despliegues híbridos (central + bridge) la decisión es por job en
// useBridgeFor: la GPU propia del tenant primero, el central como fallback.
func (g *DownloadedGenerator) bridgeMode() bool { return g.bridge != nil && g.addr == "" }

// SetWorkerAddr sobreescribe la dirección del worker para este generador.
// Útil como fallback global; se prefiere WorkerAddr del request (Option B).
func (g *DownloadedGenerator) SetWorkerAddr(addr string) { g.addr = addr }

// effectiveAddr devuelve la dirección del worker a usar: la del request si
// se proporcionó (Option B), o la global del generador (BM_WORKER_ADDR), o la
// global del paquete model como último recurso.
func (g *DownloadedGenerator) effectiveAddr(reqAddr string) string {
	if strings.TrimSpace(reqAddr) != "" {
		return reqAddr
	}
	if g.addr != "" {
		return g.addr
	}
	return model.WorkerAddr()
}

func (g *DownloadedGenerator) Name() string        { return g.label }
func (g *DownloadedGenerator) ContentType() string { return "video" }

// Match claims every catalog model of type "downloaded". Con bridge
// activo se reclama además todo nombre fuera del catálogo API y la
// validación real (¿alguna GPU del tenant puede correrlo?) ocurre en
// Validate contra el snapshot del registry y, finalmente, en el worker al
// ejecutar.
func (g *DownloadedGenerator) Match(modelName string) bool {
	m := agency.LookupModel(modelName, g.addr)
	if m != nil {
		return m.Type == model.TypeDownloaded
	}
	return g.hasBridge()
}

// bridgeSnapshotFor devuelve la entrada del snapshot del tenant para el
// modelo (nil cuando ninguna GPU emparejada lo declara). Es una lectura de
// DB barata y con scope de tenant: cada usuario ve solo sus máquinas.
func (g *DownloadedGenerator) bridgeSnapshotFor(ctx context.Context, modelName string) *workerbridge.WorkerModel {
	if g.bridge == nil {
		return nil
	}
	snapshots, err := g.bridge.SnapshotsForTenant(ctx, g.tenantID)
	if err != nil {
		return nil
	}
	for i, s := range snapshots {
		if strings.EqualFold(s.Key, modelName) {
		return &snapshots[i]
		}
	}
	return nil
}

// bridgeCanRun informa si una GPU online del tenant tiene los pesos del
// modelo listos ahora (el camino preferido: la máquina propia del usuario).
func (g *DownloadedGenerator) bridgeCanRun(modelName string) bool {
	if g.bridge == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), worker.DefaultTimeout)
	defer cancel()
	if online, err := g.bridge.HasOnlineWorker(ctx, g.tenantID); err != nil || !online {
		return false
	}
	snap := g.bridgeSnapshotFor(ctx, modelName)
	return snap != nil && snap.Available
}

// useBridgeFor decide el despacho de un job: la GPU propia del tenant cuando
// tiene los pesos (local-first multi-usuario), el worker central en caso
// contrario. Así un central caído o sin el modelo no bloquea a los usuarios
// con worker propio emparejado.
func (g *DownloadedGenerator) useBridgeFor(modelName string) bool {
	return g.bridgeCanRun(modelName)
}

// Validate checks the shared requirements plus a reachable dispatch path:
// la GPU propia del tenant (bridge) cuando tiene los pesos, o el worker
// central por gRPC como fallback.
func (g *DownloadedGenerator) Validate(req *agency.GeneratorRequest) error {
	if err := agency.ValidateCommon(req); err.HasErrors() {
		return err
	}
	// Camino preferido: la máquina del propio usuario (multi-usuario).
	if g.bridgeCanRun(req.Model) {
		return nil
	}
	// Fallback: worker central por gRPC (modo local / VM con túnel).
	addr := g.effectiveAddr(req.WorkerAddr)
	if addr != "" {
		ctx, cancel := context.WithTimeout(context.Background(), worker.DefaultTimeout)
		defer cancel()
		if err := g.client.Ping(ctx, addr); err != nil {
			// El central no responde: si hay bridge con GPUs online el
			// problema es el modelo, no la conectividad.
			if g.hasBridge() {
				return g.bridgeValidateError(req.Model)
			}
			return fmt.Errorf("inference worker unreachable: %w", err)
		}
		return nil
	}
	// Sin central: el error preciso lo da el estado del bridge.
	if g.hasBridge() {
		return g.bridgeValidateError(req.Model)
	}
	return fmt.Errorf("inference worker not configured (BM_WORKER_ADDR)")
}

// bridgeValidateError explica por qué la GPU del tenant no puede correr el
// modelo: sin GPU online, modelo no declarado, o pesos sin descargar (con la
// acción concreta para cada caso).
func (g *DownloadedGenerator) bridgeValidateError(modelName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), worker.DefaultTimeout)
	defer cancel()
	online, err := g.bridge.HasOnlineWorker(ctx, g.tenantID)
	if err != nil {
		return fmt.Errorf("worker registry unavailable: %w", err)
	}
	if !online {
		return fmt.Errorf("no hay GPU conectada: emparejá una máquina con tu GPU desde Tu GPU para usar modelos descargados")
	}
	if snap := g.bridgeSnapshotFor(ctx, modelName); snap != nil {
		// El catálogo de la GPU declara el modelo pero no tiene los pesos:
		// encolar el job lo haría fallar tarde; se avisa ahora y con la
		// acción concreta (descargarlo desde el panel).
		return fmt.Errorf("la GPU conectada no bajó %q todavía: "+
			"descargalo en Tu GPU → Modelos del worker", modelName)
	}
	return fmt.Errorf("la GPU conectada no tiene el modelo %q", modelName)
}

// Generate submits the job to the worker and starts a background consumer of
// the progress stream. It returns running immediately: the core treats a
// non-terminal submit as an async task and keeps polling GetStatus.
func (g *DownloadedGenerator) Generate(req *agency.GeneratorRequest) (*agency.GeneratorResult, error) {
	jobID := fmt.Sprintf("syn_%d", time.Now().UnixNano())

	prompt := agency.CompileContentText(req.Content)
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("downloaded models need a text prompt")
	}

	// Mode follows the model's resolved modality (image vs video): downloaded
	// models of both kinds are served through this generator. El core la
	// resolvió en el catálogo (incluidos los snapshots del bridge), así que
	// acá no se vuelve a consultar: en modo bridge no hay catálogo gRPC vivo.
	mode := string(req.Modality)
	if mode == "" {
		mode = "video"
	}

	// The worker's engines read width/height; map the agency ratio onto
	// 16:9-ish dims. Registered last, so this generator only ever sees
	// downloaded models (video/image are their modalities today).
	width, height := 832, 480
	switch req.Ratio {
	case "9:16":
		width, height = 480, 832
	case "1:1":
		width, height = 512, 512
	case "4:3":
		width, height = 640, 480
	case "3:4":
		width, height = 480, 640
	}

	seed := int64(0) // 0 → worker picks a random seed
	if s := strings.TrimSpace(req.Seed); s != "" {
		if _, err := fmt.Sscanf(s, "%d", &seed); err != nil {
			seed = 0
		}
	}

	gReq := &workerpb.MediaGenerationRequest{
		JobId:     jobID,
		Mode:      mode,
		ModelName: req.Model,
		Prompt:    prompt,
		Width:     int32(width),
		Height:    int32(height),
		Fps:       16,
		Steps:     0, // 0 → worker's per-model default (spec.steps)
		CfgScale:  0, // 0 → worker's per-model default
		Seed:      seed,
	}
	if mode == "video" {
		gReq.NumFrames = int32(clampInt(req.Duration*16, 8, 257)) // 16 fps
	}

	// Dirección del worker para este job: per-request (Option B) o global.
	addr := g.effectiveAddr(req.WorkerAddr)

	// El despacho se decide UNA vez por job: la misma decisión se guarda en el
	// task (para cada poll de status) y se loguea, sin volver a consultar la
	// DB del bridge para explicar por dónde salió.
	useBridge := g.useBridgeFor(req.Model)
	dispatch := DispatchCentral
	if useBridge {
		dispatch = DispatchBridge
	}

	// Registro el task ANTES de arrancar cualquier goroutine/encolado: un
	// poll inmediato no debe ver el estado "unknown".
	g.store.init(jobID, req.Model, addr, dispatch)

	// ── Camino bridge (GPU propia del tenant): el trabajo queda en la DB y
	// la máquina del usuario lo reclama por HTTP. El estado vive en
	// worker_jobs, no en memoria. Local-first: si la GPU del tenant tiene
	// los pesos, se usa aunque haya central configurado (multi-usuario).
	if useBridge {
		payload, err := protojson.Marshal(gReq)
		if err != nil {
			g.store.fail(jobID, fmt.Sprintf("marshal job payload: %v", err))
			return nil, fmt.Errorf("marshal job payload: %w", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), worker.DefaultTimeout)
		defer cancel()
		target := g.bridgeTarget(ctx, req.Model)
		if err := g.bridge.Enqueue(ctx, g.tenantID, jobID, target, payload); err != nil {
			g.store.fail(jobID, fmt.Sprintf("enqueue: %v", err))
			return nil, fmt.Errorf("enqueue job for GPU worker: %w", err)
		}
		log.Printf("[downloaded] job %s dispatch=%s enqueued for bridge (model=%s tenant=%d worker=%s)",
			jobID, dispatch, req.Model, g.tenantID, shortWorker(target))
		return &agency.GeneratorResult{
			TaskID: jobID,
			Model:  req.Model,
			Status: config.STATUS_RUNNING,
			Raw: map[string]interface{}{
				"job_id": jobID, "engine": "inference-worker-bridge", "model": req.Model,
				"dispatch": dispatch,
			},
		}, nil
	}

	// Con un worker central configurado es imposible distinguir "fue al
	// central" de "se encoló y nadie lo reclamó" sin este motivo: queda en el
	// log y en la respuesta de status de cada poll.
	log.Printf("[downloaded] job %s dispatch=%s addr=%q (bridge=%t): la GPU del tenant no "+
		"tiene %q disponible ahora, va al worker central",
		jobID, dispatch, addr, g.bridge != nil, req.Model)

	ctx, cancel := context.WithCancel(context.Background())
	g.store.attachCancel(jobID, cancel)
	go func() {
		defer func() {
			cancel()
			g.store.detachCancel(jobID)
		}()
		if err := g.streamJob(ctx, gReq, addr); err != nil {
			log.Printf("[downloaded] job %s stream error: %v", jobID, err)
			g.store.fail(jobID, fmt.Sprintf("stream lost: %v", err))
		}
	}()

	return &agency.GeneratorResult{
		TaskID: jobID,
		Model:  req.Model,
		Status: config.STATUS_RUNNING,
		Raw: map[string]interface{}{
			"job_id": jobID, "engine": "inference-worker", "model": req.Model,
			"dispatch": dispatch,
		},
	}, nil
}

// bridgeTarget devuelve la GPU que declara el modelo (la que tiene —o está
// bajando— los pesos). Con varias máquinas emparejadas el job debe fijarse a
// esa: si lo tomara otra, fallaría al no encontrar el modelo. "" significa
// "cualquier GPU del tenant", que es la única opción honesta cuando el
// snapshot no identifica a nadie.
func (g *DownloadedGenerator) bridgeTarget(ctx context.Context, modelName string) string {
	snapshots, err := g.bridge.SnapshotsForTenant(ctx, g.tenantID)
	if err != nil {
		log.Printf("[downloaded] snapshots for pin (model=%s tenant=%d): %v", modelName, g.tenantID, err)
		return ""
	}
	for _, s := range snapshots {
		if strings.EqualFold(s.Key, modelName) {
			return s.WorkerID
		}
	}
	return ""
}

// shortWorker recorta un worker id para los logs (vacío → "any").
func shortWorker(id string) string {
	if id == "" {
		return "any"
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// streamJob consumes one GenerateMedia stream and mirrors every event into
// the task store. On COMPLETED the artifact is pulled from the worker
// (shared disk or artifact server) and re-hosted under Synapta's outputs.
func (g *DownloadedGenerator) streamJob(ctx context.Context, req *workerpb.MediaGenerationRequest, addr string) error {
	sctx, scancel := context.WithTimeout(ctx, streamTimeout)
	defer scancel()

	return g.client.StreamGeneration(sctx, addr, req, func(ev worker.StreamEvent) {
		switch ev.Status {
		case "COMPLETED":
			g.store.complete(req.JobId, ev.OutputPath)
		case "FAILED":
			msg := ev.ErrMsg
			if msg == "" {
				msg = "worker reported FAILED"
			}
			g.store.fail(req.JobId, msg)
		case "CANCELLED":
			g.store.fail(req.JobId, "cancelled")
		default:
			g.store.progress(req.JobId, ev.Percent, ev.Status)
		}
	})
}

// bridgeJobForTask devuelve el job del bridge cuando el task se encoló por
// ese camino (multi-usuario híbrido: el mismo generator mezcla tasks
// centrales en memoria y tasks de bridge en la DB del tenant).
func (g *DownloadedGenerator) bridgeJobForTask(taskID string) *workerbridge.Job {
	if g.bridge == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), worker.DefaultTimeout)
	defer cancel()
	job, err := g.bridge.JobByTaskID(ctx, g.tenantID, taskID)
	if err != nil || job == nil {
		return nil
	}
	return job
}

// GetStatus returns the task's live state. Los jobs de bridge se leen de
// worker_jobs (la GPU reporta por HTTP y el artefacto se sube al back) en
// cada poll; los centrales del mapa local en memoria.
func (g *DownloadedGenerator) GetStatus(taskID, apiKey, baseURL, endpoint string) (*agency.GeneratorResult, error) {
	// Un solo read de worker_jobs por poll: el job ya traído se mapea
	// directo en vez de consultar la DB dos veces por cada encuesta.
	if job := g.bridgeJobForTask(taskID); job != nil {
		return g.bridgeStatusFromJob(job, taskID), nil
	}
	ts, ok := g.store.get(taskID)
	if !ok {
		// Unknown task (e.g. after a restart): report running with worker
		// metadata so the reconciler keeps polling; the stream goroutine is
		// gone, so a worker-side check would be needed to conclude failure.
		return &agency.GeneratorResult{
			TaskID: taskID, Status: config.STATUS_RUNNING,
			Raw: map[string]interface{}{
				"job_id": taskID, "engine": "inference-worker", "dispatch": DispatchUnknown,
			},
		}, nil
	}
	result := &agency.GeneratorResult{
		TaskID: taskID,
		Model:  ts.Model,
		Status: ts.Status,
		Raw: map[string]interface{}{
			"job_id":        taskID,
			"engine":        "inference-worker",
			"dispatch":      ts.Dispatch,
			"stream_status": ts.StreamStatus,
			"progress":      ts.Percent,
		},
	}
	if ts.ErrMsg != "" {
		result.Error = ts.ErrMsg
	}
	if ts.LocalURL != "" {
		result.Outputs = []agency.OutputResource{{URL: ts.LocalURL, LocalURL: ts.LocalURL, Type: outputType(ts.LocalURL)}}
	}
	return result, nil
}

// outputType classifies a stored artifact by extension for the outputs list.
func outputType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp":
		return "image"
	case ".mp3", ".wav", ".flac", ".ogg":
		return "audio"
	default:
		return "video"
	}
}

// bridgeStatusFromJob mapea una fila de worker_jobs al resultado del
// generator. Recibe el job ya leído por bridgeJobForTask: un solo read de
// DB por poll, importante cuando el front encuesta cada pocos segundos.
func (g *DownloadedGenerator) bridgeStatusFromJob(job *workerbridge.Job, taskID string) *agency.GeneratorResult {
	result := &agency.GeneratorResult{
		TaskID: taskID,
		Status: config.STATUS_RUNNING,
		Raw: map[string]interface{}{
			"job_id":        taskID,
			"engine":        "inference-worker-bridge",
			"dispatch":      DispatchBridge,
			"stream_status": job.StreamStatus,
			"progress":      job.Progress,
			// Diagnóstico de la cola: a qué GPU está fijado el job y en qué
			// estado está la fila. Un job fijado a una máquina que ya no
			// reclama (o una fila que quedó 'running' sin ejecutor) se ve acá
			// sin acceso a la DB del back.
			"job_status":    job.Status,
			"worker_pinned": shortWorker(job.WorkerID),
		},
	}
	switch job.Status {
	case "completed":
		result.Status = config.STATUS_SUCCESS
		if job.ArtifactURL != "" {
			result.Outputs = []agency.OutputResource{{
				URL: job.ArtifactURL, LocalURL: job.ArtifactURL,
				Type: outputType(job.ArtifactName),
			}}
		}
	case "failed":
		result.Status = config.STATUS_FAILED
		result.Error = job.ErrorMessage
		if result.Error == "" {
			result.Error = "GPU worker reported failure"
		}
	case "cancelled":
		result.Status = config.STATUS_FAILED
		result.Error = "cancelled"
	}
	return result
}

// CancelTask drops the local stream and asks the worker to abort the job.
func (g *DownloadedGenerator) CancelTask(taskID, apiKey, baseURL, endpoint string) error {
	// Camino bridge: la cancelación viaja como flag en la fila del job y
	// llega a la GPU en la próxima respuesta de heartbeat.
	if g.bridgeJobForTask(taskID) != nil {
		ctx, cancel := context.WithTimeout(context.Background(), worker.DefaultTimeout)
		defer cancel()
		if err := g.bridge.RequestCancel(ctx, g.tenantID, taskID); err != nil {
			return fmt.Errorf("bridge cancel: %w", err)
		}
		return nil
	}
	if cancel, ok := g.store.cancelFunc(taskID); ok {
		cancel()
	}
	addr := g.store.workerAddr(taskID)
	if addr == "" {
		addr = g.addr
	}
	ctx, cancel := context.WithTimeout(context.Background(), worker.DefaultTimeout)
	defer cancel()
	accepted, err := g.client.CancelGeneration(ctx, addr, taskID)
	if err != nil {
		return fmt.Errorf("worker cancel: %w", err)
	}
	if !accepted {
		return fmt.Errorf("worker has no active job %s", taskID)
	}
	return nil
}

// BuildPayload returns the gRPC request shape for logging/preview (the
// transport is gRPC, not HTTP — there is no URL to hit).
func (g *DownloadedGenerator) BuildPayload(req *agency.GeneratorRequest) map[string]interface{} {
	return map[string]interface{}{
		"transport": "grpc",
		"service":   "maestro.inference.v1.InferenceService/GenerateMedia",
		"model":     req.Model,
		"prompt":    agency.CompileContentText(req.Content),
		"ratio":     req.Ratio,
		"duration":  req.Duration,
		"seed":      req.Seed,
	}
}

// ─── Task store ──────────────────────────────────────────────────

// Valores de Dispatch: por dónde salió el job. "bridge" es una GPU del
// tenant que lo reclama por HTTP; "central" es el gRPC directo a
// BM_WORKER_ADDR. Se decide una sola vez en Generate y se publica en el
// estado de la tarea, porque el progreso y el `engine` por sí solos no
// distinguen "corrió en mi GPU" de "lo tomó el worker central".
const (
	DispatchBridge  = "bridge"
	DispatchCentral = "central"
	// DispatchUnknown: no hay registro del job (p.ej. tras un restart del
	// back, que pierde el task store en memoria y sólo conserva el log).
	DispatchUnknown = "unknown"
)

// downloadedTask is the live state of one job, filled by the stream consumer.
type downloadedTask struct {
	Model        string
	Dispatch     string // bridge | central (decidido en Generate)
	WorkerAddr   string // dirección del worker usada para este job (per-request o global)
	Status       string // config.STATUS_RUNNING until terminal
	StreamStatus string // worker-side: SUBMITTED | LOADING_MODEL | GENERATING | ...
	Percent      float64
	ErrMsg       string
	LocalURL     string // set on COMPLETED after storing the artifact
}

// downloadedTaskStore keeps stream state between the background consumer and
// GetStatus polls.
type downloadedTaskStore struct {
	mu      sync.RWMutex
	tasks   map[string]*downloadedTask
	cancels map[string]context.CancelFunc
}

func newDownloadedTaskStore() *downloadedTaskStore {
	return &downloadedTaskStore{
		tasks:   map[string]*downloadedTask{},
		cancels: map[string]context.CancelFunc{},
	}
}

func (s *downloadedTaskStore) init(jobID, model, workerAddr, dispatch string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[jobID] = &downloadedTask{
		Model: model, Dispatch: dispatch, WorkerAddr: workerAddr,
		Status: config.STATUS_RUNNING, StreamStatus: "SUBMITTED",
	}
}

// workerAddr devuelve la dirección del worker registrada para un job (o "" si no hay).
func (s *downloadedTaskStore) workerAddr(jobID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if t, ok := s.tasks[jobID]; ok {
		return t.WorkerAddr
	}
	return ""
}

func (s *downloadedTaskStore) progress(jobID string, pct float64, streamStatus string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[jobID]; ok {
		t.Percent = pct
		t.StreamStatus = streamStatus
	}
}

// complete marks success and stores the artifact; a failed store keeps the
// task failed with an explanatory message (never silently successful).
//
// La recuperación del artefacto es I/O (copia de disco o descarga HTTP con
// timeout de 120 s), así que corre FUERA del mutex: con el lock tomado cada
// GET /status del front esperaba a que terminara la descarga y la toma se
// quedaba clavada en 100 % sin responder (el front encuesta cada 3 s y no
// vuelve a preguntar hasta que la anterior responde).
func (s *downloadedTaskStore) complete(jobID, artifactPath string) {
	s.mu.Lock()
	t, ok := s.tasks[jobID]
	if !ok {
		s.mu.Unlock()
		return
	}
	t.StreamStatus = "COMPLETED"
	t.Percent = 100
	workerAddr := t.WorkerAddr
	s.mu.Unlock()

	local := storeArtifact(artifactPath, jobID, workerAddr)

	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok = s.tasks[jobID]
	if !ok { // el job desapareció mientras se bajaba el artefacto
		return
	}
	t.LocalURL = local
	if local == "" {
		t.Status = config.STATUS_FAILED
		t.ErrMsg = fmt.Sprintf("artifact %q could not be retrieved from the worker", filepath.Base(artifactPath))
		return
	}
	t.Status = config.STATUS_SUCCESS
}

func (s *downloadedTaskStore) fail(jobID, msg string) {
	s.mu.Lock()
	t, ok := s.tasks[jobID]
	if ok {
		t.Status = config.STATUS_FAILED
		t.ErrMsg = msg
	}
	cancel, hasCancel := s.cancels[jobID]
	delete(s.cancels, jobID)
	s.mu.Unlock()
	if hasCancel {
		cancel() // release the stream goroutine
	}
}

func (s *downloadedTaskStore) attachCancel(jobID string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancels[jobID] = cancel
}

func (s *downloadedTaskStore) detachCancel(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cancels, jobID)
}

func (s *downloadedTaskStore) cancelFunc(jobID string) (context.CancelFunc, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.cancels[jobID]
	return c, ok
}

func (s *downloadedTaskStore) get(jobID string) (downloadedTask, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if t, ok := s.tasks[jobID]; ok {
		return *t, true
	}
	return downloadedTask{}, false
}

// storeArtifact re-hosts the worker-side artifact under Synapta's outputs.
// The COMPLETED event reports the worker's local path; retrieval paths:
//  1. shared disk (back y worker en el mismo host / volumen montado): copia
//     directa si la ruta reportada es absoluta y visible desde el back,
//  2. worker remoto: el artifact server del worker, en
//     BM_WORKER_ARTIFACT_BASE/artifacts/<nombre> o, si esa variable no está
//     configurada, en el puerto estándar 50052 del mismo host del worker.
//
// En el camino bridge esto no se usa: el worker sube el archivo al back
// (POST /api/v1/internal/workers/artifacts) y el back es el dueño del output.
func storeArtifact(workerPath, jobID, workerAddr string) string {
	if workerPath == "" {
		return ""
	}
	name := filepath.Base(workerPath)
	if data, err := os.ReadFile(workerPath); err == nil {
		if local, err := utils.SaveBinaryOutput(data, name); err == nil {
			log.Printf("[downloaded] job %s artifact stored locally: %s", jobID, local)
			return local
		}
	}
	if base := artifactBase(workerAddr); base != "" {
		if local, err := utils.SaveURLOutput(base+"/artifacts/"+name, name); err == nil {
			log.Printf("[downloaded] job %s artifact downloaded from artifact server: %s", jobID, local)
			return local
		} else {
			log.Printf("[downloaded] job %s artifact download failed from %s: %v", jobID, base, err)
		}
	}
	log.Printf("[downloaded] job %s artifact %q not retrievable (no shared disk, no artifact server: "+
		"definí BM_WORKER_ARTIFACT_BASE o usá el camino bridge)", jobID, name)
	return ""
}

// artifactBase resuelve la base del artifact server del worker remoto.
// BM_WORKER_ARTIFACT_BASE manda cuando está configurada; si no, se asume el
// puerto estándar del artifact server (50052) en el mismo host del worker:
// así el camino gRPC directo cierra en success sin configurar nada extra.
func artifactBase(workerAddr string) string {
	if base := strings.TrimRight(strings.TrimSpace(os.Getenv("BM_WORKER_ARTIFACT_BASE")), "/"); base != "" {
		return base
	}
	addr := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(workerAddr), "tls://"))
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return ""
	}
	return "http://" + net.JoinHostPort(host, "50052")
}

// clampInt keeps v inside [lo, hi].
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
