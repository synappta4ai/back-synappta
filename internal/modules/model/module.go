package model

import (
	"errors"
	"log"

	"github.com/gin-gonic/gin"

	"synapta/internal/utils"
)

// Module exposes the model catalog.
//
// The catalog has two sources:
//   - "api" models: static entries defined in catalog.go (external provider
//     HTTP APIs, per-tenant credentials).
//   - "downloaded" models: served live by the brain-master inference worker
//     (gRPC). Nothing is cached: every request re-reads the worker.
type Module struct{}

func NewModule() *Module { return &Module{} }

func (m *Module) Name() string { return "models" }

func (m *Module) Register(rg *gin.RouterGroup, authMw, _, _ gin.HandlerFunc) {
	g := rg.Group("/models")
	g.Use(authMw)
	{
		g.GET("", m.List)
		g.GET("/:modality", m.ListByModality)
	}
	w := rg.Group("/worker")
	w.Use(authMw)
	{
		w.GET("/status", m.WorkerStatus)
	}
}

// List handles GET /api/v1/models?modality=video&type=downloaded
//
// type is optional: "api" | "downloaded". Without it, both sources are
// merged — API models always appear; downloaded ones only while the worker
// answers (never cached, never invented offline).
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
		models, err := ListLiveFiltered(c.Request.Context(), workerAddr(), typeFilter, modality)
		if err != nil {
			utils.InternalError(c, workerMessage(err))
			return
		}
		utils.Success(c, models)
		return
	}

	// Merged view: static API models + whatever the worker reports now.
	models := List(Modality(modality))
	live, err := ListLive(c.Request.Context(), workerAddr())
	if err != nil {
		logWorkerError(c, err) // API models are still served
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
		models, err := ListLiveFiltered(c.Request.Context(), workerAddr(), typeFilter, modality)
		if err != nil {
			utils.InternalError(c, workerMessage(err))
			return
		}
		utils.Success(c, models)
		return
	}

	models := List(Modality(modality))
	live, err := ListLive(c.Request.Context(), workerAddr())
	if err != nil {
		logWorkerError(c, err)
	} else {
		models = append(models, filterByModalityOrAll(live, modality)...)
	}
	utils.Success(c, models)
}

// WorkerStatus handles GET /api/v1/worker/status — live reachability and GPU
// snapshot of the inference worker. Never cached.
func (m *Module) WorkerStatus(c *gin.Context) {
	status, err := WorkerStatusNow(c.Request.Context(), workerAddr())
	if err != nil {
		// Report the degraded status with 200: the endpoint answered
		// truthfully — the worker is just not reachable right now.
		if status == nil {
			status = &WorkerStatus{Configured: workerAddr() != ""}
		}
		utils.Success(c, status)
		return
	}
	utils.Success(c, status)
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

// logWorkerError marks the response and logs a degraded merged listing.
func logWorkerError(c *gin.Context, err error) {
	c.Header("X-Worker-Status", "unreachable")
	log.Printf("[model] merged listing without live worker: %v", err)
}
