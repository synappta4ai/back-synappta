package model

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"synapta/internal/utils"
	inferencev1 "synapta/internal/worker/proto"
)

// CredentialLister is the tenant credential capability the public model
// listings need: only API models whose provider has a stored authentication
// secret are offered as generation choices.
type CredentialLister interface {
	UsableProviders() (map[string]bool, error)
}

// CredentialResolver supplies the tenant-scoped credential lister. It returns
// false when the request has no tenant bundle, in which case listings fail
// closed instead of silently offering models the tenant cannot run.
type CredentialResolver func(c *gin.Context) (CredentialLister, bool)

// Module exposes the model catalog.
//
// The public catalog has two sources:
//   - "api" models: static entries defined in catalog.go, filtered by the
//     tenant's stored provider credentials.
//   - "downloaded" models: served live by the brain-master inference worker
//     (gRPC), filtered by the worker's availability. Nothing is cached:
//     every request re-reads the worker.
//
// The /worker/models management routes are the full worker view (including
// unavailable entries, download progress and on-disk size) with download,
// cancel and delete operations.
type Module struct {
	resolve CredentialResolver
}

func NewModule(resolve CredentialResolver) *Module { return &Module{resolve: resolve} }

func (m *Module) Name() string { return "models" }

func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, adminMw gin.HandlerFunc) {
	g := rg.Group("/models")
	g.Use(authMw, tenantMw)
	{
		g.GET("", m.List)
		g.GET("/:modality", m.ListByModality)
	}
	w := rg.Group("/worker")
	w.Use(authMw, tenantMw)
	{
		w.GET("/status", m.WorkerStatus)

		admin := w.Group("", adminMw)
		{
			admin.GET("/models", m.WorkerModels)
			admin.POST("/models/:key/download", m.WorkerDownloadModel)
			admin.POST("/models/:key/cancel", m.WorkerCancelDownload)
			admin.DELETE("/models", m.WorkerDeleteAllModels)
			admin.DELETE("/models/:key", m.WorkerDeleteModel)
		}
	}
}

// List handles GET /api/v1/models?modality=video&type=downloaded
//
// type is optional: "api" | "downloaded". Without it, both usable sources are
// merged — credentialed API models plus available downloaded models. Models
// without credentials or reported unavailable never appear here: they are
// managed through the admin /worker/models routes instead.
func (m *Module) List(c *gin.Context) {
	modality := c.Query("modality")
	if modality != "" && !IsValidModality(modality) {
		utils.BadRequest(c, "invalid modality: "+modality)
		return
	}
	typeFilter := c.Query("type")
	if typeFilter != "" && !IsValidType(typeFilter) {
		utils.BadRequest(c, "invalid type: "+typeFilter+" (use api | downloaded)")
		return
	}

	// Explicit type → single source, errors propagate.
	if typeFilter != "" {
		models, handled := m.listByType(c, typeFilter, modality)
		if handled {
			return
		}
		utils.Success(c, models)
		return
	}

	// Merged view: usable API models + whatever usable models the worker
	// reports now. A dead worker degrades to API-only, never to invented
	// downloaded entries.
	models, handled := m.usableAPIModels(c, Modality(modality))
	if handled {
		return
	}
	live, lerr := ListUsable(c.Request.Context(), workerAddr())
	if lerr != nil {
		logWorkerError(c, lerr) // API models are still served
	} else {
		models = append(models, filterByModalityOrAll(live, modality)...)
	}
	utils.Success(c, models)
}

// ListByModality handles GET /api/v1/models/:modality
func (m *Module) ListByModality(c *gin.Context) {
	modality := c.Param("modality")
	if !IsValidModality(modality) {
		utils.NotFound(c, "unknown modality: "+modality)
		return
	}
	typeFilter := c.Query("type")
	if typeFilter != "" && !IsValidType(typeFilter) {
		utils.BadRequest(c, "invalid type: "+typeFilter+" (use api | downloaded)")
		return
	}

	if typeFilter != "" {
		models, handled := m.listByType(c, typeFilter, modality)
		if handled {
			return
		}
		utils.Success(c, models)
		return
	}

	models, handled := m.usableAPIModels(c, Modality(modality))
	if handled {
		return
	}
	live, lerr := ListUsable(c.Request.Context(), workerAddr())
	if lerr != nil {
		logWorkerError(c, lerr)
	} else {
		models = append(models, filterByModalityOrAll(live, modality)...)
	}
	utils.Success(c, models)
}

// WorkerStatus handles GET /api/v1/worker/status — live reachability and GPU
// snapshot of the inference worker. Never cached.
func (m *Module) WorkerStatus(c *gin.Context) {
	st, err := WorkerStatusNow(c.Request.Context(), workerAddr())
	if err != nil {
		// Report the degraded status with 200: the endpoint answered
		// truthfully — the worker is just not reachable right now.
		if st == nil {
			st = &WorkerStatus{Configured: workerAddr() != ""}
		}
		utils.Success(c, st)
		return
	}
	utils.Success(c, st)
}

// WorkerModels handles GET /api/v1/worker/models — the full management view:
// every worker catalog entry with availability, download progress and on-disk
// size. Unavailable entries are included on purpose: this is where an admin
// downloads them, retries failures, or deletes weights.
func (m *Module) WorkerModels(c *gin.Context) {
	models, err := ListLive(c.Request.Context(), workerAddr())
	if err != nil {
		utils.InternalError(c, workerMessage(err))
		return
	}
	if models == nil {
		models = []Model{}
	}
	utils.Success(c, models)
}

// WorkerDownloadModel handles POST /api/v1/worker/models/:key/download.
func (m *Module) WorkerDownloadModel(c *gin.Context) {
	m.operateOnWeights(c, "DownloadModel", false)
}

// WorkerCancelDownload handles POST /api/v1/worker/models/:key/cancel.
func (m *Module) WorkerCancelDownload(c *gin.Context) {
	m.operateOnWeights(c, "DownloadModel", true)
}

// WorkerDeleteModel handles DELETE /api/v1/worker/models/:key.
func (m *Module) WorkerDeleteModel(c *gin.Context) {
	key, client, addr, ok := workerWriteTarget(c)
	if !ok {
		return
	}
	resp, err := client.DeleteModel(c.Request.Context(), addr, key)
	if err != nil {
		workerRPCError(c, "DeleteModel", err)
		return
	}
	utils.Success(c, toModelOperation(resp))
}

// BulkDeleteResult summarizes a DELETE /api/v1/worker/models call.
type BulkDeleteResult struct {
	Deleted []string `json:"deleted"`
	// Failed maps model keys that kept their weights to the reason.
	Failed     map[string]string `json:"failed"`
	FreedBytes int64             `json:"freed_bytes"`
}

// WorkerDeleteAllModels handles DELETE /api/v1/worker/models — the
// "uninstall everything" option: removes every downloaded weight from the
// worker disk. Models that cannot be deleted (download in progress, no
// disk weights) are reported in Failed without aborting the rest.
func (m *Module) WorkerDeleteAllModels(c *gin.Context) {
	client, addr, ok := workerClientOnly(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	models, err := ListLive(ctx, addr)
	if err != nil {
		utils.InternalError(c, workerMessage(err))
		return
	}
	result := BulkDeleteResult{Deleted: []string{}, Failed: map[string]string{}}
	for _, model := range models {
		if model.Type != TypeDownloaded || model.Downloaded == nil {
			continue
		}
		if model.Downloaded.SizeBytes == 0 && !model.Downloaded.Downloading && model.Downloaded.DownloadError == "" {
			continue // nothing on disk for this entry
		}
		if _, derr := client.DeleteModel(ctx, addr, model.Name); derr != nil {
			st, _ := status.FromError(derr)
			msg := strings.TrimSpace(st.Message())
			if msg == "" {
				msg = derr.Error()
			}
			result.Failed[model.Name] = msg
			continue
		}
		result.Deleted = append(result.Deleted, model.Name)
		result.FreedBytes += model.Downloaded.SizeBytes
	}
	utils.Success(c, result)
}

// operateOnWeights starts or cancels a background weight download.
func (m *Module) operateOnWeights(c *gin.Context, op string, cancel bool) {
	key, client, addr, ok := workerWriteTarget(c)
	if !ok {
		return
	}
	resp, err := client.DownloadModel(c.Request.Context(), addr, key, cancel)
	if err != nil {
		workerRPCError(c, op, err)
		return
	}
	utils.Created(c, toModelOperation(resp))
}

// listByType serves one explicit source for the public catalog. The boolean
// reports whether an error response was already written.
func (m *Module) listByType(c *gin.Context, typeFilter, modality string) ([]Model, bool) {
	if ModelType(strings.ToLower(typeFilter)) == TypeAPI {
		models, handled := m.usableAPIModels(c, Modality(modality))
		if handled {
			return nil, true
		}
		return models, false
	}
	models, err := ListLiveFiltered(c.Request.Context(), workerAddr(), typeFilter, modality)
	if err != nil {
		utils.InternalError(c, workerMessage(err))
		return nil, true
	}
	return models, false
}

// usableAPIModels returns the static catalog entries whose provider has a
// stored authentication secret for this tenant. The boolean reports whether
// an error response was already written.
func (m *Module) usableAPIModels(c *gin.Context, modality Modality) ([]Model, bool) {
	usable, err := m.usableProviders(c)
	if err != nil {
		utils.InternalError(c, err.Error())
		return nil, true
	}
	return FilterAPIByCredentials(List(modality), usable), false
}

// usableProviders resolves the tenant credential lister and reports which
// providers can actually run. Unknown tenants fail closed.
func (m *Module) usableProviders(c *gin.Context) (map[string]bool, error) {
	if m.resolve == nil {
		return nil, errors.New("credential resolver not wired")
	}
	lister, ok := m.resolve(c)
	if !ok || lister == nil {
		return nil, errors.New("credential lookup failed for this tenant")
	}
	return lister.UsableProviders()
}

// FilterAPIByCredentials drops API models whose provider has no stored
// authentication secret. Entries without a provider are kept: they declare
// no credential requirement.
func FilterAPIByCredentials(models []Model, usable map[string]bool) []Model {
	out := make([]Model, 0, len(models))
	for _, m := range models {
		if m.Type == TypeAPI && m.CredentialProvider != "" && !usable[string(m.CredentialProvider)] {
			continue
		}
		out = append(out, m)
	}
	return out
}

// ModelOperation is the JSON view of a worker weight operation.
type ModelOperation struct {
	Key             string  `json:"key"`
	State           string  `json:"state"`
	Progress        float32 `json:"progress"`
	BytesDownloaded int64   `json:"bytes_downloaded"`
	TotalBytes      int64   `json:"total_bytes"`
	Error           string  `json:"error,omitempty"`
}

// modelKeyParam validates the :key route parameter shared by weight writes.
func modelKeyParam(c *gin.Context) (string, bool) {
	key := strings.TrimSpace(c.Param("key"))
	if key == "" {
		utils.BadRequest(c, "model key is required")
		return "", false
	}
	return key, true
}

// weightWriter is the narrow gRPC surface the management writes need.
type weightWriter interface {
	DownloadModel(ctx context.Context, addr, key string, cancel bool) (*inferencev1.ModelOperationResponse, error)
	DeleteModel(ctx context.Context, addr, key string) (*inferencev1.ModelOperationResponse, error)
}

// workerClientOnly resolves the wired gRPC client and configured address
// for management writes. Reads degrade gracefully elsewhere, but a write
// with no worker to receive it must fail explicitly.
func workerClientOnly(c *gin.Context) (weightWriter, string, bool) {
	client := workerClientOrNil()
	if client == nil {
		utils.InternalError(c, "inference worker client not wired")
		return nil, "", false
	}
	addr := workerAddr()
	if addr == "" {
		utils.BadRequest(c, "inference worker not configured (BM_WORKER_ADDR)")
		return nil, "", false
	}
	return client, addr, true
}

// workerWriteTarget resolves the model key, wired gRPC client and configured
// address for single-model management writes.
func workerWriteTarget(c *gin.Context) (string, weightWriter, string, bool) {
	key, ok := modelKeyParam(c)
	if !ok {
		return "", nil, "", false
	}
	client, addr, ok := workerClientOnly(c)
	if !ok {
		return "", nil, "", false
	}
	return key, client, addr, true
}

// toModelOperation maps a worker operation response onto the stable HTTP
// shape served by the management routes.
func toModelOperation(resp *inferencev1.ModelOperationResponse) ModelOperation {
	if resp == nil {
		return ModelOperation{}
	}
	return ModelOperation{
		Key:             resp.Key,
		State:           resp.State,
		Progress:        resp.Progress,
		BytesDownloaded: resp.BytesDownloaded,
		TotalBytes:      resp.TotalBytes,
		Error:           resp.Error,
	}
}

// filterByModalityOrAll filters live entries by a validated modality, or
// returns them all when modality is empty.
func filterByModalityOrAll(models []Model, modality string) []Model {
	if modality == "" {
		return models
	}
	out, err := filterByModality(models, modality)
	if err != nil {
		return models // already validated upstream; unreachable
	}
	return out
}

// workerMessage turns a worker failure into a client-facing message.
func workerMessage(err error) string {
	if we := (*WorkerError)(nil); errors.As(err, &we) {
		return "inference worker unavailable: " + we.Err.Error()
	}
	if errors.Is(err, ErrWorkerUnreachable) {
		return "inference worker not configured (BM_WORKER_ADDR)"
	}
	return err.Error()
}

// workerRPCError maps a worker RPC failure to the HTTP status its gRPC code
// implies: unknown keys are 404, conflicting operations are 409, and a dead
// worker is 503 — not a generic 500.
func workerRPCError(c *gin.Context, op string, err error) {
	st, _ := status.FromError(err)
	message := strings.TrimSpace(st.Message())
	if message == "" {
		message = err.Error()
	}
	switch st.Code() {
	case codes.InvalidArgument:
		utils.BadRequest(c, message)
	case codes.NotFound:
		utils.NotFound(c, message)
	case codes.FailedPrecondition:
		utils.Conflict(c, message)
	case codes.Unavailable:
		c.JSON(http.StatusServiceUnavailable,
			gin.H{"success": false, "message": "inference worker unavailable: " + message})
	default:
		utils.InternalError(c, op+" failed: "+message)
	}
}

// logWorkerError marks the response and logs a degraded merged listing.
func logWorkerError(c *gin.Context, err error) {
	c.Header("X-Worker-Status", "unreachable")
	log.Printf("[model] merged listing without live worker: %v", err)
}
