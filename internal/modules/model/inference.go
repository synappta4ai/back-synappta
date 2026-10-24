// Hosted inference providers.
//
// Besides the curated API catalog (catalog.go) and the tenant's own local
// servers (local.go), Synapta can run models on hosted inference clouds the
// tenant pays for itself: RunPod serverless endpoints and the
// OpenAI-compatible clouds (Together, DeepInfra, GMI, Baseten, Vultr,
// DigitalOcean, CoreWeave, OpenRouter).
//
// Their model list cannot live in the static catalog — each tenant has access
// to a different subset — so names are namespaced as
// "<provider>/<model id>" (e.g. "together/black-forest-labs/FLUX.1-schnell")
// and the catalog lists whatever GET {base_url}/models answers, live and
// uncached. RunPod has no model listing (each endpoint is a model), so its
// entry comes from the credential's endpoint id: "runpod/<endpoint-id>".
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// ProviderRunPod is RunPod serverless: one endpoint id per model.
	ProviderRunPod CredentialProvider = "runpod"
	// ProviderTogether is Together AI (OpenAI-compatible + images).
	ProviderTogether CredentialProvider = "together"
	// ProviderDeepInfra is DeepInfra (OpenAI-compatible + images).
	ProviderDeepInfra CredentialProvider = "deepinfra"
	// ProviderGMI is GMI Cloud's OpenAI-compatible serving API.
	ProviderGMI CredentialProvider = "gmi"
	// ProviderBaseten is Baseten Model APIs (OpenAI-compatible).
	ProviderBaseten CredentialProvider = "baseten"
	// ProviderVultr is Vultr Serverless Inference (OpenAI-compatible).
	ProviderVultr CredentialProvider = "vultr"
	// ProviderDigitalOcean is DigitalOcean Gradient AI (OpenAI-compatible).
	ProviderDigitalOcean CredentialProvider = "digitalocean"
	// ProviderCoreWeave is CoreWeave Inference (OpenAI-compatible).
	ProviderCoreWeave CredentialProvider = "coreweave"
	// ProviderOpenRouter is OpenRouter. It is also the agency agent's LLM
	// provider; here it serves the tenant's own text and image models.
	ProviderOpenRouter CredentialProvider = "openrouter"
)

// Generator names declared by discovered inference entries.
const (
	GeneratorOpenAIText  = "openai-text"
	GeneratorOpenAIImage = "openai-image"
	GeneratorRunPod      = "runpod"
	// GeneratorOpenRouterVideo serves OpenRouter video models through the
	// dedicated async API (POST /videos, poll /videos/{id}, download
	// /videos/{id}/content). It is the only hosted video route: no other
	// provider in the registry exposes video generation.
	GeneratorOpenRouterVideo = "openrouter-video"
)

// inferenceProviderSpec describes one hosted provider: where its
// OpenAI-compatible API lives, whether it can list models, and whether it
// exposes the OpenAI Images route.
type inferenceProviderSpec struct {
	Label string
	// BaseURL is the OpenAI-compatible API root (includes /v1 when the
	// provider mounts its routes there).
	BaseURL string
	// ImagePath is the OpenAI-compatible image route, "" when the provider
	// has none (or needs a deployment-specific URL, like Baseten).
	ImagePath string
	// Discoverable reports whether GET {BaseURL}/models lists the models.
	// RunPod cannot: every serverless endpoint is its own model.
	Discoverable bool
	// EndpointScoped reports that the model id is a RunPod endpoint id, so
	// the request goes to /v2/{id}/… instead of an OpenAI route.
	EndpointScoped bool
}

// inferenceProviders is the single source of truth for hosted inference
// providers. Keys are the credential provider names (and the model prefix).
var inferenceProviders = map[CredentialProvider]inferenceProviderSpec{
	ProviderRunPod: {
		Label:          "RunPod",
		BaseURL:        "https://api.runpod.ai",
		EndpointScoped: true,
	},
	ProviderTogether: {
		Label:        "Together AI",
		BaseURL:      "https://api.together.xyz/v1",
		ImagePath:    "/images/generations",
		Discoverable: true,
	},
	ProviderDeepInfra: {
		Label:        "DeepInfra",
		BaseURL:      "https://api.deepinfra.com/v1/openai",
		ImagePath:    "/images/generations",
		Discoverable: true,
	},
	ProviderGMI: {
		Label:        "GMI Cloud",
		BaseURL:      "https://api.gmi-serving.com/v1",
		Discoverable: true,
	},
	ProviderBaseten: {
		Label:        "Baseten",
		BaseURL:      "https://inference.baseten.co/v1",
		Discoverable: true,
	},
	ProviderVultr: {
		Label:        "Vultr",
		BaseURL:      "https://api.vultrinference.com/v1",
		Discoverable: true,
	},
	ProviderDigitalOcean: {
		Label:        "DigitalOcean",
		BaseURL:      "https://inference.do-ai.run/v1",
		Discoverable: true,
	},
	ProviderCoreWeave: {
		Label:        "CoreWeave",
		BaseURL:      "https://api.inference.wandb.ai/v1",
		Discoverable: true,
	},
	ProviderOpenRouter: {
		Label:        "OpenRouter",
		BaseURL:      "https://openrouter.ai/api/v1",
		ImagePath:    "/images",
		Discoverable: true,
	},
}

// inferenceModelsTimeout bounds discovery per provider. Hosted APIs answer
// fast; a provider that hangs must not delay the tenant's whole catalog.
const inferenceModelsTimeout = 5 * time.Second

// InferenceEndpoint is one hosted provider resolved for catalog discovery:
// where its API lives, the tenant's key and, for RunPod, the serverless
// endpoint id saved with the credential.
type InferenceEndpoint struct {
	Provider   string
	BaseURL    string
	APIKey     string
	EndpointID string
}

// InferenceProviderNames returns the supported inference providers, sorted.
func InferenceProviderNames() []string {
	names := make([]string, 0, len(inferenceProviders))
	for p := range inferenceProviders {
		names = append(names, string(p))
	}
	sort.Strings(names)
	return names
}

// IsInferenceProvider reports whether p is a hosted inference provider.
func IsInferenceProvider(p CredentialProvider) bool {
	_, ok := inferenceProviders[p]
	return ok
}

// IsEndpointScopedProvider reports whether the model id is a provider
// endpoint (RunPod) instead of a model name on a shared API.
func IsEndpointScopedProvider(p CredentialProvider) bool {
	return inferenceProviders[p].EndpointScoped
}

// InferenceProviderLabel returns the human name for a provider ("" when
// unknown).
func InferenceProviderLabel(p CredentialProvider) string { return inferenceProviders[p].Label }

// InferenceProviderBaseURL returns the provider's default OpenAI-compatible
// base URL ("" when unknown).
func InferenceProviderBaseURL(p CredentialProvider) string { return inferenceProviders[p].BaseURL }

// InferenceImagePath returns the provider's OpenAI-compatible image route, ""
// when the provider has none.
func InferenceImagePath(p CredentialProvider) string { return inferenceProviders[p].ImagePath }

// InferenceVideoPath returns the provider's video generation route, "" when
// the provider has none. Today only OpenRouter exposes one (POST /videos).
func InferenceVideoPath(p CredentialProvider) string {
	if p == ProviderOpenRouter {
		return "/videos"
	}
	return ""
}

// InferenceModelName builds the catalog name for an inference model:
// "<provider>/<model id>".
func InferenceModelName(p CredentialProvider, id string) string {
	return string(p) + localModelSeparator + strings.TrimSpace(id)
}

// ParseInferenceModel splits "together/black-forest-labs/FLUX.1-schnell" into
// the provider and the provider-side model id (which may contain slashes).
// Local providers are handled by ParseLocalModel; names without a known
// hosted provider prefix report false.
func ParseInferenceModel(name string) (CredentialProvider, string, bool) {
	provider, id, found := strings.Cut(strings.TrimSpace(name), localModelSeparator)
	if !found {
		return "", "", false
	}
	p := CredentialProvider(strings.ToLower(strings.TrimSpace(provider)))
	id = strings.TrimSpace(id)
	if !IsInferenceProvider(p) || IsLocalProvider(p) || id == "" {
		return "", "", false
	}
	return p, id, true
}

// inferenceVideoMarkers are the model-id fragments that identify a
// text-to-video model on providers with a video generation route (today
// only OpenRouter). Checked before the image markers: families like Flux
// ship both image models (flux-3-image) and video ones (flux-video-edit).
// Verified 2026-10 against the live OpenRouter catalogs (61 image + 30
// video models): no text-model id matches these, and every video id matches
// at least one. Hyphenated forms (kling-, wan-) avoid false positives like
// thinkingmachines/inkling.
var inferenceVideoMarkers = []string{
	"veo", "hailuo", "wan-", "seedance", "/kling", "heygen", "runway",
	"aleph", "happyhorse", "grok-imagine-video", "flux-video", "video",
}

// inferenceImageMarkers are the model-id fragments that identify a
// text-to-image model. Providers' /models listings rarely declare a modality,
// so classification is name-based; it must stay deterministic so discovery
// and generation routing agree without persisting anything.
// The generic "image" fragment is data-backed: in the 2026-10 OpenRouter
// catalog every id containing "image" generates images and none is
// text-only. Family fragments cover image models without "image" in the id
// (Google's Nano Banana, Krea, Sourceful Riverflow).
var inferenceImageMarkers = []string{
	"flux", "stable-diffusion", "sdxl", "sd3", "sd-1", "sd-2", "sd-3",
	"seedream", "imagen", "gpt-image", "image", "nano-banana", "krea",
	"riverflow", "dall-e", "kolors", "recraft",
	"ideogram", "cogview", "photon", "qwen-image", "playground-v2", "hidream",
}

// inferenceNoiseMarkers are the entries a generation picker should not offer:
// embeddings, rerankers, guardrails, moderation models and batch-only
// variants. OpenRouter lists ":batch" ids in /models but rejects them on the
// interactive chat route, so offering them only produces 400s.
var inferenceNoiseMarkers = []string{
	"embed", "rerank", "guard", "moderation", "classifier", ":batch",
	"bge-", "bge_", "e5-", "e5_", "gte-", "gte_", "voyage-",
}

// ClassifyInferenceModel maps a provider-side model id to a generation
// modality. Video is checked first so dual families (flux image vs video)
// route correctly.
func ClassifyInferenceModel(id string) Modality {
	lower := strings.ToLower(id)
	for _, marker := range inferenceVideoMarkers {
		if strings.Contains(lower, marker) {
			return ModalityVideo
		}
	}
	for _, marker := range inferenceImageMarkers {
		if strings.Contains(lower, marker) {
			return ModalityImage
		}
	}
	return ModalityText
}

// ResolveInferenceModality is the modality discovery and generation routing
// must agree on. Hosted video generation only exists on OpenRouter; on every
// other provider a video-named id stays text (usually a video-input chat
// model), preserving the historical behavior.
func ResolveInferenceModality(provider CredentialProvider, id string) Modality {
	m := ClassifyInferenceModel(id)
	if m == ModalityVideo && provider != ProviderOpenRouter {
		return ModalityText
	}
	return m
}

// listedModality prefers the provider's declared output modalities over the
// name heuristic. It only promotes an entry to a media modality the provider
// can actually serve; a router that may answer with text+image keeps its text
// classification so it does not show up as an image generator.
func listedModality(provider CredentialProvider, id string, outputs []string) Modality {
	has := func(want string) bool {
		for _, v := range outputs {
			if strings.EqualFold(strings.TrimSpace(v), want) {
				return true
			}
		}
		return false
	}
	if has("video") && InferenceVideoPath(provider) != "" {
		return ModalityVideo
	}
	// Image generators also return text (captions), so text+image is still an
	// image model. "auto" routers are the exception: they forward to any
	// model and are not image generators themselves.
	if has("image") && InferenceImagePath(provider) != "" && !isRoutingModel(id) {
		return ModalityImage
	}
	return ResolveInferenceModality(provider, id)
}

func isRoutingModel(id string) bool {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "auto", "auto-beta", "openrouter/auto", "openrouter/auto-beta":
		return true
	}
	return false
}

// supportsReasoning reads the provider's supported_parameters, which OpenRouter
// publishes as an array of strings and its media catalogs as an object.
func supportsReasoning(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		for _, p := range list {
			if isReasoningParameter(p) {
				return true
			}
		}
		return false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err == nil {
		for key := range object {
			if isReasoningParameter(key) {
				return true
			}
		}
	}
	return false
}

func isReasoningParameter(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "reasoning", "include_reasoning", "reasoning_effort":
		return true
	}
	return false
}

// isInferenceNoise reports whether a listed model is not a generation model.
func isInferenceNoise(id string) bool {
	lower := strings.ToLower(id)
	for _, marker := range inferenceNoiseMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// InferenceModelEndpoint returns the route a discovered inference model should
// be submitted to: the OpenAI chat route or the provider's images/videos route.
func InferenceModelEndpoint(p CredentialProvider, modality Modality) string {
	if modality == ModalityImage {
		if path := InferenceImagePath(p); path != "" {
			return path
		}
	}
	if modality == ModalityVideo {
		if path := InferenceVideoPath(p); path != "" {
			return path
		}
	}
	return "/chat/completions"
}

// DiscoverInferenceModels reads the models a hosted provider serves right now
// and maps them to catalog entries.
//
// RunPod has no model listing: its single entry comes from endpointID (the
// serverless endpoint saved with the credential) and is always an image model,
// the modality RunPod workers are most commonly used for.
//
// For every other provider the read is a live, bounded GET {base}/models;
// failures are returned so the caller can degrade to "no models from this
// provider" instead of inventing entries.
func DiscoverInferenceModels(
	ctx context.Context,
	provider CredentialProvider,
	baseURL, apiKey, endpointID string,
) ([]Model, error) {
	spec, ok := inferenceProviders[provider]
	if !ok {
		return nil, fmt.Errorf("provider %q is not a hosted inference provider", provider)
	}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = spec.BaseURL
	}

	if spec.EndpointScoped {
		id := strings.TrimSpace(endpointID)
		if id == "" {
			return nil, nil
		}
		return []Model{{
			Name:               InferenceModelName(provider, id),
			DisplayName:        spec.Label + " · " + id,
			Modality:           ModalityImage,
			Generator:          GeneratorRunPod,
			CredentialProvider: provider,
			Type:               TypeAPI,
			BaseURL:            base,
			Endpoint:           "/v2/" + id,
		}}, nil
	}

	if !spec.Discoverable {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, inferenceModelsTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", provider, err)
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	}
	client := &http.Client{Timeout: inferenceModelsTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", provider, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: read models: %w", provider, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", provider, resp.Status)
	}

	// Every hosted provider follows the OpenAI list shape:
	// {"data":[{"id":"…","type":"…"}]}. The type field is informational only:
	// classification is name-based so generation routing reaches the same
	// answer without re-reading the listing.
	var payload struct {
		Data []struct {
			ID                  string          `json:"id"`
			Type                string          `json:"type"`
			SupportedParameters json.RawMessage `json:"supported_parameters"`
			Architecture        struct {
				OutputModalities []string `json:"output_modalities"`
			} `json:"architecture"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("%s: parse models: %w", provider, err)
	}

	out := make([]Model, 0, len(payload.Data))
	for _, entry := range payload.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" || isInferenceNoise(id) {
			continue
		}
		modality := listedModality(provider, id, entry.Architecture.OutputModalities)
		if modality == ModalityVideo && InferenceVideoPath(provider) == "" {
			// Listed as a video model by id, but this provider has no
			// video generation route: offering it would fail on submit.
			continue
		}
		endpoint := InferenceModelEndpoint(provider, modality)
		if modality == ModalityImage && spec.ImagePath == "" {
			// Listed as an image model by id, but this provider has no
			// OpenAI-compatible image route: offering it would fail on
			// submit. Text models keep working.
			continue
		}
		out = append(out, Model{
			Name:               InferenceModelName(provider, id),
			DisplayName:        id,
			Modality:           modality,
			Generator:          generatorForProviderModality(provider, modality),
			CredentialProvider: provider,
			Type:               TypeAPI,
			BaseURL:            base,
			Endpoint:           endpoint,
			Reasoning:          modality == ModalityText && supportsReasoning(entry.SupportedParameters),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DisplayName < out[j].DisplayName })

	// OpenRouter also publishes dedicated image/video catalogs with
	// capabilities (supported resolutions, durations, aspect ratios). They
	// are authoritative for modality and enrich the entries; the /models
	// read above still supplies every text model. Dedicated entries go
	// first so they win the dedupe below.
	if provider == ProviderOpenRouter {
		out = append(discoverOpenRouterMediaModels(base, apiKey), out...)
		out = dedupeModels(out)
	}
	return out, nil
}

// openRouterImageTiers are the resolution tiers the OpenRouter Image API
// accepts and our image validation understands (ValidResolutionsImage).
var openRouterImageTiers = map[string]bool{"1K": true, "2K": true, "4K": true}

// openRouterImageRatios is the fallback ratio set for Image API models: the
// endpoint clamps to each model's subset, so a standard set is safe and the
// per-model capabilities endpoint remains the source of truth.
var openRouterImageRatios = []string{"1:1", "16:9", "9:16", "4:3", "3:4"}

// openRouterImageRatioValues keeps the ratios a model advertises, dropping
// "auto" and malformed values. It falls back to the standard set when the
// endpoint publishes no aspect_ratio capability.
func openRouterImageRatioValues(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if v == "" || strings.EqualFold(v, "auto") || !strings.Contains(v, ":") || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return openRouterImageRatios
	}
	return out
}

// openRouterVideoResolutions / Ratios are the values the OpenRouter Video API
// documents. Entries from /videos/models are filtered to these so our
// generator validation and the UI never offer a value the API rejects.
var openRouterVideoResolutions = map[string]bool{
	"480p": true, "720p": true, "768p": true, "1080p": true,
	"1K": true, "2K": true, "4K": true,
}

var openRouterVideoRatios = map[string]bool{
	"16:9": true, "9:16": true, "1:1": true, "4:3": true, "3:4": true,
	"3:2": true, "2:3": true, "21:9": true, "9:21": true,
}

// discoverOpenRouterMediaModels reads OpenRouter's dedicated image and video
// catalogs (GET /images/models, GET /videos/models, no auth required) and
// maps them with their real capabilities. Fresh bounded contexts: the
// caller's listing deadline already covers /models, and these two reads must
// not inherit its leftovers.
func discoverOpenRouterMediaModels(base, apiKey string) []Model {
	out := []Model{}
	out = append(out, discoverOpenRouterImages(base, apiKey)...)
	out = append(out, discoverOpenRouterVideos(base, apiKey)...)
	return out
}

// openRouterGet fetches a JSON document from the OpenRouter API with auth
// when a key is configured (quotas/throughput) and a hard timeout.
func openRouterGet(base, path, apiKey string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(strings.TrimSpace(base), "/")+path, nil)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter %s: %s", path, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// discoverOpenRouterImages maps GET /images/models to image entries.
func discoverOpenRouterImages(base, apiKey string) []Model {
	body, err := openRouterGet(base, "/images/models", apiKey, inferenceModelsTimeout)
	if err != nil {
		log.Printf("[model] openrouter images catalog unavailable: %v", err)
		return nil
	}
	var payload struct {
		Data []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Params struct {
				Resolution struct {
					Values []string `json:"values"`
				} `json:"resolution"`
				AspectRatio struct {
					Values []string `json:"values"`
				} `json:"aspect_ratio"`
				InputReferences struct {
					Max int `json:"max"`
				} `json:"input_references"`
			} `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("[model] openrouter images catalog parse: %v", err)
		return nil
	}
	out := make([]Model, 0, len(payload.Data))
	for _, entry := range payload.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" || isInferenceNoise(id) {
			continue
		}
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			name = id
		}
		resolutions := []string{}
		for _, v := range entry.Params.Resolution.Values {
			if openRouterImageTiers[v] {
				resolutions = append(resolutions, v)
			}
		}
		ratios := openRouterImageRatioValues(entry.Params.AspectRatio.Values)
		out = append(out, Model{
			Name:               InferenceModelName(ProviderOpenRouter, id),
			DisplayName:        name,
			Modality:           ModalityImage,
			Generator:          GeneratorOpenAIImage,
			CredentialProvider: ProviderOpenRouter,
			Type:               TypeAPI,
			BaseURL:            strings.TrimRight(strings.TrimSpace(base), "/"),
			Endpoint:           "/images",
			MaxReferences:      entry.Params.InputReferences.Max,
			Defaults:           Defaults{Ratios: ratios, Resolutions: resolutions},
		})
	}
	return out
}

// discoverOpenRouterVideos maps GET /videos/models to video entries with
// the exact durations, resolutions and aspect ratios each model supports.
func discoverOpenRouterVideos(base, apiKey string) []Model {
	body, err := openRouterGet(base, "/videos/models", apiKey, inferenceModelsTimeout)
	if err != nil {
		log.Printf("[model] openrouter videos catalog unavailable: %v", err)
		return nil
	}
	var payload struct {
		Data []struct {
			ID           string   `json:"id"`
			Name         string   `json:"name"`
			Durations    []int    `json:"supported_durations"`
			Resolutions  []string `json:"supported_resolutions"`
			AspectRatios []string `json:"supported_aspect_ratios"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("[model] openrouter videos catalog parse: %v", err)
		return nil
	}
	out := make([]Model, 0, len(payload.Data))
	for _, entry := range payload.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" || isInferenceNoise(id) {
			continue
		}
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			name = id
		}
		resolutions := []string{}
		for _, v := range entry.Resolutions {
			if openRouterVideoResolutions[strings.TrimSpace(v)] {
				resolutions = append(resolutions, strings.TrimSpace(v))
			}
		}
		ratios := []string{}
		for _, v := range entry.AspectRatios {
			if openRouterVideoRatios[strings.TrimSpace(v)] {
				ratios = append(ratios, strings.TrimSpace(v))
			}
		}
		out = append(out, Model{
			Name:               InferenceModelName(ProviderOpenRouter, id),
			DisplayName:        name,
			Modality:           ModalityVideo,
			Generator:          GeneratorOpenRouterVideo,
			CredentialProvider: ProviderOpenRouter,
			Type:               TypeAPI,
			BaseURL:            strings.TrimRight(strings.TrimSpace(base), "/"),
			Endpoint:           "/videos",
			Defaults: Defaults{
				Ratios:      ratios,
				Resolutions: resolutions,
				Durations:   entry.Durations,
			},
		})
	}
	return out
}

// DiscoverAllInferenceModels queries every configured provider concurrently
// and merges the results, keeping one entry per model name. A provider that
// fails contributes nothing (the failure is logged), so one slow or broken
// cloud never hides the rest of the catalog.
func DiscoverAllInferenceModels(ctx context.Context, endpoints []InferenceEndpoint) []Model {
	buckets := make([][]Model, len(endpoints))
	var wg sync.WaitGroup
	for i, ep := range endpoints {
		wg.Add(1)
		go func(i int, ep InferenceEndpoint) {
			defer wg.Done()
			models, err := DiscoverInferenceModels(
				ctx,
				CredentialProvider(ep.Provider),
				ep.BaseURL,
				ep.APIKey,
				ep.EndpointID,
			)
			if err != nil {
				log.Printf("[model] inference catalog %s unavailable: %v", ep.Provider, err)
				return
			}
			buckets[i] = models
		}(i, ep)
	}
	wg.Wait()

	out := make([]Model, 0)
	seen := map[string]bool{}
	for _, models := range buckets {
		for _, m := range models {
			key := strings.ToLower(m.Name)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CredentialProvider != out[j].CredentialProvider {
			return out[i].CredentialProvider < out[j].CredentialProvider
		}
		return out[i].DisplayName < out[j].DisplayName
	})
	return out
}

// generatorForModality maps a modality to the generic hosted generator.
func generatorForModality(modality Modality) string {
	if modality == ModalityImage {
		return GeneratorOpenAIImage
	}
	return GeneratorOpenAIText
}

// generatorForProviderModality maps a (provider, modality) pair to its
// generator. Video generation only exists on OpenRouter.
func generatorForProviderModality(provider CredentialProvider, modality Modality) string {
	if modality == ModalityVideo && provider == ProviderOpenRouter {
		return GeneratorOpenRouterVideo
	}
	return generatorForModality(modality)
}

// dedupeModels keeps one entry per model name (first occurrence wins) and
// returns the deterministic provider-then-name order.
func dedupeModels(models []Model) []Model {
	seen := map[string]bool{}
	out := make([]Model, 0, len(models))
	for _, m := range models {
		key := strings.ToLower(m.Name)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CredentialProvider != out[j].CredentialProvider {
			return out[i].CredentialProvider < out[j].CredentialProvider
		}
		return out[i].DisplayName < out[j].DisplayName
	})
	return out
}
