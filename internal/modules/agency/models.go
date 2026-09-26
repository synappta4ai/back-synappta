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
			GallerySync:        m.GallerySync,
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
	live, err := model.ListLive(ctx, addr)
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
