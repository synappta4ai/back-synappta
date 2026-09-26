// Package agencyvideo: downloaded-model generator.
//
// Runs models whose weights live on the brain-master inference worker
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
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"synapta/config"
	"synapta/internal/modules/agency"
	"synapta/internal/modules/model"
	"synapta/internal/utils"
	"synapta/internal/worker"
	workerpb "synapta/internal/worker/proto"
)

// streamTimeout bounds a whole generation stream. The worker of brain-master
// can take a long time for heavy jobs, so this is generous — it only guards
// against a silently dead worker (no events, no EOF).
const streamTimeout = 6 * time.Hour

// DownloadedGenerator serves every model type "downloaded": the worker owns
// the weights, Synapta only dispatches jobs over gRPC.
type DownloadedGenerator struct {
	client  *worker.Client
	addr    string // BM_WORKER_ADDR
	outputs string // Synapta outputs dir (informational; utils handles storage)
	store   *downloadedTaskStore
	label   string
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

func (g *DownloadedGenerator) Name() string        { return g.label }
func (g *DownloadedGenerator) ContentType() string { return "video" }

// Match claims every catalog model of type "downloaded" (resolved live
// against the worker). API models resolve as "api" and unknown names as nil:
// neither is claimed here — API generators get first pick because this
// generator is registered last in runtime.go.
func (g *DownloadedGenerator) Match(modelName string) bool {
	m := agency.LookupModel(modelName)
	return m != nil && m.Type == model.TypeDownloaded
}

// Validate checks the shared requirements plus a live worker.
func (g *DownloadedGenerator) Validate(req *agency.GeneratorRequest) error {
	if err := agency.ValidateCommon(req); err.HasErrors() {
		return err
	}
	if g.addr == "" {
		return fmt.Errorf("inference worker not configured (BM_WORKER_ADDR)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), worker.DefaultTimeout)
	defer cancel()
	if err := g.client.Ping(ctx, g.addr); err != nil {
		return fmt.Errorf("inference worker unreachable: %w", err)
	}
	return nil
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

	// Mode follows the model's live modality (image vs video): downloaded
	// models of both kinds are served through this generator.
	mode := "video"
	if m := agency.LookupModel(req.Model); m != nil && m.Modality != "" {
		mode = string(m.Modality)
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

	// Register the running task before the goroutine can race a poll.
	g.store.init(jobID, req.Model)

	ctx, cancel := context.WithCancel(context.Background())
	g.store.attachCancel(jobID, cancel)
	go func() {
		defer func() {
			cancel()
			g.store.detachCancel(jobID)
		}()
		if err := g.streamJob(ctx, gReq); err != nil {
			log.Printf("[downloaded] job %s stream error: %v", jobID, err)
			g.store.fail(jobID, fmt.Sprintf("stream lost: %v", err))
		}
	}()

	return &agency.GeneratorResult{
		TaskID: jobID,
		Model:  req.Model,
		Status: config.STATUS_RUNNING,
		Raw:    map[string]interface{}{"job_id": jobID, "engine": "inference-worker", "model": req.Model},
	}, nil
}

// streamJob consumes one GenerateMedia stream and mirrors every event into
// the task store. On COMPLETED the artifact is pulled from the worker
// (shared disk or artifact server) and re-hosted under Synapta's outputs.
func (g *DownloadedGenerator) streamJob(ctx context.Context, req *workerpb.MediaGenerationRequest) error {
	sctx, scancel := context.WithTimeout(ctx, streamTimeout)
	defer scancel()

	return g.client.StreamGeneration(sctx, g.addr, req, func(ev worker.StreamEvent) {
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

// GetStatus returns the task's live state; all real state flows in through
// the background stream.
func (g *DownloadedGenerator) GetStatus(taskID, apiKey, baseURL, endpoint string) (*agency.GeneratorResult, error) {
	ts, ok := g.store.get(taskID)
	if !ok {
		// Unknown task (e.g. after a restart): report running with worker
		// metadata so the reconciler keeps polling; the stream goroutine is
		// gone, so a worker-side check would be needed to conclude failure.
		return &agency.GeneratorResult{
			TaskID: taskID, Status: config.STATUS_RUNNING,
			Raw: map[string]interface{}{"job_id": taskID, "engine": "inference-worker"},
		}, nil
	}
	result := &agency.GeneratorResult{
		TaskID: taskID,
		Model:  ts.Model,
		Status: ts.Status,
		Raw: map[string]interface{}{
			"job_id":        taskID,
			"engine":        "inference-worker",
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

// CancelTask drops the local stream and asks the worker to abort the job.
func (g *DownloadedGenerator) CancelTask(taskID, apiKey, baseURL, endpoint string) error {
	if cancel, ok := g.store.cancelFunc(taskID); ok {
		cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), worker.DefaultTimeout)
	defer cancel()
	accepted, err := g.client.CancelGeneration(ctx, g.addr, taskID)
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

// downloadedTask is the live state of one job, filled by the stream consumer.
type downloadedTask struct {
	Model        string
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

func (s *downloadedTaskStore) init(jobID, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[jobID] = &downloadedTask{Model: model, Status: config.STATUS_RUNNING, StreamStatus: "SUBMITTED"}
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
func (s *downloadedTaskStore) complete(jobID, artifactPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[jobID]
	if !ok {
		return
	}
	t.StreamStatus = "COMPLETED"
	t.Percent = 100
	t.LocalURL = storeArtifact(artifactPath, jobID)
	if t.LocalURL == "" {
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
//  1. shared disk (local dev): direct file copy,
//  2. remote worker: the artifact server URL (BM_WORKER_ARTIFACT_BASE).
func storeArtifact(workerPath, jobID string) string {
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
	if base := strings.TrimRight(os.Getenv("BM_WORKER_ARTIFACT_BASE"), "/"); base != "" {
		if local, err := utils.SaveURLOutput(base+"/artifacts/"+name, name); err == nil {
			log.Printf("[downloaded] job %s artifact downloaded from artifact server: %s", jobID, local)
			return local
		} else {
			log.Printf("[downloaded] job %s artifact download failed: %v", jobID, err)
		}
	}
	log.Printf("[downloaded] job %s artifact %q not retrievable (no shared disk, no BM_WORKER_ARTIFACT_BASE)", jobID, name)
	return ""
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
