// Core is the per-tenant orchestration service of the agency module.
package agency

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"synapta/config"
	"synapta/internal/modules/credential"
	"synapta/internal/modules/file"
	"synapta/internal/modules/model"
	"synapta/internal/utils"
)

// TaskRecord is the in-memory task tracking entry.
type TaskRecord struct {
	TaskID    string
	ModelID   string // provider name from the model catalog
	ModelName string
	CreatedAt time.Time
	Status    string
	Result    *StatusResult
	// Naming info for local output filename
	EventName   string
	PieceCode   string
	GenerationN int
	UserHandle  string
	// Push notification
	UserID       int    // owner of the task (0 when not known)
	PushNotified bool   // true once a completion push was sent for this task
	ResourceType string // "video", "image", ... — gates the completion push
}

// GenerationStore defines what the core needs from the event domain
// (implemented by the event module's service adapter in main).
type GenerationStore interface {
	GetActiveGenerationByNumber(pieceID string, number int) (genRef, error)
	DeactivateGenerationsByNumber(pieceID string, number int) error
}

// genRef mirrors the fields the core needs from a piece generation.
type genRef struct {
	ID     string
	Status string
	TaskID string
}

// Core is the per-tenant agency orchestration service.
//
// One Core exists per tenant: it tracks that tenant's in-memory tasks and
// writes to that tenant's schema (logs, comms, assets) over its own pool.
type Core struct {
	cfg          *config.Config
	fileService  *file.Service
	credStore    *credential.Store
	outputsDir   string
	baseURL      string
	slug         string // tenant slug, used to build tenant-scoped public URLs
	pipelineGens []PipelineRunner
	costCalcs    []CostCalculator
	tasks        map[string]*TaskRecord
	assetStore   *AssetSyncStore
	logStore     *GenerationLogStore
	commStore    *ServerCommunicationStore
	genAssetSt   *GeneratedAssetStore
	genSaver     GenerationSaver
	pushNotifier PushNotifier
	mu           sync.RWMutex
}

// NewCore builds the per-tenant orchestration core.
// slug scopes the public serve URLs it generates to the tenant's own files.
func NewCore(cfg *config.Config, fileService *file.Service, credStore *credential.Store, baseURL, slug string) *Core {
	return &Core{
		cfg:          cfg,
		fileService:  fileService,
		credStore:    credStore,
		outputsDir:   cfg.OutputsDir,
		baseURL:      baseURL,
		slug:         slug,
		pipelineGens: []PipelineRunner{},
		costCalcs:    []CostCalculator{},
		tasks:        make(map[string]*TaskRecord),
	}
}

// SetStores wires the tenant-scoped stores.
func (s *Core) SetStores(assetStore *AssetSyncStore, logStore *GenerationLogStore, commStore *ServerCommunicationStore, genAssetSt *GeneratedAssetStore) {
	s.assetStore = assetStore
	s.logStore = logStore
	s.commStore = commStore
	s.genAssetSt = genAssetSt
}

// SetGenerationSaver wires the callback that persists outputs to piece_generations.
func (s *Core) SetGenerationSaver(saver GenerationSaver) { s.genSaver = saver }

// serveURL builds the tenant-scoped public URL for a file so external AI
// APIs can fetch it without credentials.
func (s *Core) serveURL(fileID string) string {
	return s.baseURL + "/api/v1/t/" + s.slug + "/files/" + fileID + "/serve"
}

// SetPushNotifier wires the push service used for async completion alerts.
func (s *Core) SetPushNotifier(pn PushNotifier) { s.pushNotifier = pn }

// RegisterGenerator registers a generator that satisfies PipelineRunner.
func (s *Core) RegisterGenerator(gen PipelineRunner) { s.pipelineGens = append(s.pipelineGens, gen) }

func (s *Core) pickGenerator(modelName string) PipelineRunner {
	for _, g := range s.pipelineGens {
		if g.Match(modelName) {
			return g
		}
	}
	return nil
}

// RegisterCalculator registers a cost calculator.
func (s *Core) RegisterCalculator(calc CostCalculator) { s.costCalcs = append(s.costCalcs, calc) }

func (s *Core) pickCalculator(modelName string) CostCalculator {
	for _, c := range s.costCalcs {
		if c.Match(modelName) {
			return c
		}
	}
	return nil
}

// resolveProvider builds the generator request credentials for a model from
// the tenant's encrypted credential store.
func (s *Core) resolveProvider(m *modelRef) (*credential.Resolve, error) {
	res, err := s.credStore.Get(string(m.CredentialProvider))
	if err != nil {
		return nil, fmt.Errorf("failed to resolve credentials for %s: %w", m.CredentialProvider, err)
	}
	if res == nil {
		return nil, fmt.Errorf("no credentials configured for provider %q — add them in admin → tenants → gestionar", m.CredentialProvider)
	}
	if res.AuthKey() == "" {
		return nil, fmt.Errorf("provider %q has no credentials for generation — add an API key or Key ID + Key Secret in admin → tenants → gestionar", m.CredentialProvider)
	}
	return res, nil
}

// modelRef is the catalog view the core works with.
type modelRef = modelEntry

// resolveRoute returns the API base URL + endpoint for a model using the
// precedence: per-tenant creds → model catalog route → provider default.
func resolveRoute(m *modelRef, creds *credential.Resolve) (baseURL, endpoint string) {
	baseURL = creds.BaseURL
	if baseURL == "" {
		baseURL = m.BaseURL
	}
	if baseURL == "" {
		baseURL = defaultBaseURLFor(string(m.CredentialProvider))
	}
	endpoint = creds.Endpoint
	if endpoint == "" {
		endpoint = m.Endpoint
	}
	if endpoint == "" {
		endpoint = defaultEndpointFor(string(m.CredentialProvider))
	}
	return baseURL, endpoint
}

// AttachCaller stamps the authenticated user (from the JWT auth middleware)
// onto a generation request. Modality handlers call this before GenerateUnified
// so server_communications can record who triggered each external API call.
func AttachCaller(req *GenerateRequest, c *gin.Context) {
	req.UserID = int(utils.UserIDFromContext(c))
	if u, ok := c.Get("username"); ok {
		if s, ok := u.(string); ok && req.UserName == "" {
			req.UserName = s
		}
	}
}

// attachAudit fills the audit fields of a ServerCommunication: who triggered
// the call (user) and which credentials were used, ALWAYS masked.
func (s *Core) attachAudit(comm *ServerCommunication, m *modelRef, creds *credential.Resolve, req *GenerateRequest) {
	comm.UserID = int64(req.UserID)
	comm.Username = req.UserName
	comm.TenantSlug = s.slug
	comm.CredentialProvider = string(m.CredentialProvider)
	comm.APIKeyMask = credential.MaskPublic(creds.APIKey)
	comm.AccessKeyMask = credential.MaskPublic(creds.AccessKeyID)
	comm.SecretKeyMask = credential.MaskPublic(creds.SecretAccessKey)
	switch {
	case creds.APIKey != "":
		comm.AuthType = "bearer"
	case creds.AccessKeyID != "" && creds.SecretAccessKey != "":
		comm.AuthType = "ak_sk"
	default:
		comm.AuthType = "none"
	}
}

// GenerateUnified validates the request, dispatches to the right generator
// and records everything in generation_logs + server_communications.
func (s *Core) GenerateUnified(req *GenerateRequest) (*GenerateResponse, error) {
	// Validar que los campos de sesión estén presentes (obligatorios para tracking).
	if req.EventID == "" || req.PieceID == "" || req.PieceCode == "" || req.GenerationNumber <= 0 {
		return nil, fmt.Errorf("event_id, piece_id, piece_code and generation_number are required for generation")
	}

	var (
		genReq        *GeneratorRequest
		modelName     string
		taskID        string
		status        = "failed"
		outputs       []OutputResource
		errLog        string
		estimatedCost float64
		costSource    string
	)

	// Defer log save — runs on every return path (including early errors)
	defer func() {
		if s.logStore == nil {
			return
		}
		reqBytes, _ := json.Marshal(req)
		if modelName == "" {
			modelName = req.Model
		}
		if modelName == "" {
			return
		}
		if taskID == "" {
			taskID = "<no-task>"
		}
		logEntry := &GenerationLog{
			TaskID:           taskID,
			ModelName:        modelName,
			UserID:           intPtrOrNil(req.UserID),
			EventID:          req.EventID,
			ProgramID:        req.ProgramID,
			PieceID:          req.PieceID,
			PieceCode:        req.PieceCode,
			GenerationNumber: req.GenerationNumber,
			Request:          string(reqBytes),
			Outputs:          outputs,
			Status:           status,
			ErrorMessage:     errLog,
			ResourceType:     req.ResourceType,
			ContentTypes:     extractContentTypes(req.Content),
			EstimatedCost:    estimatedCost,
			CostSource:       costSource,
		}
		if saveErr := s.logStore.Create(logEntry); saveErr != nil {
			fmt.Printf("failed to save generation log: %v\n", saveErr)
		}
	}()

	// Look up model in the code catalog
	m := LookupModel(req.Model)
	if m == nil {
		errLog = fmt.Sprintf("model not found: %s", req.Model)
		return nil, fmt.Errorf("model not found: %s", req.Model)
	}
	modelName = m.Name

	// Resolve per-tenant credentials (API models only: downloaded models run
	// on our own inference worker and need no external provider).
	var creds *credential.Resolve
	if m.Type != model.TypeDownloaded {
		resolved, err := s.resolveProvider(m)
		if err != nil {
			errLog = err.Error()
			return nil, err
		}
		creds = resolved
	} else {
		creds = &credential.Resolve{}
	}

	// Resolve file IDs in content to data URLs (or asset:// URIs if synced)
	resolvedContent, err := s.resolveContent(req.Content, m.Name)
	if err != nil {
		errLog = fmt.Sprintf("failed to resolve content: %v", err)
		return nil, fmt.Errorf("failed to resolve content: %w", err)
	}
	// Compute total input video duration from resolved content
	inputDuration := float64(0)
	for _, item := range resolvedContent {
		if item.Type == "video" && item.ID != "" {
			if f, err := s.fileService.GetFile(item.ID); err == nil && f != nil {
				inputDuration += f.Duration
			}
		}
	}

	// Convert to generator request
	genReq = &GeneratorRequest{
		Model:         m.Name,
		Content:       resolvedContent,
		Ratio:         req.Ratio,
		Duration:      int(req.Duration),
		CameraFixed:   req.CameraFixed != nil && *req.CameraFixed,
		Seed:          req.Seed,
		Quality:       req.Quality,
		Quantity:      req.Quantity,
		Watermark:     req.Watermark != nil && *req.Watermark,
		Resolution:    req.Resolution,
		ImageMode:     req.ImageMode,
		InputDuration: inputDuration,
		APIKey:        creds.APIKey,
		AuthKey:       creds.AuthKey(),
	}
	// Route resolution: per-tenant creds → model catalog route → provider
	// default. Downloaded models have no HTTP route (gRPC to the worker).
	if m.Type != model.TypeDownloaded {
		genReq.BaseURL, genReq.Endpoint = resolveRoute(m, creds)
	}
	if req.GenerateAudio != nil {
		genReq.GenerateAudio = *req.GenerateAudio
	}

	// Gallery models: sync unsynced assets before generation
	if m.GallerySync {
		synced, syncErr := s.GallerySyncContent(resolvedContent, m.Name)
		if syncErr == nil {
			genReq.Content = synced
		} else {
			log.Printf("[agency] gallery sync failed for %q: %v (continuing with original content)", m.Name, syncErr)
		}
	}

	// Pick generator
	gen := s.pickGenerator(m.Name)
	if gen == nil {
		errLog = fmt.Sprintf("no generator available for model: %s", m.Name)
		return nil, fmt.Errorf("no generator available for model: %s", m.Name)
	}

	// Validate request against the generator
	if err := gen.Validate(genReq); err != nil {
		errLog = fmt.Sprintf("invalid request: %v", err)
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	// Build the actual API payload (for logging and server communications)
	apiPayload := gen.BuildPayload(genReq)
	apiPayloadBytes, _ := json.Marshal(apiPayload)

	genStart := time.Now()
	result, err := gen.Generate(genReq)
	genDur := time.Since(genStart).Milliseconds()

	// Log server communication with the actual request body
	if s.commStore != nil {
		respBody := ""
		genStatus := 200
		if err != nil {
			genStatus = 0
			respBody = err.Error()
		} else if result != nil && result.Raw != nil {
			rawBytes, _ := json.Marshal(result.Raw)
			respBody = string(rawBytes)
		}
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		comm := &ServerCommunication{
			TaskID:    taskID,
			ModelName: m.Name,
			Endpoint:  genReq.BaseURL + genReq.Endpoint,
			Method:    "POST",
			// Generation submit row: phase "generate", spans just the submit call.
			Phase:           "generate",
			StartedAt:       genStart,
			FinishedAt:      time.Now(),
			TotalDurationMs: genDur,
			RequestBody:     string(apiPayloadBytes),
			ResponseBody:    respBody,
			StatusCode:      genStatus,
			DurationMs:      genDur,
			ErrorMessage:    errMsg,
		}
		s.attachAudit(comm, m, creds, req)
		_ = s.commStore.Create(comm)
	}
	log.Printf("[generate-unified] model=%q dur=%dms err=%v", m.Name, genDur, err != nil)

	if result != nil {
		taskID = result.TaskID
		status = result.Status
		if len(result.Outputs) > 0 {
			outputs = result.Outputs
		}
		// Backfill the submit trace with the task ID so the generate (POST)
		// and poll (GET) rows of the same task group into one record.
		if s.commStore != nil && taskID != "" {
			_ = s.commStore.AttachTaskIDByPhase("generate", taskID, m.Name)
		}
	}
	if err != nil {
		errLog = err.Error()
		return nil, err
	}

	// Calculate estimated cost
	if calc := s.pickCalculator(modelName); calc != nil {
		if cost, ok := calc.CalculateFromResponse(result.Raw, genReq); ok {
			estimatedCost = cost
			costSource = "api_response"
		} else if !calc.NeedsBackgroundCalc() {
			estimatedCost = calc.CalculateEstimated(genReq)
			costSource = "calculator"
		} else {
			costSource = "pending"
		}
	}

	// Track the task for status polling
	userName := req.UserName
	if userName == "" {
		userName = fmt.Sprintf("u%d", req.UserID)
	}
	terminal := result.Status == config.STATUS_SUCCESS || result.Status == config.STATUS_FAILED
	s.mu.Lock()
	s.tasks[result.TaskID] = &TaskRecord{
		TaskID: result.TaskID,
		// Anchor for submit→result durations and progress estimation.
		CreatedAt:       time.Now(),
		ModelID:         string(m.CredentialProvider),
		ModelName:       m.Name,
		Status:          result.Status,
		EventName:       req.EventName,
		PieceCode:       req.PieceCode,
		GenerationN:     req.GenerationNumber,
		UserHandle:      userName,
		UserID:          req.UserID,
		PushNotified:    terminal,
		ResourceType:    req.ResourceType,
		Result: &StatusResult{
			Status: result.Status,
			Raw:    result.Raw,
		},
	}
	s.mu.Unlock()

	// A terminal task is never polled — notify here.
	if terminal {
		s.notifyTaskCompletion(req.UserID, taskNotifyInfo{
			TaskID:           result.TaskID,
			Status:           result.Status,
			ResourceType:     req.ResourceType,
			ModelName:        m.Name,
			EventName:        req.EventName,
			PieceCode:        req.PieceCode,
			GenerationNumber: req.GenerationNumber,
		})
	}

	return &GenerateResponse{
		TaskID:  result.TaskID,
		Model:   result.Model,
		Status:  result.Status,
		Outputs: result.Outputs,
	}, nil
}

// GetStatus polls a task's status (memory → log → generator).
func (s *Core) GetStatus(taskID string) (*StatusResult, error) {
	s.mu.RLock()
	record, ok := s.tasks[taskID]
	s.mu.RUnlock()

	if !ok {
		if s.logStore == nil {
			return nil, fmt.Errorf("unknown task: %s", taskID)
		}
		logEntry, logErr := s.logStore.GetByTaskID(taskID)
		if logErr != nil || logEntry == nil {
			return nil, fmt.Errorf("unknown task: %s", taskID)
		}
		resp, err := s.statusFromLog(logEntry)
		if err != nil {
			return nil, err
		}
		if resp.Status == config.STATUS_SUCCESS && resp.LocalURL != "" {
			s.saveToGeneration(taskID, resp.VideoURL, resp.LocalURL)
		}
		return resp, nil
	}

	// Terminal tasks are answered from the stored result: no provider re-poll,
	// no duplicate local download, no extra server_communications rows.
	// Success requires stored outputs; terminal-at-submit records (sync
	// generators) keep falling through to the provider until outputs exist.
	if record.Result != nil {
		terminal := record.Status == config.STATUS_FAILED || record.Status == config.STATUS_CANCELLED
		completedWithOutputs := record.Status == config.STATUS_SUCCESS &&
			(record.Result.VideoURL != "" || record.Result.LocalURL != "" || record.Result.ImageURL != "")
		if terminal || completedWithOutputs {
			return record.Result, nil
		}
	}

	m := LookupModel(record.ModelName)
	if m == nil {
		return nil, fmt.Errorf("model for task %s not found: %s", taskID, record.ModelName)
	}
	var creds *credential.Resolve
	if m.Type != model.TypeDownloaded {
		resolved, err := s.resolveProvider(m)
		if err != nil {
			return nil, err
		}
		creds = resolved
	} else {
		creds = &credential.Resolve{}
	}
	gen := s.pickGenerator(m.Name)
	if gen == nil {
		return nil, fmt.Errorf("no generator available for model: %s", m.Name)
	}

	baseURL, endpoint := "", ""
	if m.Type != model.TypeDownloaded {
		baseURL, endpoint = resolveRoute(m, creds)
	}

	pollStart := time.Now()
	result, err := gen.GetStatus(taskID, creds.AuthKey(), baseURL, endpoint)
	pollDur := time.Since(pollStart).Milliseconds()

	// Log server communication — ONLY for meaningful polls: terminal states
	// (video ready / failed) or transport errors. Intermediate "running"
	// responses are not stored, so the log stays focused on one row per task
	// event instead of one row per polling tick.
	if s.commStore != nil && (err != nil || result == nil ||
		result.Status == config.STATUS_SUCCESS || result.Status == config.STATUS_FAILED || result.Status == config.STATUS_CANCELLED) {
		reqBytes, _ := json.Marshal(map[string]string{"task_id": taskID})
		respBody := ""
		genStatus := 200
		if err != nil {
			genStatus = 0
			respBody = err.Error()
		} else if result != nil && result.Raw != nil {
			rawBytes, _ := json.Marshal(result.Raw)
			respBody = string(rawBytes)
		}
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		comm := &ServerCommunication{
			TaskID:    taskID,
			ModelName: m.Name,
			Endpoint:  baseURL + endpoint,
			Method:    "GET",
			// Poll row: phase "poll", anchored to the task's submit time so
			// the store can compute total_duration_ms = submit → final poll.
			Phase:           "poll",
			StartedAt:       record.CreatedAt,
			DurationMs:      pollDur,
			RequestBody:     string(reqBytes),
			ResponseBody:    respBody,
			StatusCode:      genStatus,
			ErrorMessage:    errMsg,
		}
		if err == nil && result != nil && isTerminalStatus(result.Status) {
			comm.FinishedAt = time.Now()
			comm.TotalDurationMs = time.Since(record.CreatedAt).Milliseconds()
		}
		s.attachAudit(comm, m, creds, &GenerateRequest{UserID: record.UserID, UserName: record.UserHandle})
		_ = s.commStore.Create(comm)
	}

	if err != nil {
		return nil, err
	}

	statusResult := &StatusResult{
		Status: result.Status,
		Error:  result.Error,
		Raw:    result.Raw,
	}
	if len(result.Outputs) > 0 {
		statusResult.VideoURL = result.Outputs[0].URL
		statusResult.LocalURL = result.Outputs[0].LocalURL
		statusResult.ImageURL = result.Outputs[0].URL
	}

	// Sample metadata + progress estimate on every poll (monotonic writes).
	createdAt := record.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	s.trackVideoMetadata(taskID, createdAt, record.ResourceType, record.ModelName, result)

	if result.Status == config.STATUS_SUCCESS || result.Status == config.STATUS_FAILED {
		// Persist the video under the server's own outputs and serve it from
		// our domain instead of the expiring signed provider URL.
		s.ensureLocalVideo(taskID, result)
		if len(result.Outputs) > 0 {
			statusResult.VideoURL = result.Outputs[0].URL
			statusResult.LocalURL = result.Outputs[0].LocalURL
		}
		s.mu.Lock()
		notify := !record.PushNotified
		record.PushNotified = true
		record.Status = result.Status
		record.Result = statusResult
		s.mu.Unlock()
		s.updateLogWithFinalStatus(taskID, result)
		if result.Status == config.STATUS_SUCCESS {
			s.saveGeneratedAssets(taskID, result)
			s.saveToGeneration(taskID, statusResult.VideoURL, statusResult.LocalURL)
		}
		if notify {
			s.notifyTaskCompletion(record.UserID, taskNotifyInfo{
				TaskID:           taskID,
				Status:           result.Status,
				ResourceType:     record.ResourceType,
				ModelName:        record.ModelName,
				EventName:        record.EventName,
				PieceCode:        record.PieceCode,
				GenerationNumber: record.GenerationN,
			})
		}
	}
	return statusResult, nil
}

// GetStatusUnified maps the internal status into the API shape.
func (s *Core) GetStatusUnified(taskID string) (*StatusResponse, error) {
	sr, err := s.GetStatus(taskID)
	if err != nil {
		return nil, err
	}

	resp := &StatusResponse{
		Status:  sr.Status,
		Error:   sr.Error,
		Raw:     sr.Raw,
		Outputs: []OutputResource{},
	}

	// Prefer the server-owned copy of the video over the expiring signed
	// provider URL so playback survives provider URL expiry.
	localURL := sr.LocalURL
	providerURL := sr.VideoURL
	if localURL == "" {
		localURL = providerURL
	}
	if localURL != "" {
		resp.Outputs = append(resp.Outputs, OutputResource{
			URL:      s.absoluteOutputURL(localURL),
			LocalURL: localURL,
			Type:     "video",
		})
	}
	if sr.ImageURL != "" && sr.ImageURL != sr.VideoURL && sr.ImageURL != localURL {
		resp.Outputs = append(resp.Outputs, OutputResource{URL: sr.ImageURL, Type: "image"})
	}

	// Progress: provider field when present, otherwise our persisted estimate.
	resp.ProgressPercent = providerProgressPct(sr.Raw)
	if resp.ProgressPercent == 0 && s.logStore != nil {
		if p, err := s.logStore.GetProgressByTaskID(taskID); err == nil {
			resp.ProgressPercent = p
		}
	}
	switch sr.Status {
	case config.STATUS_SUCCESS:
		resp.ProgressPercent = 100
	case config.STATUS_FAILED, config.STATUS_CANCELLED:
		if resp.ProgressPercent == 0 {
			resp.ProgressPercent = 0
		}
	}
	if rawMap, ok := sr.Raw.(map[string]interface{}); ok {
		for _, key := range []string{"progress", "percentage", "task_progress"} {
			if v, exists := rawMap[key]; exists {
				resp.Progress = v
				break
			}
		}
	}
	return resp, nil
}

// CancelTask cancels a running task.
func (s *Core) CancelTask(taskID string) error {
	s.mu.RLock()
	record, ok := s.tasks[taskID]
	s.mu.RUnlock()

	if !ok {
		return fmt.Errorf("unknown task: %s", taskID)
	}

	m := LookupModel(record.ModelName)
	if m == nil {
		return fmt.Errorf("model for task %s not found", taskID)
	}
	var creds *credential.Resolve
	if m.Type != model.TypeDownloaded {
		resolved, err := s.resolveProvider(m)
		if err != nil {
			return err
		}
		creds = resolved
	} else {
		creds = &credential.Resolve{}
	}
	gen := s.pickGenerator(m.Name)
	if gen == nil {
		return fmt.Errorf("no generator available for model: %s", m.Name)
	}
	baseURL, endpoint := "", ""
	if m.Type != model.TypeDownloaded {
		baseURL, endpoint = resolveRoute(m, creds)
	}
	return gen.CancelTask(taskID, creds.AuthKey(), baseURL, endpoint)
}

// PreviewPayload builds the AI API payload without sending it or saving logs.
func (s *Core) PreviewPayload(req *GenerateRequest) (*PreviewPayloadResponse, error) {
	m := LookupModel(req.Model)
	if m == nil {
		return nil, fmt.Errorf("model not found: %s", req.Model)
	}
	var creds *credential.Resolve
	if m.Type != model.TypeDownloaded {
		resolved, err := s.resolveProvider(m)
		if err != nil {
			return nil, err
		}
		creds = resolved
	} else {
		creds = &credential.Resolve{}
	}

	resolvedContent, err := s.resolveContent(req.Content, m.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve content: %w", err)
	}

	genReq := &GeneratorRequest{
		Model:       m.Name,
		Content:     resolvedContent,
		Ratio:       req.Ratio,
		Duration:    int(req.Duration),
		CameraFixed: req.CameraFixed != nil && *req.CameraFixed,
		Seed:        req.Seed,
		Quality:     req.Quality,
		Quantity:    req.Quantity,
		Watermark:   req.Watermark != nil && *req.Watermark,
		Resolution:  req.Resolution,
		ImageMode:   req.ImageMode,
		APIKey:      creds.APIKey,
		AuthKey:     creds.AuthKey(),
	}
	// Route resolution: per-tenant creds → model catalog route → provider
	// default. Downloaded models have no HTTP route (gRPC to the worker).
	if m.Type != model.TypeDownloaded {
		genReq.BaseURL, genReq.Endpoint = resolveRoute(m, creds)
	}
	if req.GenerateAudio != nil {
		genReq.GenerateAudio = *req.GenerateAudio
	}

	gen := s.pickGenerator(m.Name)
	if gen == nil {
		return nil, fmt.Errorf("no generator available for model: %s", m.Name)
	}

	payload := gen.BuildPayload(genReq)
	return &PreviewPayloadResponse{
		Model:       m.Name,
		Endpoint:    genReq.Endpoint,
		Payload:     payload,
		ContentType: gen.ContentType(),
	}, nil
}

// ─── Internal helpers ───────────────────────────────────────────

// isTerminalStatus reports whether a generation status ends the task.
func isTerminalStatus(status string) bool {
	switch status {
	case config.STATUS_SUCCESS, config.STATUS_FAILED, config.STATUS_CANCELLED:
		return true
	}
	return false
}

// statusFromLog recovers a task's status from the log after a restart.
func (s *Core) statusFromLog(logEntry *GenerationLog) (*StatusResult, error) {
	if logEntry.Status == config.STATUS_SUCCESS || logEntry.Status == config.STATUS_FAILED {
		sr := &StatusResult{
			Status: logEntry.Status,
			Error:  logEntry.ErrorMessage,
		}
		if len(logEntry.Outputs) > 0 {
			sr.VideoURL = logEntry.Outputs[0].URL
			sr.LocalURL = logEntry.Outputs[0].LocalURL
			sr.ImageURL = logEntry.Outputs[0].URL
		}
		return sr, nil
	}

	m := LookupModel(logEntry.ModelName)
	if m == nil {
		return &StatusResult{Status: logEntry.Status, Error: "model not found for task " + logEntry.TaskID}, nil
	}
	gen := s.pickGenerator(m.Name)
	if gen == nil {
		return &StatusResult{Status: logEntry.Status}, nil
	}
	var creds *credential.Resolve
	if m.Type != model.TypeDownloaded {
		resolved, rerr := s.resolveProvider(m)
		if rerr != nil {
			return &StatusResult{Status: logEntry.Status, Error: rerr.Error()}, nil
		}
		creds = resolved
	} else {
		creds = &credential.Resolve{}
	}
	baseURL, endpoint := "", ""
	if m.Type != model.TypeDownloaded {
		baseURL, endpoint = resolveRoute(m, creds)
	}

	result, err := gen.GetStatus(logEntry.TaskID, creds.AuthKey(), baseURL, endpoint)
	if err != nil {
		return &StatusResult{Status: logEntry.Status, Error: err.Error()}, nil
	}

	statusResult := &StatusResult{
		Status: result.Status,
		Error:  result.Error,
		Raw:    result.Raw,
	}
	if len(result.Outputs) > 0 {
		statusResult.VideoURL = result.Outputs[0].URL
		statusResult.LocalURL = result.Outputs[0].LocalURL
		statusResult.ImageURL = result.Outputs[0].URL
	}

	// Sample metadata + progress estimate on every poll (monotonic writes).
	s.trackVideoMetadata(logEntry.TaskID, logEntry.CreatedAt, logEntry.ResourceType, logEntry.ModelName, result)

	if result.Status == config.STATUS_SUCCESS || result.Status == config.STATUS_FAILED {
		s.ensureLocalVideo(logEntry.TaskID, result)
		if len(result.Outputs) > 0 {
			statusResult.VideoURL = result.Outputs[0].URL
			statusResult.LocalURL = result.Outputs[0].LocalURL
		}
		s.updateLogWithFinalStatus(logEntry.TaskID, result)
		if result.Status == config.STATUS_SUCCESS {
			s.saveGeneratedAssets(logEntry.TaskID, result)
		}
		if logEntry.UserID != nil {
			s.notifyTaskCompletion(*logEntry.UserID, taskNotifyInfo{
				TaskID:           logEntry.TaskID,
				Status:           result.Status,
				ResourceType:     logEntry.ResourceType,
				ModelName:        logEntry.ModelName,
				EventName:        logEntry.EventName,
				PieceCode:        logEntry.PieceCode,
				GenerationNumber: logEntry.GenerationNumber,
			})
		}
	}
	return statusResult, nil
}

// updateLogWithFinalStatus updates the generation log when an async task completes.
func (s *Core) updateLogWithFinalStatus(taskID string, result *GeneratorResult) {
	if s.logStore == nil {
		return
	}
	logEntry, logErr := s.logStore.GetByTaskID(taskID)
	if logErr != nil || logEntry == nil {
		return
	}
	// Persist outputs pointing at the server-owned copy when available.
	outputs := make([]OutputResource, len(result.Outputs))
	copy(outputs, result.Outputs)
	for i := range outputs {
		if outputs[i].LocalURL != "" {
			outputs[i].URL = s.absoluteOutputURL(outputs[i].LocalURL)
		}
	}
	if saveErr := s.logStore.UpdateByTaskID(taskID, outputs, result.Status, result.Error); saveErr != nil {
		fmt.Printf("failed to update generation log for task %s: %v\n", taskID, saveErr)
	}
}

// saveGeneratedAssets records output URLs as generated_assets (status: pending).
func (s *Core) saveGeneratedAssets(taskID string, result *GeneratorResult) {
	if s.genAssetSt == nil || s.logStore == nil {
		return
	}
	logEntry, logErr := s.logStore.GetByTaskID(taskID)
	if logErr != nil || logEntry == nil {
		return
	}
	for _, out := range result.Outputs {
		asset := &GeneratedAsset{
			TaskID:           taskID,
			ModelName:        logEntry.ModelName,
			UserID:           logEntry.UserID,
			EventID:          logEntry.EventID,
			ProgramID:        logEntry.ProgramID,
			PieceID:          logEntry.PieceID,
			PieceCode:        logEntry.PieceCode,
			GenerationNumber: logEntry.GenerationNumber,
			OriginalURL:      out.URL,
			Status:           "pending",
		}
		if asset.OriginalURL == "" {
			asset.OriginalURL = out.LocalURL
		}
		if asset.OriginalURL == "" {
			continue
		}
		if err := s.genAssetSt.Create(asset); err != nil {
			fmt.Printf("failed to save generated asset for task %s: %v\n", taskID, err)
		}
	}
}

// saveToGeneration persists output URLs into the piece's generation row.
func (s *Core) saveToGeneration(taskID, videoURL, localURL string) {
	if s.genSaver == nil || s.logStore == nil {
		return
	}
	logEntry, logErr := s.logStore.GetByTaskID(taskID)
	if logErr != nil || logEntry == nil {
		return
	}
	if logEntry.PieceID == "" || logEntry.GenerationNumber <= 0 {
		return
	}
	if err := s.genSaver(logEntry.PieceID, logEntry.GenerationNumber, videoURL, localURL, taskID); err != nil {
		fmt.Printf("failed to save generation output for task %s: %v\n", taskID, err)
		return
	}
	outputs := []OutputResource{{URL: s.absoluteOutputURL(localURL), LocalURL: localURL, Type: "video"}}
	if err := s.logStore.UpdateByTaskID(taskID, outputs, logEntry.Status, logEntry.ErrorMessage); err != nil {
		fmt.Printf("failed to update generation log outputs for task %s: %v\n", taskID, err)
	}
}

// taskNotifyInfo carries the context used to build a completion push.
type taskNotifyInfo struct {
	TaskID           string
	Status           string
	ResourceType     string
	ModelName        string
	EventName        string
	PieceCode        string
	GenerationNumber int
}

// notifyTaskCompletion sends a Web Push to the task owner (video only today).
func (s *Core) notifyTaskCompletion(userID int, info taskNotifyInfo) {
	if s.pushNotifier == nil || userID <= 0 || info.ResourceType != "video" {
		return
	}

	title, ntype := "🎬 Video ready", "video-ready"
	if info.Status == config.STATUS_FAILED {
		title, ntype = "⚠️ Video failed", "video-failed"
	}

	body := strings.TrimSpace(info.EventName)
	if body == "" {
		body = info.ModelName
	} else if info.ModelName != "" {
		body += " · " + info.ModelName
	}
	if info.PieceCode != "" {
		if body != "" {
			body += "\n"
		}
		body += info.PieceCode
		if info.GenerationNumber > 0 {
			body += " · G" + fmt.Sprintf("%d", info.GenerationNumber)
		}
	}

	go s.pushNotifier.SendToUser(int64(userID), title, body, map[string]string{
		"type":    ntype,
		"task_id": info.TaskID,
	})
}

// resolveContent maps file IDs to public serve URLs, or asset:// URIs when
// the file is already synced with the model's gallery.
func (s *Core) resolveContent(items []ContentItem, modelName string) ([]ContentItem, error) {
	resolved := make([]ContentItem, len(items))
	for i, item := range items {
		ci := ContentItem{
			Type: item.Type,
			Text: item.Text,
			Name: item.Name,
			ID:   item.ID,
		}

		if item.Type != "text" && item.ID != "" {
			// Check if file is synced to this model's asset library
			if modelName != "" && s.assetStore != nil {
				synced, err := s.assetStore.GetByModelAndFile(modelName, item.ID)
				if err == nil && synced != nil && synced.Status == "active" && synced.AssetID != "" {
					ci.DataURL = synced.ReferenceURI
					resolved[i] = ci
					continue
				}
			}

			// Verify file exists
			f, err := s.fileService.GetFile(item.ID)
			if err != nil {
				return nil, fmt.Errorf("content[%d] file %s: %w", i, item.ID, err)
			}
			if f == nil {
				return nil, fmt.Errorf("content[%d] file %s not found", i, item.ID)
			}
			ci.DataURL = s.serveURL(item.ID)
		}
		resolved[i] = ci
	}
	return resolved, nil
}

// ─── Gallery sync ───────────────────────────────────────────────

// SyncAsset uploads a local file to the model's asset library and stores the mapping.
func (s *Core) SyncAsset(modelName, fileID string) (*SyncAssetResult, error) {
	if s.assetStore == nil {
		return nil, fmt.Errorf("asset sync store not available")
	}

	m := LookupModel(modelName)
	if m == nil {
		return nil, fmt.Errorf("model not found: %s", modelName)
	}
	creds, err := s.resolveProvider(m)
	if err != nil {
		return nil, err
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return nil, fmt.Errorf("provider %q has no AK/SK configured", m.CredentialProvider)
	}
	groupID := creds.Extra // convention: asset group stored in extra JSON-free field
	if groupID == "" {
		return nil, fmt.Errorf("no asset group configured for provider %q (set extra=group_id)", m.CredentialProvider)
	}

	f, err := s.fileService.GetFile(fileID)
	if err != nil {
		return nil, fmt.Errorf("failed to get file: %w", err)
	}
	if f == nil {
		return nil, fmt.Errorf("file not found")
	}

	record := &ModelAsset{
		ModelID:      modelName,
		FileID:       fileID,
		AssetGroupID: groupID,
		Status:       "syncing",
		AssetType:    strings.ToUpper(DetectAssetType(f.MimeType)),
	}
	if err := s.assetStore.Create(record); err != nil {
		return nil, fmt.Errorf("failed to create sync record: %w", err)
	}

	fileURL := s.serveURL(fileID)
	uploadName := f.Filename
	normalized := false

	// Auto-normalize images that violate BytePlus dimension limits.
	if IsImageMime(f.MimeType) {
		if servePath, err := s.fileService.GetServePath(fileID); err == nil {
			if data, err := normalizeImage(servePath, "pad"); err != nil {
				log.Printf("[sync-asset] normalizeImage error: %v", err)
			} else if data != nil {
				up, upErr := s.fileService.Upload(data, "normalized-"+fileID+".jpg", "temp", "temp", true)
				if upErr != nil || up == nil {
					log.Printf("[sync-asset] upload normalized copy failed: %v", upErr)
				} else {
					fileURL = s.serveURL(up.ID)
					uploadName = "normalized-" + f.Filename
					normalized = true
				}
			}
		}
	}

	api := NewAssetAPI(creds.AccessKeyID, creds.SecretAccessKey, groupID)
	api.SetCommStore(s.commStore)
	result, err := api.CreateAsset(fileURL, uploadName, DetectAssetType(f.MimeType), "")
	if err != nil {
		_ = s.assetStore.UpdateStatus(record.ID, "failed", err.Error(), "", "", "", "")
		return &SyncAssetResult{
			ID: record.ID, ModelID: modelName, FileID: fileID,
			Status: "failed", ErrorMessage: err.Error(), Normalized: normalized,
		}, nil
	}

	assetID, _ := result["id"].(string)

	// Poll until Active (up to ~2 min)
	assetStatus, assetURL, assetType := "", "", ""
	for i := 0; i < 20; i++ {
		statusResult, err := api.GetAsset(assetID, "")
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		assetStatus, _ = statusResult["Status"].(string)
		if url, ok := statusResult["URL"].(string); ok && url != "" {
			assetURL = url
		}
		if at, ok := statusResult["AssetType"].(string); ok && at != "" {
			assetType = strings.ToUpper(at)
		}
		if assetStatus == "Active" || assetStatus == "Failed" {
			break
		}
		time.Sleep(3 * time.Second)
	}

	referenceURI := BuildReferenceURI(modelName, assetID, assetURL)

	finalStatus := "active"
	errMsg := ""
	if assetStatus != "Active" {
		finalStatus = "failed"
		errMsg = fmt.Sprintf("asset did not become Active, last status: %s", assetStatus)
	}
	if err := s.assetStore.UpdateStatus(record.ID, finalStatus, errMsg, assetID, assetURL, assetType, referenceURI); err != nil {
		return nil, fmt.Errorf("failed to update sync status: %w", err)
	}

	return &SyncAssetResult{
		ID: record.ID, ModelID: modelName, FileID: fileID,
		AssetID: assetID, AssetGroupID: groupID, Status: finalStatus,
		ErrorMessage: errMsg, ReferenceURI: referenceURI, Normalized: normalized,
	}, nil
}

// SyncAssetResult reports one asset-sync attempt.
type SyncAssetResult struct {
	ID           string `json:"id"`
	ModelID      string `json:"model_id"`
	FileID       string `json:"file_id"`
	AssetID      string `json:"asset_id,omitempty"`
	AssetGroupID string `json:"asset_group_id,omitempty"`
	Status       string `json:"status"`
	ErrorMessage string `json:"error_message,omitempty"`
	ReferenceURI string `json:"reference_uri,omitempty"`
	Normalized   bool   `json:"normalized,omitempty"`
}

// ListSyncedAssets returns all synced assets for a model.
func (s *Core) ListSyncedAssets(modelName string) ([]ModelAsset, error) {
	if s.assetStore == nil {
		return nil, fmt.Errorf("asset sync store not available")
	}
	return s.assetStore.ListByModel(modelName)
}

// GallerySyncContent resolves non-text content items for gallery models:
// each unsynced asset is uploaded to the model's gallery and its DataURL
// replaced with the model-specific reference URI.
func (s *Core) GallerySyncContent(items []ContentItem, modelName string) ([]ContentItem, error) {
	if s.assetStore == nil {
		return nil, fmt.Errorf("asset sync store not available")
	}
	m := LookupModel(modelName)
	if m == nil {
		return nil, fmt.Errorf("model not found: %s", modelName)
	}
	creds, err := s.resolveProvider(m)
	if err != nil {
		return nil, err
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return nil, fmt.Errorf("no AK/SK configured for gallery sync (provider %q)", m.CredentialProvider)
	}

	result := make([]ContentItem, len(items))
	copy(result, items)

	for i, item := range items {
		if item.Type == "text" || item.ID == "" {
			continue
		}
		synced, err := s.assetStore.GetByModelAndFile(m.Name, item.ID)
		if err == nil && synced != nil && synced.Status == "active" && synced.AssetID != "" {
			result[i].DataURL = synced.ReferenceURI
			continue
		}
		resp, err := s.SyncAsset(modelName, item.ID)
		if err == nil && resp.Status == "active" && resp.AssetID != "" {
			result[i].DataURL = resp.ReferenceURI
		} else if err != nil {
			log.Printf("[gallery-sync] item[%d] sync failed: %v", i, err)
		}
	}
	return result, nil
}

// ─── Provider URL defaults ──────────────────────────────────────

// defaultBaseURLFor returns the API base URL for a credential provider.
func defaultBaseURLFor(p string) string {
	switch p {
	case "byteplus":
		return "https://ark.ap-southeast.bytepluses.com"
	case "gemini":
		return "https://generativelanguage.googleapis.com/v1beta"
	case "anthropic":
		return "https://api.anthropic.com/v1"
	default:
		return ""
	}
}

// defaultEndpointFor returns the generation endpoint path per provider.
func defaultEndpointFor(p string) string {
	switch p {
	case "byteplus":
		return "/api/v3/contents/generations/tasks"
	case "gemini":
		return ""
	case "anthropic":
		return "/messages"
	default:
		return ""
	}
}

// Logs accessors used by the handler.

// ListLogs returns paginated generation logs.
func (s *Core) ListLogs(f ListLogsFilter) (*ListLogsResponse, error) {
	if s.logStore == nil {
		return nil, fmt.Errorf("log store not available")
	}
	logs, total, err := s.logStore.ListByFilter(f)
	if err != nil {
		return nil, err
	}
	totalPages := (total + f.Limit - 1) / f.Limit
	if totalPages < 1 {
		totalPages = 1
	}
	return &ListLogsResponse{Logs: logs, Total: total, Page: f.Page, Limit: f.Limit, TotalPages: totalPages}, nil
}

// RecentTasksForUser returns the caller's recent generations (any modality),
// newest first — powers the studio take-reel hydration after a page reload.
func (s *Core) RecentTasksForUser(userID int64, limit int) ([]GenerationLog, error) {
	if s.logStore == nil {
		return nil, fmt.Errorf("log store not available")
	}
	return s.logStore.ListRecentByUser(userID, limit)
}

// SumLogsCost returns the total estimated cost for filtered logs.
func (s *Core) SumLogsCost(f ListLogsFilter) (*CostSummaryResponse, error) {
	if s.logStore == nil {
		return nil, fmt.Errorf("log store not available")
	}
	total, err := s.logStore.SumCostByFilter(f)
	if err != nil {
		return nil, err
	}
	return &CostSummaryResponse{TotalCost: total}, nil
}

// GetLog returns one generation log by ID.
func (s *Core) GetLog(id string) (*GenerationLog, error) {
	if s.logStore == nil {
		return nil, fmt.Errorf("log store not available")
	}
	logEntry, err := s.logStore.GetByID(id)
	if err != nil {
		return nil, err
	}
	if logEntry == nil {
		return nil, fmt.Errorf("generation log not found: %s", id)
	}
	return logEntry, nil
}

// ListComms returns paginated server communications.
func (s *Core) ListComms(filter ServerCommFilter) (*ServerCommListResponse, error) {
	if s.commStore == nil {
		return nil, fmt.Errorf("server communication store not available")
	}
	return s.commStore.List(filter)
}

// GetComm returns one server communication by ID.
func (s *Core) GetComm(id string) (*ServerCommunication, error) {
	if s.commStore == nil {
		return nil, fmt.Errorf("server communication store not available")
	}
	return s.commStore.GetByID(id)
}

// ListAssets returns generated assets of a piece.
func (s *Core) ListAssets(pieceID string) ([]GeneratedAsset, error) {
	if s.genAssetSt == nil {
		return nil, fmt.Errorf("generated asset store not available")
	}
	assets, err := s.genAssetSt.ListByPiece(pieceID)
	if assets == nil {
		assets = []GeneratedAsset{}
	}
	return assets, err
}

// ListGeneratedVideos returns completed generations that produced outputs
// (videos today), enriched with project/piece/user context for the admin
// gallery. Logs stay task-focused: one entry per task. When eventID is set
// only generations of that project (event) are returned.
func (s *Core) ListGeneratedVideos(page, limit int, eventID string) (*ListLogsResponse, error) {
	return s.listGeneratedByModality(page, limit, config.ModalityVideo, eventID)
}

// ListGeneratedImages returns completed image generations with the same
// enriched context as videos, for the admin images gallery. Optional eventID
// narrows the list to one project.
func (s *Core) ListGeneratedImages(page, limit int, eventID string) (*ListLogsResponse, error) {
	return s.listGeneratedByModality(page, limit, config.ModalityImage, eventID)
}

func (s *Core) listGeneratedByModality(page, limit int, modality, eventID string) (*ListLogsResponse, error) {
	if s.logStore == nil {
		return nil, fmt.Errorf("log store not available")
	}
	filter := ListLogsFilter{
		Page:         page,
		Limit:        limit,
		Status:       config.STATUS_SUCCESS,
		ResourceType: modality,
		HasOutputs:   true,
		EventID:      eventID,
	}
	logs, total, err := s.logStore.ListByFilter(filter)
	if err != nil {
		return nil, err
	}
	totalPages := (total + filter.Limit - 1) / filter.Limit
	if totalPages < 1 {
		totalPages = 1
	}
	return &ListLogsResponse{Logs: logs, Total: total, Page: filter.Page, Limit: filter.Limit, TotalPages: totalPages}, nil
}
