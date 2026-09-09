package agency

import "synapta/internal/modules/model"

// modelEntry is the catalog view the agency core works with.
type modelEntry struct {
	Name               string
	CredentialProvider model.CredentialProvider
	Modality           model.Modality
	Generator          string
	BaseURL            string
	Endpoint           string
	GallerySync        bool
}

// LookupModel resolves a model from the read-only code catalog.
func LookupModel(name string) *modelEntry {
	m := model.ByName(name)
	if m == nil {
		return nil
	}
	return &modelEntry{
		Name:               m.Name,
		CredentialProvider: m.CredentialProvider,
		Modality:           m.Modality,
		Generator:          m.Generator,
		BaseURL:            m.BaseURL,
		Endpoint:           m.Endpoint,
		GallerySync:        m.GallerySync,
	}
}
