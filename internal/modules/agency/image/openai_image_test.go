package agencyimage

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"synapta/internal/modules/agency"
)

func TestOpenAIImageMatch(t *testing.T) {
	g := NewOpenAIImageGenerator()
	cases := map[string]bool{
		"together/black-forest-labs/FLUX.1-schnell":  true,
		"deepinfra/black-forest-labs/FLUX-1-schnell": true,
		"openrouter/bytedance-seed/seedream-4.5":     true,
		"together/meta-llama/Llama-3.3-70B-Instruct": false, // text
		"gmi/black-forest-labs/FLUX.1-schnell":       false, // no image route
		"runpod/abc12def":                            false, // own generator
		"ollama/qwen2.5:7b":                          false,
		"gemini-nano-banana":                         false,
	}
	for name, want := range cases {
		if got := g.Match(name); got != want {
			t.Errorf("Match(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestOpenAIImageBuildPayload(t *testing.T) {
	g := NewOpenAIImageGenerator()
	req := &agency.GeneratorRequest{
		Model:   "together/black-forest-labs/FLUX.1-schnell",
		Content: []agency.ContentItem{{Type: "text", Text: "un faro al atardecer"}},
		Ratio:   "16:9",
		Seed:    "42",
	}
	payload := g.BuildPayload(req)

	if payload["model"] != "black-forest-labs/FLUX.1-schnell" {
		t.Fatalf("model = %v, want the provider-side id", payload["model"])
	}
	if payload["size"] != "1344x768" {
		t.Fatalf("size = %v", payload["size"])
	}
	if payload["seed"] != 42 {
		t.Fatalf("seed = %v", payload["seed"])
	}
	if _, has := payload["response_format"]; has {
		t.Fatal("response_format must stay unset: not every provider accepts it")
	}
}

func TestOpenAIImageValidateRejectsReferenceImages(t *testing.T) {
	g := NewOpenAIImageGenerator()
	req := &agency.GeneratorRequest{
		Model: "together/black-forest-labs/FLUX.1-schnell",
		Content: []agency.ContentItem{
			{Type: "text", Text: "remix"},
			{Type: "image", DataURL: "data:image/png;base64,AAAA"},
		},
	}
	if err := g.Validate(req); err == nil {
		t.Fatal("expected a validation error for reference images")
	}
}

func TestOpenAIImageGenerateReadsURL(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"url":"https://cdn.example.com/out.png"}]}`))
	}))
	defer srv.Close()

	g := NewOpenAIImageGenerator()
	req := &agency.GeneratorRequest{
		Model:    "together/black-forest-labs/FLUX.1-schnell",
		Content:  []agency.ContentItem{{Type: "text", Text: "test"}},
		BaseURL:  srv.URL + "/v1",
		Endpoint: "/images/generations",
		AuthKey:  "secret",
	}
	result, err := g.Generate(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/v1/images/generations" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if len(result.Outputs) != 1 || result.Outputs[0].URL != "https://cdn.example.com/out.png" {
		t.Fatalf("outputs = %+v", result.Outputs)
	}
}

func TestOpenAIImageGenerateReadsBase64(t *testing.T) {
	// 1x1 transparent PNG.
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg=="
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + png + `"}]}`))
	}))
	defer srv.Close()

	g := NewOpenAIImageGenerator()
	req := &agency.GeneratorRequest{
		Model:    "deepinfra/black-forest-labs/FLUX-1-schnell",
		Content:  []agency.ContentItem{{Type: "text", Text: "test"}},
		BaseURL:  srv.URL,
		Endpoint: "/images/generations",
	}
	result, err := g.Generate(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Outputs) != 1 {
		t.Fatalf("outputs = %+v", result.Outputs)
	}
	if result.Outputs[0].Type != "image" {
		t.Fatalf("output type = %q", result.Outputs[0].Type)
	}
	// The base64 payload is persisted under /outputs and served from there.
	if len(result.Outputs[0].URL) < len("/outputs/") || result.Outputs[0].URL[:len("/outputs/")] != "/outputs/" {
		t.Fatalf("url = %q, want an /outputs path", result.Outputs[0].URL)
	}
	removeOutputFile(t, result.Outputs[0].URL)
}

func TestOpenAIImageGenerateReadsUsageCost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"url":"https://cdn.example.com/out.png"}],"usage":{"cost":0.012}}`))
	}))
	defer srv.Close()

	g := NewOpenAIImageGenerator()
	req := &agency.GeneratorRequest{
		Model:    "openrouter/google/gemini-3.1-flash-image-preview",
		Content:  []agency.ContentItem{{Type: "text", Text: "test"}},
		BaseURL:  srv.URL,
		Endpoint: "/images/generations",
		AuthKey:  "secret",
	}
	result, err := g.Generate(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CostUSD != 0.012 {
		t.Fatalf("CostUSD = %v, want 0.012", result.CostUSD)
	}
}

func TestOpenAIImageValidateAcceptsOpenRouterReferences(t *testing.T) {
	g := NewOpenAIImageGenerator()
	req := &agency.GeneratorRequest{
		Model: "openrouter/google/gemini-3-pro-image-preview",
		Content: []agency.ContentItem{
			{Type: "text", Text: "storyboard"},
			{Type: "image", DataURL: dataURL(noisyPNG(t, 64, 64))},
		},
	}
	if err := g.Validate(req); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestOpenAIImageValidateRejectsTooManyOpenRouterReferences(t *testing.T) {
	g := NewOpenAIImageGenerator()
	content := []agency.ContentItem{{Type: "text", Text: "storyboard"}}
	for i := 0; i < 17; i++ {
		content = append(content, agency.ContentItem{Type: "image", DataURL: dataURL(noisyPNG(t, 8, 8))})
	}
	req := &agency.GeneratorRequest{
		Model:   "openrouter/openai/gpt-image-1",
		Content: content,
	}
	if err := g.Validate(req); err == nil {
		t.Fatal("expected a validation error for more than 16 references")
	}
}

func TestOpenAIImageBuildPayloadOpenRouterReferences(t *testing.T) {
	g := NewOpenAIImageGenerator()
	req := &agency.GeneratorRequest{
		Model: "openrouter/google/gemini-3-pro-image-preview",
		Content: []agency.ContentItem{
			{Type: "text", Text: "storyboard"},
			{Type: "image", DataURL: dataURL(noisyPNG(t, 64, 64))},
		},
		Ratio:      "3:2",
		Resolution: "2K",
	}
	payload := g.BuildPayload(req)

	if _, has := payload["size"]; has {
		t.Fatal("OpenRouter payload must not send size together with aspect_ratio/resolution")
	}
	if payload["aspect_ratio"] != "3:2" {
		t.Fatalf("aspect_ratio = %v", payload["aspect_ratio"])
	}
	if payload["resolution"] != "2K" {
		t.Fatalf("resolution = %v", payload["resolution"])
	}
	refs, ok := payload["input_references"].([]map[string]interface{})
	if !ok || len(refs) != 1 {
		t.Fatalf("input_references = %#v, want one entry", payload["input_references"])
	}
	imageURL := refs[0]["image_url"].(map[string]interface{})["url"].(string)
	if !strings.HasPrefix(imageURL, "data:image/png;base64,") {
		t.Fatalf("reference url = %.40q, want a base64 data URL", imageURL)
	}
}

func TestOpenAIImageBuildPayloadOpenRouterCompactsLargeReference(t *testing.T) {
	g := NewOpenAIImageGenerator()
	req := &agency.GeneratorRequest{
		Model: "openrouter/google/gemini-3-pro-image-preview",
		Content: []agency.ContentItem{
			{Type: "text", Text: "storyboard"},
			{Type: "image", DataURL: dataURL(noisyPNG(t, 3000, 3000))},
		},
	}
	payload := g.BuildPayload(req)

	refs, ok := payload["input_references"].([]map[string]interface{})
	if !ok || len(refs) != 1 {
		t.Fatalf("input_references = %#v, want one entry", payload["input_references"])
	}
	imageURL := refs[0]["image_url"].(map[string]interface{})["url"].(string)
	if len(imageURL) > openRouterInlineBudget {
		t.Errorf("reference data URL = %d bytes, want <= %d", len(imageURL), openRouterInlineBudget)
	}
	if !strings.HasPrefix(imageURL, "data:image/jpeg;base64,") {
		t.Errorf("reference url prefix = %.30q, want a compacted JPEG", imageURL)
	}
}
