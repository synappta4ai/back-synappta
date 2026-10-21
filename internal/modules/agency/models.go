package agency

import (
	"context"
	"log"
	"strings"

	"synapta/internal/modules/model"
	"synapta/internal/worker"
)

// modelEntry is the catalog view the agency core works with.
type modelEntry struct {
	Name               string
	CredentialProvider model.CredentialProvider
	Modality           model.Modality
	Generator          string
	BaseURL            string
	Endpoint           string
	ImageEndpoint      string
	ReferenceEndpoint  string
	GallerySync        bool
	Type               model.ModelType
}

// LookupModel resolves a model from the read-only code catalog, falling back
// to a live lookup on the inference worker for downloaded models. Downloaded
// names are never cached: every unknown name hits the worker once per call.
func LookupModel(name string) *modelEntry {
	if m := model.ByName(name); m != nil {
		return &modelEntry{
			Name:               m.Name,
			CredentialProvider: m.CredentialProvider,
			Modality:           m.Modality,
			Generator:          m.Generator,
			BaseURL:            m.BaseURL,
			Endpoint:           m.Endpoint,
			ImageEndpoint:      m.ImageEndpoint,
			ReferenceEndpoint:  m.ReferenceEndpoint,
			GallerySync:        m.GallerySync,
			Type:               model.TypeAPI,
		}
	}

	// Local OpenAI-compatible providers (Ollama, LM Studio): the name is
	// "<provider>/<model id>", so every model the tenant's server reports
	// resolves without a static catalog entry. The credential's base_url wins
	// in resolveRoute; the default port is the fallback.
	if provider, id, ok := model.ParseLocalModel(name); ok {
		return &modelEntry{
			Name:               model.LocalModelName(provider, id),
			CredentialProvider: provider,
			Modality:           model.ModalityText,
			Generator:          "openai-text",
			BaseURL:            model.DefaultLocalBaseURL(provider),
			Endpoint:           "/chat/completions",
			Type:               model.TypeAPI,
		}
	}

	// Downloaded models: ask the worker live (no cache, no local copy).
	addr := model.WorkerAddr()
	c := model.WorkerClient()
	if addr == "" || c == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), worker.DefaultTimeout)
	defer cancel()
	live, err := model.ListUsable(ctx, addr)
	if err != nil {
		log.Printf("[agency] live model lookup for %q failed: %v", name, err)
		return nil
	}
	for _, lm := range live {
		if strings.EqualFold(lm.Name, name) {
			return &modelEntry{
				Name:     lm.Name,
				Modality: lm.Modality,
				Type:     model.TypeDownloaded,
			}
		}
	}
	return nil
}

// BridgeModelRef describe un modelo que sólo sirve una GPU emparejada del
// tenant (camino bridge: la máquina del usuario, detrás de su NAT). No lleva
// credenciales ni ruta HTTP: el trabajo se encola y lo ejecuta esa GPU.
type BridgeModelRef struct {
	Name     string
	Label    string
	Modality model.Modality
	// WorkerID identifica la GPU que declara el modelo: es a quien se fija el
	// job cuando varias máquinas del tenant están emparejadas.
	WorkerID   string
	WorkerName string
	// Available: la GPU tiene los pesos en disco. Un modelo declarado pero
	// sin pesos se resuelve igual (y Validate explica que hay que bajarlo)
	// en vez de aparecer como "modelo inexistente".
	Available   bool
	Downloading bool
	SizeBytes   int64
}

// BridgeModelResolver busca un modelo en las GPUs emparejadas del tenant.
//
// Un Core existe por tenant, así que el scope ya está fijado en el resolver y
// la firma sólo lleva el nombre. Devuelve (nil, nil) cuando el catálogo de las
// GPUs no declara ese modelo: no es un error, simplemente no está ahí.
type BridgeModelResolver func(ctx context.Context, name string) (*BridgeModelRef, error)

// SetBridgeModels habilita la resolución de modelos servidos por GPUs
// emparejadas. Sin esto (deploy con worker central) el catálogo vivo por gRPC
// sigue siendo la única fuente de modelos "downloaded".
func (s *Core) SetBridgeModels(resolve BridgeModelResolver) { s.bridgeModels = resolve }

// lookupModel resuelve un modelo de generación: primero el catálogo API y el
// worker central (LookupModel) y, si ese no lo conoce, las GPUs emparejadas
// del tenant. Sin este segundo paso un modelo que sólo existe en la GPU del
// usuario moría en "model not found" antes de llegar al generator (y su
// modalidad —image vs video— se perdía).
func (s *Core) lookupModel(name string) *modelRef {
	if m := LookupModel(name); m != nil {
		return m
	}
	if s.bridgeModels == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), worker.DefaultTimeout)
	defer cancel()
	ref, err := s.bridgeModels(ctx, name)
	if err != nil {
		log.Printf("[agency] bridge model lookup for %q failed: %v", name, err)
		return nil
	}
	if ref == nil {
		return nil
	}
	return &modelEntry{
		Name:     ref.Name,
		Modality: ref.Modality,
		Type:     model.TypeDownloaded,
	}
}
