package agencyvideo

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"synapta/internal/modules/agency"
	"synapta/internal/modules/model"
)

func openRouterVideoServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/videos", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("submit method = %s, want POST", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer key" {
			t.Errorf("submit auth = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"job-1","polling_url":"/videos/job-1","status":"pending"}`))
	})
	mux.HandleFunc("/videos/job-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Absolute download URL like the real API (authorized: needs the key).
		clip := "http://" + r.Host + "/clip.mp4"
		_, _ = w.Write([]byte(`{"id":"job-1","status":"completed",` +
			`"unsigned_urls":["` + clip + `"],` +
			`"usage":{"cost":0.5}}`))
	})
	mux.HandleFunc("/clip.mp4", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("fake-mp4-bytes"))
	})
	mux.HandleFunc("/videos/job-1/content", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fake-mp4-bytes"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func openRouterVideoRequest(model string) *agency.GeneratorRequest {
	return &agency.GeneratorRequest{
		Model:      model,
		Endpoint:   "/videos",
		Content:    []agency.ContentItem{{Type: "text", Text: "a red panda astronaut"}},
		Duration:   5,
		Ratio:      "16:9",
		Resolution: "720p",
		AuthKey:    "key",
	}
}

func TestOpenRouterVideoMatch(t *testing.T) {
	g := NewOpenRouterVideoGenerator()
	cases := map[string]bool{
		"openrouter/google/veo-3.1":            true,
		"openrouter/minimax/hailuo-3":          true,
		"openrouter/google/gemini-3-pro-image": false,
		"openrouter/meta-llama/llama-3.3":      false,
		"together/google/veo-3.1":              false,
		"runpod/abc123":                        false,
	}
	for name, want := range cases {
		if got := g.Match(name); got != want {
			t.Errorf("Match(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestOpenRouterVideoBuildPayload(t *testing.T) {
	g := NewOpenRouterVideoGenerator()
	req := openRouterVideoRequest("openrouter/google/veo-3.1")
	req.GenerateAudio = true
	req.Seed = "7"
	p := g.BuildPayload(req)
	if p["model"] != "google/veo-3.1" {
		t.Fatalf("model = %v, want provider prefix stripped", p["model"])
	}
	if p["duration"] != 5 || p["aspect_ratio"] != "16:9" || p["resolution"] != "720p" {
		t.Fatalf("payload = %v", p)
	}
	if p["generate_audio"] != true || p["seed"] != 7 {
		t.Fatalf("payload = %v", p)
	}
}

func TestOpenRouterVideoGenerateAndStatus(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := openRouterVideoServer(t)
	g := NewOpenRouterVideoGenerator()

	req := openRouterVideoRequest("openrouter/google/veo-3.1")
	req.BaseURL = srv.URL
	res, err := g.Generate(req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.TaskID != "job-1" || res.Status != "running" {
		t.Fatalf("submit = %+v", res)
	}

	st, err := g.GetStatus("job-1", "key", srv.URL, "/videos")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.Status != "succeeded" {
		t.Fatalf("status = %+v", st)
	}
	if len(st.Outputs) != 1 || st.Outputs[0].LocalURL == "" {
		t.Fatalf("outputs = %+v", st.Outputs)
	}
	if st.CostUSD != 0.5 {
		t.Fatalf("cost = %v, want usage cost 0.5", st.CostUSD)
	}
}

func TestOpenRouterVideoValidate(t *testing.T) {
	g := NewOpenRouterVideoGenerator()
	if err := g.Validate(openRouterVideoRequest("openrouter/google/veo-3.1")); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	bad := openRouterVideoRequest("openrouter/google/veo-3.1")
	bad.Resolution = "8K"
	if err := g.Validate(bad); err == nil {
		t.Fatal("expected resolution error")
	}
	if err := g.CancelTask("job-1", "", "", ""); err == nil ||
		!strings.Contains(err.Error(), "cannot be cancelled") {
		t.Fatalf("cancel = %v, want honest unsupported error", err)
	}
	if model.GeneratorOpenRouterVideo != "openrouter-video" {
		t.Fatal("generator name constant drift")
	}
}
