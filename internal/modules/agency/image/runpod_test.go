package agencyimage

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"synapta/config"
	"synapta/internal/modules/agency"
)

func TestRunPodMatch(t *testing.T) {
	g := NewRunPodGenerator()
	cases := map[string]bool{
		"runpod/abc12def": true,
		"runpod/xyz":      true,
		"together/black-forest-labs/FLUX.1-schnell": false,
		"ollama/qwen2.5:7b":                         false,
		"gemini-nano-banana":                        false,
	}
	for name, want := range cases {
		if got := g.Match(name); got != want {
			t.Errorf("Match(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRunPodBuildPayload(t *testing.T) {
	g := NewRunPodGenerator()
	req := &agency.GeneratorRequest{
		Model:   "runpod/abc12def",
		Content: []agency.ContentItem{{Type: "text", Text: "un faro"}},
		Ratio:   "9:16",
		Seed:    "7",
	}
	payload := g.BuildPayload(req)
	input, ok := payload["input"].(map[string]interface{})
	if !ok {
		t.Fatalf("payload has no input envelope: %+v", payload)
	}
	if input["prompt"] != "un faro." {
		t.Fatalf("prompt = %v", input["prompt"])
	}
	if input["width"] != 768 || input["height"] != 1344 {
		t.Fatalf("dimensions = %vx%v", input["width"], input["height"])
	}
	if input["seed"] != 7 {
		t.Fatalf("seed = %v", input["seed"])
	}
}

func TestRunPodValidateRequiresEndpoint(t *testing.T) {
	g := NewRunPodGenerator()
	req := &agency.GeneratorRequest{
		Model:   "runpod/abc12def",
		Content: []agency.ContentItem{{Type: "text", Text: "x"}},
	}
	if err := g.Validate(req); err == nil {
		t.Fatal("expected an error when the endpoint id is missing")
	}
	req.Endpoint = "/v2/abc12def"
	if err := g.Validate(req); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestRunPodGenerateSubmitsAsyncJob(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		buf, _ := io.ReadAll(r.Body)
		gotBody = string(buf)
		_, _ = w.Write([]byte(`{"id":"job-1","status":"IN_QUEUE"}`))
	}))
	defer srv.Close()

	g := NewRunPodGenerator()
	req := &agency.GeneratorRequest{
		Model:    "runpod/abc12def",
		Content:  []agency.ContentItem{{Type: "text", Text: "x"}},
		BaseURL:  srv.URL,
		Endpoint: "/v2/abc12def",
		AuthKey:  "key",
	}
	result, err := g.Generate(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/v2/abc12def/run" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer key" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"input"`) {
		t.Fatalf("body = %q, want the RunPod input envelope", gotBody)
	}
	if result.TaskID != "job-1" || result.Status != config.STATUS_RUNNING {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunPodGetStatusCompleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/abc12def/status/job-1" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"job-1","status":"COMPLETED","output":[{"image":"https://cdn.example.com/a.png","seed":1}]}`))
	}))
	defer srv.Close()

	g := NewRunPodGenerator()
	result, err := g.GetStatus("job-1", "key", srv.URL, "/v2/abc12def")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != config.STATUS_SUCCESS {
		t.Fatalf("status = %q", result.Status)
	}
	if len(result.Outputs) != 1 || result.Outputs[0].URL != "https://cdn.example.com/a.png" {
		t.Fatalf("outputs = %+v", result.Outputs)
	}
}

func TestRunPodGetStatusFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"job-1","status":"FAILED","error":"CUDA out of memory"}`))
	}))
	defer srv.Close()

	g := NewRunPodGenerator()
	result, err := g.GetStatus("job-1", "key", srv.URL, "/v2/abc12def")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != config.STATUS_FAILED || result.Error == "" {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunPodGetStatusCompletedWithoutImageFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"COMPLETED","output":{"text":"done"}}`))
	}))
	defer srv.Close()

	g := NewRunPodGenerator()
	result, err := g.GetStatus("job-1", "key", srv.URL, "/v2/abc12def")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != config.STATUS_FAILED {
		t.Fatalf("status = %q, want failed so the task closes", result.Status)
	}
}

func TestCollectRunPodImageStringsIgnoresPrompts(t *testing.T) {
	output := map[string]interface{}{
		"prompt": "un faro al atardecer",
		"images": []interface{}{
			"https://cdn.example.com/a.png",
			"aGVsbG8gd29ybGQ=", // small base64 that is not an image
		},
		"nested": map[string]interface{}{"image_url": "https://cdn.example.com/b.png"},
	}
	got := collectRunPodImageStrings(output, nil)
	if len(got) != 2 {
		t.Fatalf("got %v, want the two URLs only", got)
	}
}
