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

	"synapta/internal/modules/workerbridge"
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

// workerArtifactBase holds the configured BM_WORKER_ARTIFACT_BASE (set once at
// startup). Used to proxy artifact list/delete to the worker's artifact server.
var workerArtifactBase atomic.Value // string

// SetWorkerArtifactBase stores the artifact base URL.
func SetWorkerArtifactBase(base string) { workerArtifactBase.Store(base) }

// WorkerArtifactBase returns the configured artifact base ("" if unset).
func WorkerArtifactBase() string {
	if v, ok := workerArtifactBase.Load().(string); ok {
		return v
	}
	return ""
}

// workerClientOrNil returns the wired client, or nil.
func workerClientOrNil() *worker.Client {
	if v, ok := workerClient.Load().(*worker.Client); ok {
		return v
	}
	return nil
}

// BridgeSnapshots lists online bridge workers' model snapshots for a tenant.
// *workerbridge.Store implements it; tests use a fake. Snapshots carry the
// catalog (key/label/mode/engine/…), the weight state the agent read from its
// local gRPC, and the identity of the GPU that reported it: the panel needs
// both to show honest availability and to route a download to that machine.
type BridgeSnapshots interface {
	SnapshotsForTenant(ctx context.Context, tenantID int64) ([]workerbridge.WorkerModel, error)
}

// bridgeSource is the snapshot source injected by main. Reads are nil-safe
// even if never wired (central-only mode, tests).
var bridgeSource atomic.Value // BridgeSnapshots

// SetBridgeSource wires the bridge snapshot fallback. Called once at startup.
func SetBridgeSource(s BridgeSnapshots) { bridgeSource.Store(s) }

func bridgeSourceOrNil() BridgeSnapshots {
	if v, ok := bridgeSource.Load().(BridgeSnapshots); ok {
		return v
	}
	return nil
}

// BridgeModelsForTenant maps online bridge snapshots into management entries
// (same shape as ListLive) plus the identity of the GPU that reported each
// one. Availability and download telemetry come from the snapshot as-is: the
// agent reads them from its local gRPC, and guessing them here (the old
// "available ⇒ 100%" shortcut) painted weights that were not on disk as
// ready. It returns nil without an error when there is simply nothing to
// show: no source wired, no online workers, or an empty merged catalog.
func BridgeModelsForTenant(ctx context.Context, tenantID int64) ([]Model, error) {
	src := bridgeSourceOrNil()
	if src == nil {
		return nil, nil
	}
	snaps, err := src.SnapshotsForTenant(ctx, tenantID)
	if err != nil || len(snaps) == 0 {
		return nil, err
	}
	out := make([]Model, 0, len(snaps))
	for _, m := range snaps {
		out = append(out, Model{
			Name:          m.Key,
			DisplayName:   m.Label,
			Modality:      workerModeToModality(m.Mode),
			Type:          TypeDownloaded,
			WorkerID:      m.WorkerID,
			WorkerName:    m.WorkerName,
			WorkerVersion: m.WorkerVersion,
			Downloaded: &DownloadedModel{
				Mode:            m.Mode,
				Engine:          m.Engine,
				Pipeline:        m.Pipeline,
				Repo:            m.Repo,
				Steps:           int32(m.Steps),
				VRAMGb:          int32(m.VRAMGb),
				Family:          m.Family,
				Available:       m.Available,
				Downloaded:      m.Downloaded,
				Downloading:     m.Downloading,
				DownloadPercent: m.DownloadPercent,
				SizeBytes:       m.SizeBytes,
				DownloadError:   m.DownloadError,
				DownloadPath:    m.DownloadPath,
				Notes:           m.Notes,
			},
		})
	}
	// Same deterministic order as ListLive: modality, family, name.
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

// UsableBridgeModelsForTenant is the generation-side view of the bridge
// catalog: only the entries a paired GPU can run right now (weights on disk).
// The management view (/worker/models) must keep showing the entries without
// weights so an admin can download them, but a generation choice that cannot
// run would only fail after the user picked it.
func UsableBridgeModelsForTenant(ctx context.Context, tenantID int64) ([]Model, error) {
	models, err := BridgeModelsForTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return FilterAvailable(models), nil
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
				Downloaded:      m.Downloaded,
				Downloading:     m.Downloading,
				DownloadPercent: m.DownloadPercent,
				SizeBytes:       m.SizeBytes,
				DownloadError:   m.DownloadError,
				DownloadPath:    m.DownloadPath,
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

// ModalityForWorkerMode es el mapeo público de workerModeToModality: lo usan
// los consumidores de los snapshots del bridge (el resolver del runtime), que
// mapean a modalidades igual que el catálogo vivo del worker central.
func ModalityForWorkerMode(mode string) Modality { return workerModeToModality(mode) }

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
