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

// inferenceImageMarkers are the model-id fragments that identify a
// text-to-image model. Providers' /models listings rarely declare a modality,
// so classification is name-based; it must stay deterministic so discovery
// and generation routing agree without persisting anything.
var inferenceImageMarkers = []string{
	"flux", "stable-diffusion", "sdxl", "sd3", "sd-1", "sd-2", "sd-3",
	"seedream", "imagen", "gpt-image", "dall-e", "kolors", "recraft",
	"ideogram", "cogview", "photon", "qwen-image", "playground-v2", "hidream",
}

// inferenceNoiseMarkers are the entries a generation picker should not offer:
// embeddings, rerankers, guardrails and moderation models. The list covers the
// common embedding families too (bge, e5, gte, voyage) since their ids often
// do not contain the word "embed".
var inferenceNoiseMarkers = []string{
	"embed", "rerank", "guard", "moderation", "classifier",
	"bge-", "bge_", "e5-", "e5_", "gte-", "gte_", "voyage-",
}

// ClassifyInferenceModel maps a provider-side model id to a generation
// modality. Only text and image are supported by the hosted generators.
func ClassifyInferenceModel(id string) Modality {
	lower := strings.ToLower(id)
	for _, marker := range inferenceImageMarkers {
		if strings.Contains(lower, marker) {
			return ModalityImage
		}
	}
	return ModalityText
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
// be submitted to: the OpenAI chat route or the provider's images route.
func InferenceModelEndpoint(p CredentialProvider, modality Modality) string {
	if modality == ModalityImage {
		if path := InferenceImagePath(p); path != "" {
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
			ID   string `json:"id"`
			Type string `json:"type"`
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
		modality := ClassifyInferenceModel(id)
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
			Generator:          generatorForModality(modality),
			CredentialProvider: provider,
			Type:               TypeAPI,
			BaseURL:            base,
			Endpoint:           endpoint,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DisplayName < out[j].DisplayName })
	return out, nil
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
