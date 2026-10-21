// Local OpenAI-compatible providers (Ollama, LM Studio).
//
// These servers run on the tenant's own machine, need no API key and expose
// whatever weights the user pulled: their model list cannot live in the static
// catalog. Names are namespaced as "<provider>/<model id>" (e.g.
// "ollama/qwen2.5:7b") so the pipeline can route a request back to the right
// server, and the catalog lists them from a live GET {base_url}/models read —
// never cached, never invented. An unreachable server contributes nothing.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	// ProviderOllama is the Ollama server (default port 11434).
	ProviderOllama CredentialProvider = "ollama"
	// ProviderLMStudio is LM Studio's local server (default port 1234).
	ProviderLMStudio CredentialProvider = "lmstudio"
)

// localModelSeparator joins the provider and the server-side model id.
const localModelSeparator = "/"

// localProviderDefaults maps each local provider to the base URL of its
// OpenAI-compatible API.
var localProviderDefaults = map[CredentialProvider]string{
	ProviderOllama:   "http://localhost:11434/v1",
	ProviderLMStudio: "http://localhost:1234/v1",
}

// localModelsTimeout bounds discovery: a stopped server must not delay the
// tenant's model listing.
const localModelsTimeout = 2 * time.Second

// IsLocalProvider reports whether the provider is a tenant-run OpenAI-
// compatible server.
func IsLocalProvider(p CredentialProvider) bool {
	_, ok := localProviderDefaults[p]
	return ok
}

// DefaultLocalBaseURL returns the provider's default OpenAI base URL ("" for
// providers that are not local).
func DefaultLocalBaseURL(p CredentialProvider) string { return localProviderDefaults[p] }

// LocalModelName builds the catalog name for a model served by a local
// provider: "<provider>/<model id>".
func LocalModelName(p CredentialProvider, id string) string {
	return string(p) + localModelSeparator + strings.TrimSpace(id)
}

// ParseLocalModel splits "ollama/qwen2.5:7b" into provider and model id. It
// reports false for names that do not carry a known local provider prefix, so
// callers can treat those through their normal lookup path.
func ParseLocalModel(name string) (CredentialProvider, string, bool) {
	provider, id, found := strings.Cut(strings.TrimSpace(name), localModelSeparator)
	if !found {
		return "", "", false
	}
	p := CredentialProvider(strings.ToLower(strings.TrimSpace(provider)))
	id = strings.TrimSpace(id)
	if !IsLocalProvider(p) || id == "" {
		return "", "", false
	}
	return p, id, true
}

// NormalizeLocalBaseURL completes a bare host with /v1: Ollama and LM Studio
// expose the OpenAI routes under /v1, and users usually type just the host
// (http://localhost:11434). A URL that already carries a path is left as is,
// so gateways and custom prefixes keep working.
func NormalizeLocalBaseURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return ""
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return base
	}
	if parsed.Path == "" {
		return base + "/v1"
	}
	return base
}

// DiscoverLocalModels reads the models a local server serves right now from
// GET {base_url}/models and maps them to catalog entries. baseURL empty falls
// back to the provider's default port.
//
// The read is live and bounded by localModelsTimeout; any failure (server
// stopped, non-2xx, unreadable body) is returned so the caller can degrade to
// "no local models" instead of inventing entries.
func DiscoverLocalModels(ctx context.Context, provider CredentialProvider, baseURL string) ([]Model, error) {
	if !IsLocalProvider(provider) {
		return nil, fmt.Errorf("provider %q is not a local model server", provider)
	}
	base := NormalizeLocalBaseURL(baseURL)
	if base == "" {
		base = DefaultLocalBaseURL(provider)
	}

	ctx, cancel := context.WithTimeout(ctx, localModelsTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", provider, err)
	}
	client := &http.Client{Timeout: localModelsTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", provider, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: read models: %w", provider, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", provider, resp.Status)
	}

	// Both Ollama and LM Studio answer the OpenAI list shape: {"data":[{"id":…}]}.
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("%s: parse models: %w", provider, err)
	}

	out := make([]Model, 0, len(payload.Data))
	for _, entry := range payload.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		out = append(out, Model{
			Name:               LocalModelName(provider, id),
			DisplayName:        id,
			Modality:           ModalityText,
			Generator:          "openai-text",
			CredentialProvider: provider,
			Type:               TypeAPI,
			BaseURL:            base,
			Endpoint:           "/chat/completions",
			// No ratios/resolutions/durations: text has none.
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DisplayName < out[j].DisplayName })
	return out, nil
}
