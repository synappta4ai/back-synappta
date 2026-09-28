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
	ProviderBytePlus   CredentialProvider = "byteplus"
	ProviderGemini     CredentialProvider = "gemini"
	ProviderAnthropic  CredentialProvider = "anthropic"
	ProviderHiggsfield CredentialProvider = "higgsfield"
)

// ModelType distinguishes how a model is served:
//   - "api": external provider HTTP API (per-tenant credentials).
//   - "downloaded": weights executed by the brain-master inference worker
//     (gRPC). Listed live from the worker; never cached by Synapta.
type ModelType string

const (
	TypeAPI        ModelType = "api"
	TypeDownloaded ModelType = "downloaded"
)

// IsValidType reports whether the type filter string is one of the two.
func IsValidType(s string) bool {
	switch ModelType(strings.ToLower(s)) {
	case TypeAPI, TypeDownloaded:
		return true
	}
	return false
}

// Model is a single catalog entry (defined in code, immutable at runtime).
type Model struct {
	// Name is the stable identifier clients send in generation requests.
	Name string `json:"name"`
	// Modality: video | audio | image | text.
	Modality Modality `json:"modality"`
	// Generator name registered in the agency pipeline that serves this model.
	Generator string `json:"generator"`
	// CredentialProvider whose per-tenant credential is required.
	// Empty for downloaded models (they run on our own worker).
	CredentialProvider CredentialProvider `json:"credential_provider,omitempty"`
	// Type: "api" | "downloaded". Derived from Downloaded at init.
	Type ModelType `json:"type"`
	// Downloaded carries inference-worker details; nil for API models.
	Downloaded *DownloadedModel `json:"downloaded,omitempty"`
	// BaseURL is the provider API base origin (scheme://host[/path]).
	BaseURL string `json:"base_url,omitempty"`
	// Endpoint is the provider API route appended to BaseURL.
	Endpoint string `json:"endpoint,omitempty"`
	// GallerySync: uploads reference files to the BytePlus asset library
	// before generation and references them as asset://<id>.
	GallerySync bool `json:"gallery_sync"`
	// DisplayName for UIs.
	DisplayName string `json:"display_name"`
	// Defaults the UI can offer (ratios, resolutions, durations).
	Defaults Defaults `json:"defaults"`
}

// DownloadedModel describes a model whose weights run on the brain-master
// inference worker. Every field is reported live by the worker's ListModels
// RPC; Synapta never persists or caches it.
type DownloadedModel struct {
	// Mode as the worker reports it: image | video | tts | audio.
	Mode string `json:"mode"`
	// Engine: diffusers | v1_bridge | mock.
	Engine string `json:"engine"`
	// Pipeline family inside the worker's engine factory.
	Pipeline string `json:"pipeline"`
	// Repo is the Hugging Face repository holding the weights.
	Repo string `json:"repo,omitempty"`
	// Steps the worker runs by default for this model.
	Steps int32 `json:"steps"`
	// VRAMGb is the worker's declared VRAM requirement.
	VRAMGb int32 `json:"vram_gb"`
	// Family groups related models (sd, sdxl, wan, ...).
	Family string `json:"family,omitempty"`
	// Available: false when the worker lists the model but cannot run it
	// (e.g. V1 bridge volume not mounted).
	Available bool `json:"available"`
	// Notes from the worker catalog.
	Notes string `json:"notes,omitempty"`
}

// Defaults enumerates the options a modality supports.
type Defaults struct {
	Ratios      []string `json:"ratios,omitempty"`
	Resolutions []string `json:"resolutions,omitempty"`
	Durations   []int    `json:"durations,omitempty"`
}

// catalog is the single source of truth for every API model Synapta can run.
// Downloaded models are NOT listed here: they arrive live from the worker
// (see live.go). init() labels every static entry as TypeAPI.
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
		Name: "dreamina-seedance-2-5-260628", Modality: ModalityVideo,
		Generator: "seedance25", CredentialProvider: ProviderBytePlus,
		BaseURL:     "https://ark.ap-southeast.bytepluses.com/api/v3",
		Endpoint:    "/contents/generations/tasks",
		DisplayName: "Seedance 2.5",
		Defaults: Defaults{
			Ratios:      []string{"adaptive", "16:9", "9:16", "1:1", "4:3", "3:4", "21:9"},
			Resolutions: []string{"480p", "720p", "1080p"},
			Durations:   []int{5, 30},
		},
	},

	// ─── Image ────────────────────────────────────────────────
	{
		Name: "dola-seedream-5-0-pro-260628", Modality: ModalityImage,
		Generator: "seedream", CredentialProvider: ProviderBytePlus,
		BaseURL:     "https://ark.ap-southeast.bytepluses.com/api/v3",
		Endpoint:    "/images/generations",
		DisplayName: "Dola Seedream 5.0 Pro",
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

	// ─── Video (Higgsfield) ──────────────────────────────────
	{
		Name: "higgsfield-kling-3-0-turbo-t2v", Modality: ModalityVideo,
		Generator: "higgsfield", CredentialProvider: ProviderHiggsfield,
		BaseURL:     "https://api.higgsfield.ai",
		Endpoint:    "/kling-video/v3.0-turbo/text-to-video",
		DisplayName: "Kling 3.0 Turbo",
		Defaults: Defaults{
			Ratios:      []string{"16:9", "9:16", "1:1"},
			Resolutions: []string{"720p", "1080p"},
			Durations:   []int{3, 5, 10, 15},
		},
	},
	{
		Name: "higgsfield-wan-2-6-reference", Modality: ModalityVideo,
		Generator: "higgsfield", CredentialProvider: ProviderHiggsfield,
		BaseURL:     "https://api.higgsfield.ai",
		Endpoint:    "/wan/v2.6/reference-to-video",
		DisplayName: "Wan 2.6 Reference",
		Defaults: Defaults{
			Ratios:      []string{"16:9", "9:16", "1:1", "4:3", "3:4"},
			Resolutions: []string{"720p", "1080p"},
			Durations:   []int{5, 10},
		},
	},
	// Curated additions — Endpoint IDs verified against docs.higgsfield.ai.
	{
		Name: "higgsfield-seedance-2-5-t2v", Modality: ModalityVideo,
		Generator: "higgsfield", CredentialProvider: ProviderHiggsfield,
		BaseURL:     "https://api.higgsfield.ai",
		Endpoint:    "/bytedance/seedance-2.5/text-to-video",
		DisplayName: "Seedance 2.5",
		Defaults: Defaults{
			Ratios:      []string{"16:9", "4:3", "1:1", "3:4", "9:16", "21:9"},
			Resolutions: []string{"480p", "720p"},
			Durations:   []int{4, 5, 8, 10, 15, 20, 25, 30},
		},
	},
	{
		Name: "higgsfield-seedance-2-5-i2v", Modality: ModalityVideo,
		Generator: "higgsfield", CredentialProvider: ProviderHiggsfield,
		BaseURL:     "https://api.higgsfield.ai",
		Endpoint:    "/bytedance/seedance-2.5/image-to-video",
		DisplayName: "Seedance 2.5 (Imagen a Video)",
		Defaults: Defaults{
			Ratios:      nil, // framing follows the input image
			Resolutions: []string{"480p", "720p"},
			Durations:   []int{4, 5, 8, 10, 15, 20, 25, 30},
		},
	},
	{
		Name: "higgsfield-minimax-h3-t2v", Modality: ModalityVideo,
		Generator: "higgsfield", CredentialProvider: ProviderHiggsfield,
		BaseURL:     "https://api.higgsfield.ai",
		Endpoint:    "/minimax/h3/text-to-video",
		DisplayName: "MiniMax H3 (2K)",
		Defaults: Defaults{
			Ratios:      []string{"16:9", "4:3", "1:1", "3:4", "9:16", "21:9"},
			Resolutions: []string{"2K"},
			Durations:   []int{5, 8, 10, 12, 15},
		},
	},
	{
		Name: "higgsfield-ltx-2-5-pro-t2v", Modality: ModalityVideo,
		Generator: "higgsfield", CredentialProvider: ProviderHiggsfield,
		BaseURL:     "https://api.higgsfield.ai",
		Endpoint:    "/lightricks/ltx-2.5/text-to-video/pro",
		DisplayName: "LTX 2.5 Pro",
		Defaults: Defaults{
			Ratios:      []string{"16:9", "9:16"},
			Resolutions: []string{"720p", "1080p"},
			Durations:   []int{6, 8, 10},
		},
	},
	{
		Name: "higgsfield-happyhorse-1-1-t2v", Modality: ModalityVideo,
		Generator: "higgsfield", CredentialProvider: ProviderHiggsfield,
		BaseURL:     "https://api.higgsfield.ai",
		Endpoint:    "/alibaba/happy-horse/v1.1/text-to-video",
		DisplayName: "HappyHorse 1.1",
		Defaults: Defaults{
			Ratios:      []string{"16:9", "9:16", "1:1", "4:3", "3:4"},
			Resolutions: []string{"720p", "1080p"},
			Durations:   []int{3, 5, 8, 10, 15},
		},
	},

	// ─── Image (Higgsfield) ──────────────────────────────
	// Curated additions — Endpoint IDs verified against docs.higgsfield.ai.
	{
		Name: "higgsfield-soul-2-standard", Modality: ModalityImage,
		Generator: "higgsfield", CredentialProvider: ProviderHiggsfield,
		BaseURL:     "https://api.higgsfield.ai",
		Endpoint:    "/higgsfield-ai/soul/v2/standard",
		DisplayName: "Soul 2",
		Defaults: Defaults{
			Ratios:      []string{"1:1", "4:3", "3:4", "3:2", "2:3", "5:4", "4:5", "16:9", "9:16", "21:9"},
			Resolutions: []string{"2K", "4K"},
		},
	},
	{
		Name: "higgsfield-ideogram-4-0", Modality: ModalityImage,
		Generator: "higgsfield", CredentialProvider: ProviderHiggsfield,
		BaseURL:     "https://api.higgsfield.ai",
		Endpoint:    "/ideogram/v4.0",
		DisplayName: "Ideogram 4.0",
		Defaults: Defaults{
			Ratios:      []string{"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "5:4", "4:5"},
			Resolutions: nil, // no resolution tier: rendering_speed-driven
		},
	},
	{
		Name: "higgsfield-recraft-4-1", Modality: ModalityImage,
		Generator: "higgsfield", CredentialProvider: ProviderHiggsfield,
		BaseURL:     "https://api.higgsfield.ai",
		Endpoint:    "/recraft/v4.1/text-to-image",
		DisplayName: "Recraft 4.1",
		Defaults: Defaults{
			Ratios:      []string{"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "5:4", "4:5"},
			Resolutions: []string{"1K"},
		},
	},
}

// init labels the static catalog: every entry without worker details is an
// API model. Downloaded entries are merged at request time in live.go.
func init() {
	for i := range catalog {
		catalog[i].Type = TypeAPI
	}
}

// List returns every static API model, optionally filtered by modality.
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

// ByName resolves an API model by its exact name (case-insensitive), or nil.
// Downloaded models are not resolvable here: they live on the worker only.
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
