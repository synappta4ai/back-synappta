// Live downloaded-model integration.
//
// The brain-master inference worker (Python, gRPC) is the single source of
// truth for downloaded models. Every read goes live to the worker via
// internal/worker; nothing — catalog, availability, telemetry — is cached,
// persisted or invented here. If the worker is unreachable, downloaded models
// simply do not appear (callers can inspect WorkerError).
package model

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync/atomic"

	"synapta/internal/worker"
	inferencev1 "synapta/internal/worker/proto"
)

// ErrWorkerUnreachable is returned when BM_WORKER_ADDR is unset or the worker
// does not answer within the configured timeout. It is always wrapped with
// the underlying reason.
var ErrWorkerUnreachable = errors.New("inference worker unreachable")

// workerClient is the stateless gRPC client injected by main.
// Wired in SetWorkerClient; reads are nil-safe even if never wired (tests).
var workerClient atomic.Value // *worker.Client

// SetWorkerClient wires the stateless worker client. Called once at startup.
func SetWorkerClient(c *worker.Client) { workerClient.Store(c) }

// WorkerClient returns the wired stateless client (nil if never wired).
// Read access for other packages (agency live model resolution).
func WorkerClient() *worker.Client { return workerClientOrNil() }

// defaultAddr holds the configured BM_WORKER_ADDR (set once at startup).
var defaultAddr atomic.Value // string

// SetWorkerAddr stores the worker address used when handlers don't pass one.
func SetWorkerAddr(addr string) { defaultAddr.Store(addr) }

// workerAddr returns the configured worker address ("" if unset).
func workerAddr() string {
	if v, ok := defaultAddr.Load().(string); ok {
		return v
	}
	return ""
}

// WorkerAddr exposes the configured worker address to other packages
// (agency live model resolution).
func WorkerAddr() string { return workerAddr() }

// workerClientOrNil returns the wired client, or nil.
func workerClientOrNil() *worker.Client {
	if v, ok := workerClient.Load().(*worker.Client); ok {
		return v
	}
	return nil
}

// WorkerError describes a failed live read from the worker.
type WorkerError struct{ Err error }

func (e *WorkerError) Error() string { return "inference worker: " + e.Err.Error() }
func (e *WorkerError) Unwrap() error { return e.Err }

// WorkerStatus is the live snapshot served by GET /api/v1/worker/status.
type WorkerStatus struct {
	Configured    bool   `json:"configured"` // BM_WORKER_ADDR set?
	Connected     bool   `json:"connected"`  // answered a ping?
	DeviceName    string `json:"device_name,omitempty"`
	CudaAvailable bool   `json:"cuda_available"`
	ModelCount    int    `json:"model_count"` // models the worker lists
}

// ListLive returns the worker's full catalog mapped into catalog entries with
// Type=TypeDownloaded, including unavailable entries. It is the management
// view used by /gpu: generation listings must use ListUsable instead. The
// result is built fresh on every call — zero caching.
func ListLive(ctx context.Context, addr string) ([]Model, error) {
	c := workerClientOrNil()
	if c == nil {
		return nil, fmt.Errorf("%w: worker client not wired", ErrWorkerUnreachable)
	}
	resp, err := c.ListModels(ctx, addr)
	if err != nil {
		return nil, &WorkerError{Err: err}
	}
	out := mapLiveModels(resp.Models)

	// Deterministic order for UIs: modality, then family, then name.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Modality != out[j].Modality {
			return out[i].Modality < out[j].Modality
		}
		fi, fj := out[i].Downloaded.Family, out[j].Downloaded.Family
		if fi != fj {
			return fi < fj
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// ListUsable returns only the downloaded models the worker can actually run.
// Unavailable entries (missing credentials, weights not present, failed
// downloads, unmounted volumes) stay visible to /gpu management through
// ListLive, but they must never appear as generation choices.
func ListUsable(ctx context.Context, addr string) ([]Model, error) {
	models, err := ListLive(ctx, addr)
	if err != nil {
		return nil, err
	}
	return FilterAvailable(models), nil
}

// FilterAvailable drops downloaded models the worker reports as unavailable.
// API models are returned unchanged: credential filtering happens at the HTTP
// layer, where the tenant-scoped credential handler is available.
func FilterAvailable(models []Model) []Model {
	out := make([]Model, 0, len(models))
	for _, m := range models {
		if m.Type == TypeDownloaded && (m.Downloaded == nil || !m.Downloaded.Available) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// ListLiveFiltered applies the type/modality filters to the right source:
// "api" → static catalog, "downloaded" → live usable worker models.
func ListLiveFiltered(ctx context.Context, addr, typeFilter, modality string) ([]Model, error) {
	switch ModelType(strings.ToLower(typeFilter)) {
	case TypeAPI:
		return List(Modality(modality)), nil
	case TypeDownloaded:
		models, err := ListUsable(ctx, addr)
		if err != nil {
			return nil, err
		}
		return filterByModality(models, modality)
	default:
		return nil, fmt.Errorf("invalid type: %q", typeFilter)
	}
}

// WorkerStatusNow returns the live worker status for the HTTP handler.
func WorkerStatusNow(ctx context.Context, addr string) (*WorkerStatus, error) {
	c := workerClientOrNil()
	if c == nil {
		return nil, fmt.Errorf("%w: worker client not wired", ErrWorkerUnreachable)
	}
	if addr == "" {
		return &WorkerStatus{Configured: false}, nil
	}
	resp, err := c.GpuTelemetry(ctx, addr)
	if err != nil {
		return &WorkerStatus{Configured: true, Connected: false},
			&WorkerError{Err: err}
	}
	count, cuda := 0, false
	// Catalog is a second live read: telemetry answered ⇒ the worker is up,
	// but the catalog call may still fail (worker restart mid-request, etc.).
	// cuda_available and the model count both come from it.
	if models, merr := c.ListModels(ctx, addr); merr == nil {
		count, cuda = len(FilterAvailable(mapLiveModels(models.Models))), models.CudaAvailable
	} else {
		log.Printf("[model] live model count after telemetry ok: %v", merr)
	}
	return &WorkerStatus{
		Configured:    true,
		Connected:     true,
		DeviceName:    resp.DeviceName,
		CudaAvailable: cuda,
		ModelCount:    count,
	}, nil
}

// mapLiveModels converts worker ModelInfo entries into catalog entries.
// Nil entries are skipped; unavailable entries are preserved here so /gpu can
// manage them. Callers that serve generation choices must filter afterwards.
func mapLiveModels(in []*inferencev1.ModelInfo) []Model {
	out := make([]Model, 0, len(in))
	for _, m := range in {
		if m == nil {
			continue
		}
		out = append(out, Model{
			Name:        m.Key,
			DisplayName: m.Label,
			Modality:    workerModeToModality(m.Mode),
			Type:        TypeDownloaded,
			Downloaded: &DownloadedModel{
				Mode:            m.Mode,
				Engine:          m.Engine,
				Pipeline:        m.Pipeline,
				Repo:            m.Repo,
				Steps:           m.Steps,
				VRAMGb:          m.VramGb,
				Family:          m.Family,
				Available:       m.Available,
				Downloading:     m.Downloading,
				DownloadPercent: m.DownloadPercent,
				SizeBytes:       m.SizeBytes,
				DownloadError:   m.DownloadError,
				Notes:           m.Notes,
			},
		})
	}
	return out
}

// filterByModality returns entries whose Modality matches modality; an empty
// modality returns everything unchanged, an invalid one is an error.
func filterByModality(models []Model, modality string) ([]Model, error) {
	if modality == "" {
		return models, nil
	}
	m := Modality(strings.ToLower(modality))
	if !IsValidModality(string(m)) {
		return nil, fmt.Errorf("invalid modality: %q", modality)
	}
	out := make([]Model, 0, len(models))
	for _, mm := range models {
		if mm.Modality == m {
			out = append(out, mm)
		}
	}
	return out, nil
}

// workerModeToModality maps the worker's mode strings to Synapta modalities.
// video/image/audio map 1:1. tts has no Synapta modality yet: it maps to ""
// so the model is still listed with its raw mode in Downloaded.Mode — never
// mislabeled, never dropped.
func workerModeToModality(mode string) Modality {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "video":
		return ModalityVideo
	case "image":
		return ModalityImage
	case "audio":
		return ModalityAudio
	default:
		return ""
	}
}
