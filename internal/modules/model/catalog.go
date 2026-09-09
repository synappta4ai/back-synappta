// Package model defines the READ-ONLY AI model catalog.
//
// Models are part of the implementation — they are NOT created by users and
// NOT stored in the database. Each model belongs to exactly one modality
// (video, audio, image, text) and declares which credential provider it needs.
// Per-tenant credentials live in the `credential` module.
package model

import "strings"

// Modality of a model.
type Modality string

const (
	ModalityVideo Modality = "video"
	ModalityAudio Modality = "audio"
	ModalityImage Modality = "image"
	ModalityText  Modality = "text"
)

// CredentialProvider identifies whose credentials a model requires.
type CredentialProvider string

const (
	ProviderBytePlus  CredentialProvider = "byteplus"
	ProviderGemini    CredentialProvider = "gemini"
	ProviderAnthropic CredentialProvider = "anthropic"
)

// Model is a single catalog entry (defined in code, immutable at runtime).
type Model struct {
	// Name is the stable identifier clients send in generation requests.
	Name string `json:"name"`
	// Modality: video | audio | image | text.
	Modality Modality `json:"modality"`
	// Generator name registered in the agency pipeline that serves this model.
	Generator string `json:"generator"`
	// CredentialProvider whose per-tenant credential is required.
	CredentialProvider CredentialProvider `json:"credential_provider"`
	// BaseURL is the provider API base origin (scheme://host[/path]).
	BaseURL string `json:"base_url"`
	// Endpoint is the provider API route appended to BaseURL.
	Endpoint string `json:"endpoint"`
	// GallerySync: uploads reference files to the BytePlus asset library
	// before generation and references them as asset://<id>.
	GallerySync bool `json:"gallery_sync"`
	// DisplayName for UIs.
	DisplayName string `json:"display_name"`
	// Defaults the UI can offer (ratios, resolutions, durations).
	Defaults Defaults `json:"defaults"`
}

// Defaults enumerates the options a modality supports.
type Defaults struct {
	Ratios      []string `json:"ratios,omitempty"`
	Resolutions []string `json:"resolutions,omitempty"`
	Durations   []int    `json:"durations,omitempty"`
}

// catalog is the single source of truth for every model Synapta can run.
var catalog = []Model{
	// ─── Video ────────────────────────────────────────────────
	{
		Name: "dreamina-seedance-2-0-260128", Modality: ModalityVideo,
		Generator: "seedance", CredentialProvider: ProviderBytePlus,
		BaseURL:     "https://ark.ap-southeast.bytepluses.com/api/v3",
		Endpoint:    "/contents/generations/tasks",
		DisplayName: "Seedance 2.0",
		Defaults: Defaults{
			Ratios:      []string{"16:9", "9:16", "1:1", "4:3", "3:4", "21:9"},
			Resolutions: []string{"480p", "720p", "1080p"},
			Durations:   []int{5, 10},
		},
	},
	{
		Name: "dreamina-seedance-2-0-gallery", Modality: ModalityVideo,
		Generator: "seedance-gallery", CredentialProvider: ProviderBytePlus,
		BaseURL:     "https://ark.ap-southeast.bytepluses.com/api/v3",
		Endpoint:    "/contents/generations/tasks",
		GallerySync: true, DisplayName: "Seedance 2.0 Gallery",
		Defaults: Defaults{
			Ratios:      []string{"16:9", "9:16", "1:1", "4:3", "3:4", "21:9"},
			Resolutions: []string{"480p", "720p", "1080p"},
			Durations:   []int{5, 10},
		},
	},
	{
		Name: "dreamina-seedance-2-5", Modality: ModalityVideo,
		Generator: "seedance25", CredentialProvider: ProviderBytePlus,
		BaseURL:     "https://ark.ap-southeast.bytepluses.com/api/v3",
		Endpoint:    "/contents/generations/tasks",
		DisplayName: "Seedance 2.5",
		Defaults: Defaults{
			Ratios:      []string{"adaptive", "16:9", "9:16", "1:1", "4:3", "3:4", "21:9"},
			Resolutions: []string{"480p", "720p", "1080p"},
			Durations:   []int{5, 10},
		},
	},

	// ─── Image ────────────────────────────────────────────────
	{
		Name: "dreamina-seedream-4-pro-251224", Modality: ModalityImage,
		Generator: "seedream", CredentialProvider: ProviderBytePlus,
		BaseURL:     "https://ark.ap-southeast.bytepluses.com/api/v3",
		Endpoint:    "/contents/generations/tasks",
		DisplayName: "Seedream 4 Pro",
		Defaults: Defaults{
			Ratios:      []string{"1:1", "16:9", "9:16", "4:3", "3:4"},
			Resolutions: []string{"720p", "1080p", "2K"},
		},
	},
	{
		Name: "gemini-nano-banana", Modality: ModalityImage,
		Generator: "gemini-nano", CredentialProvider: ProviderGemini,
		BaseURL:     "https://generativelanguage.googleapis.com",
		Endpoint:    "/v1beta/models/gemini-nano-banana:generateContent",
		DisplayName: "Gemini Nano Banana",
		Defaults: Defaults{
			Ratios:      []string{"1:1", "2:3", "3:2", "3:4", "4:3", "4:5", "5:4"},
			Resolutions: []string{"1K", "2K", "4K"},
		},
	},
	{
		Name: "gemini-3-pro-image-preview", Modality: ModalityImage,
		Generator: "gemini-nano-pro", CredentialProvider: ProviderGemini,
		BaseURL:     "https://generativelanguage.googleapis.com",
		Endpoint:    "/v1beta/models/gemini-3-pro-image-preview:generateContent",
		DisplayName: "Gemini 3 Pro Image",
		Defaults: Defaults{
			Ratios:      []string{"1:1", "2:3", "3:2", "3:4", "4:3", "4:5", "5:4"},
			Resolutions: []string{"1K", "2K", "4K"},
		},
	},
	{
		Name: "gemini-3.1-flash-image-preview", Modality: ModalityImage,
		Generator: "gemini-nano", CredentialProvider: ProviderGemini,
		BaseURL:     "https://generativelanguage.googleapis.com",
		Endpoint:    "/v1beta/models/gemini-3.1-flash-image-preview:generateContent",
		DisplayName: "Gemini 3.1 Flash Image",
		Defaults: Defaults{
			Ratios:      []string{"1:1", "2:3", "3:2", "3:4", "4:3", "4:5", "5:4"},
			Resolutions: []string{"1K", "2K", "4K"},
		},
	},

	// ─── Text ─────────────────────────────────────────────────
	{
		Name: "claude-text", Modality: ModalityText,
		Generator: "claude-text", CredentialProvider: ProviderAnthropic,
		BaseURL:     "https://api.anthropic.com",
		Endpoint:    "/v1/messages",
		DisplayName: "Claude Text",
	},
}

// List returns every model in the catalog, optionally filtered by modality.
// An empty modality returns all models.
func List(modality Modality) []Model {
	out := make([]Model, 0, len(catalog))
	for _, m := range catalog {
		if modality == "" || m.Modality == modality {
			out = append(out, m)
		}
	}
	return out
}

// ByName resolves a model by its exact name (case-insensitive), or nil.
func ByName(name string) *Model {
	lower := strings.ToLower(strings.TrimSpace(name))
	for i := range catalog {
		if strings.ToLower(catalog[i].Name) == lower {
			return &catalog[i]
		}
	}
	return nil
}

// ByGenerator returns every model served by a given generator name.
func ByGenerator(generator string) []Model {
	var out []Model
	for _, m := range catalog {
		if m.Generator == generator {
			out = append(out, m)
		}
	}
	return out
}

// IsGalleryModel reports whether the model syncs assets to BytePlus gallery.
func IsGalleryModel(modelName string) bool {
	m := ByName(modelName)
	return m != nil && m.GallerySync
}

// IsValidModality reports whether the modality string is one of the four.
func IsValidModality(s string) bool {
	switch Modality(strings.ToLower(s)) {
	case ModalityVideo, ModalityAudio, ModalityImage, ModalityText:
		return true
	}
	return false
}
