package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseInferenceModel(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantProvider CredentialProvider
		wantID       string
		wantOK       bool
	}{
		{
			name:         "provider and namespaced model id",
			input:        "together/black-forest-labs/FLUX.1-schnell",
			wantProvider: ProviderTogether,
			wantID:       "black-forest-labs/FLUX.1-schnell",
			wantOK:       true,
		},
		{name: "runpod endpoint", input: "runpod/abc12def", wantProvider: ProviderRunPod, wantID: "abc12def", wantOK: true},
		{name: "case-insensitive prefix", input: "DeepInfra/meta-llama/Llama-3.3-70B", wantProvider: ProviderDeepInfra, wantID: "meta-llama/Llama-3.3-70B", wantOK: true},
		{name: "local providers are not inference", input: "ollama/qwen2.5:7b"},
		{name: "unknown provider", input: "acme/flux"},
		{name: "no prefix", input: "flux.1-schnell"},
		{name: "empty model id", input: "together/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider, id, ok := ParseInferenceModel(tc.input)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if provider != tc.wantProvider || id != tc.wantID {
				t.Fatalf("got (%q, %q), want (%q, %q)", provider, id, tc.wantProvider, tc.wantID)
			}
		})
	}
}

func TestClassifyInferenceModel(t *testing.T) {
	images := []string{
		"black-forest-labs/FLUX.1-schnell",
		"stabilityai/stable-diffusion-3.5-large",
		"bytedance/seedream-4.0",
		"google/imagen-4.0",
		"openai/gpt-image-1",
		// Live OpenRouter image ids (2026-10): generic "image" fragment
		// plus families without it.
		"google/gemini-3-pro-image",
		"openai/gpt-5.4-image-2",
		"google/gemini-nano-banana-2.1",
		"tencent/hy-image-v3.5-preview",
		"krea/krea-2-medium",
		"sourceful/riverflow-v2.5-pro",
		"black-forest-labs/flux-3-image-klein",
		"x-ai/grok-imagine-image-2.0",
	}
	for _, id := range images {
		if got := ClassifyInferenceModel(id); got != ModalityImage {
			t.Errorf("ClassifyInferenceModel(%q) = %q, want image", id, got)
		}
	}
	videos := []string{
		// Live OpenRouter video ids (2026-10): video wins over the
		// image markers (flux-video-edit contains "flux").
		"google/veo-3.1",
		"minimax/hailuo-3",
		"alibaba/wan-3.0",
		"bytedance/seedance-2.5",
		"kwaivgi/kling-v3.0-std",
		"runway/gen-4.5",
		"x-ai/grok-imagine-video-1.5",
		"heygen/heygen-video-1",
		"heygen/avatar-iv",
		"black-forest-labs/flux-video-edit",
		"alibaba/happyhorse-1.1",
	}
	for _, id := range videos {
		if got := ClassifyInferenceModel(id); got != ModalityVideo {
			t.Errorf("ClassifyInferenceModel(%q) = %q, want video", id, got)
		}
	}
	texts := []string{
		"meta-llama/Llama-3.3-70B-Instruct",
		"anthropic/claude-sonnet-4.5",
		"qwen/qwen3-235b-a22b",
		// Near-misses that must stay text (notably inkling-small: "kling-"
		// would match it, so the marker requires the org slash).
		"thinkingmachines/inkling",
		"thinkingmachines/inkling-small",
		"tencent/hunyuan-a13b-instruct",
		"minimax/minimax-m2.5",
		"openrouter/auto",
	}
	for _, id := range texts {
		if got := ClassifyInferenceModel(id); got != ModalityText {
			t.Errorf("ClassifyInferenceModel(%q) = %q, want text", id, got)
		}
	}
}

func TestResolveInferenceModalityIsProviderScoped(t *testing.T) {
	// Hosted video generation only exists on OpenRouter: elsewhere a
	// video-named id keeps working as text (usually a video-input chat
	// model), exactly like before video support existed.
	if got := ResolveInferenceModality(ProviderOpenRouter, "google/veo-3.1"); got != ModalityVideo {
		t.Errorf("openrouter veo = %q, want video", got)
	}
	if got := ResolveInferenceModality(ProviderTogether, "google/veo-3.1"); got != ModalityText {
		t.Errorf("together veo = %q, want text", got)
	}
	if got := ResolveInferenceModality(ProviderOpenRouter, "google/gemini-3-pro-image"); got != ModalityImage {
		t.Errorf("openrouter gemini image = %q, want image", got)
	}
}

func TestDiscoverInferenceModelsOpenAICompatible(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"meta-llama/Llama-3.3-70B-Instruct"},
			{"id":"black-forest-labs/FLUX.1-schnell"},
			{"id":"BAAI/bge-m3"},
			{"id":""}
		]}`))
	}))
	defer srv.Close()

	models, err := DiscoverInferenceModels(context.Background(), ProviderTogether, srv.URL+"/v1", "secret", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("Authorization = %q, want Bearer secret", gotAuth)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(models), models)
	}

	byName := map[string]Model{}
	for _, m := range models {
		byName[m.Name] = m
	}

	text, ok := byName["together/meta-llama/Llama-3.3-70B-Instruct"]
	if !ok {
		t.Fatalf("text model missing: %+v", models)
	}
	if text.Modality != ModalityText || text.Generator != GeneratorOpenAIText || text.Endpoint != "/chat/completions" {
		t.Fatalf("text entry = %+v", text)
	}

	image, ok := byName["together/black-forest-labs/FLUX.1-schnell"]
	if !ok {
		t.Fatalf("image model missing: %+v", models)
	}
	if image.Modality != ModalityImage || image.Generator != GeneratorOpenAIImage {
		t.Fatalf("image entry = %+v", image)
	}
	if image.Endpoint != "/images/generations" {
		t.Fatalf("image endpoint = %q", image.Endpoint)
	}
	if image.BaseURL != srv.URL+"/v1" {
		t.Fatalf("image base url = %q, want the tenant override", image.BaseURL)
	}
}

func TestDiscoverOpenRouterMediaCatalogs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The generic listing also names the media models: the dedicated
		// catalogs below must win (modality + capabilities).
		_, _ = w.Write([]byte(`{"data":[
			{"id":"google/gemini-3-pro-image"},
			{"id":"google/veo-3.1"},
			{"id":"meta-llama/Llama-3.3-70B-Instruct"}
		]}`))
	})
	mux.HandleFunc("/v1/images/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"google/gemini-3-pro-image","name":"Google: Gemini 3 Pro Image",
			 "supported_parameters":{"resolution":{"type":"enum","values":["1K","2K"]},"aspect_ratio":{"type":"enum","values":["1:1","16:9","3:2","auto"]},"input_references":{"type":"range","min":0,"max":14}}},
			{"id":"bytedance-seed/seedream-4.5","name":"Seedream 4.5"}
		]}`))
	})
	mux.HandleFunc("/v1/videos/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"google/veo-3.1","name":"Google: Veo 3.1",
			 "supported_durations":[4,6,8],
			 "supported_resolutions":["720p","1080p"],
			 "supported_aspect_ratios":["16:9","9:16"]}
		]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	models, err := DiscoverInferenceModels(context.Background(), ProviderOpenRouter, srv.URL+"/v1", "key", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	byName := map[string]Model{}
	for _, m := range models {
		byName[m.Name] = m
	}

	img, ok := byName["openrouter/google/gemini-3-pro-image"]
	if !ok {
		t.Fatalf("image model missing: %+v", models)
	}
	if img.Modality != ModalityImage || img.Generator != GeneratorOpenAIImage || img.Endpoint != "/images" {
		t.Fatalf("image entry = %+v", img)
	}
	if len(img.Defaults.Resolutions) != 2 || img.DisplayName != "Google: Gemini 3 Pro Image" {
		t.Fatalf("image capabilities = %+v", img)
	}
	if got := strings.Join(img.Defaults.Ratios, ","); got != "1:1,16:9,3:2" {
		t.Fatalf("image ratios = %q, want the model's advertised set", got)
	}
	if img.MaxReferences != 14 {
		t.Fatalf("image max references = %d, want 14", img.MaxReferences)
	}
	if second := byName["openrouter/bytedance-seed/seedream-4.5"]; second.MaxReferences != 0 {
		t.Fatalf("model without input_references = %d, want 0", second.MaxReferences)
	}

	vid, ok := byName["openrouter/google/veo-3.1"]
	if !ok {
		t.Fatalf("video model missing: %+v", models)
	}
	if vid.Modality != ModalityVideo || vid.Generator != GeneratorOpenRouterVideo || vid.Endpoint != "/videos" {
		t.Fatalf("video entry = %+v", vid)
	}
	if len(vid.Defaults.Durations) != 3 || len(vid.Defaults.Resolutions) != 2 || len(vid.Defaults.Ratios) != 2 {
		t.Fatalf("video capabilities = %+v", vid)
	}

	text, ok := byName["openrouter/meta-llama/Llama-3.3-70B-Instruct"]
	if !ok || text.Modality != ModalityText {
		t.Fatalf("text entry = %+v", text)
	}
}

func TestDiscoverInferenceModelsSkipsImagesWithoutRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"black-forest-labs/FLUX.1-schnell"},{"id":"llama-3.3-70b"}]}`))
	}))
	defer srv.Close()

	models, err := DiscoverInferenceModels(context.Background(), ProviderGMI, srv.URL+"/v1", "key", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// GMI has no OpenAI-compatible images route in the registry: the image
	// model must not be offered, the text one must.
	if len(models) != 1 || models[0].DisplayName != "llama-3.3-70b" {
		t.Fatalf("models = %+v", models)
	}
}

func TestDiscoverInferenceModelsReportsProviderErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := DiscoverInferenceModels(context.Background(), ProviderDeepInfra, srv.URL, "key", ""); err == nil {
		t.Fatal("expected an error for a non-200 listing")
	}
}

func TestDiscoverInferenceModelsRunPod(t *testing.T) {
	models, err := DiscoverInferenceModels(context.Background(), ProviderRunPod, "", "key", "abc12def")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("got %d models, want 1", len(models))
	}
	m := models[0]
	if m.Name != "runpod/abc12def" || m.Modality != ModalityImage || m.Generator != GeneratorRunPod {
		t.Fatalf("runpod entry = %+v", m)
	}
	if m.Endpoint != "/v2/abc12def" {
		t.Fatalf("endpoint = %q, want /v2/abc12def", m.Endpoint)
	}
	if m.BaseURL != "https://api.runpod.ai" {
		t.Fatalf("base url = %q", m.BaseURL)
	}

	// No endpoint configured: nothing to list, and no error either.
	empty, err := DiscoverInferenceModels(context.Background(), ProviderRunPod, "", "key", "")
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty runpod = (%v, %v)", empty, err)
	}
}

func TestInferenceModelEndpointDefaultsToChat(t *testing.T) {
	if got := InferenceModelEndpoint(ProviderTogether, ModalityText); got != "/chat/completions" {
		t.Fatalf("text endpoint = %q", got)
	}
	if got := InferenceModelEndpoint(ProviderOpenRouter, ModalityImage); got != "/images" {
		t.Fatalf("openrouter image endpoint = %q", got)
	}
}

func TestInferenceProviderNamesAreKnown(t *testing.T) {
	names := strings.Join(InferenceProviderNames(), ",")
	for _, want := range []string{"runpod", "together", "deepinfra", "gmi", "baseten", "vultr", "digitalocean", "coreweave", "openrouter"} {
		if !strings.Contains(names, want) {
			t.Errorf("provider %q missing from %s", want, names)
		}
	}
}
