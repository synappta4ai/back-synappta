package agencytext

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"synapta/internal/modules/agency"
)

func localRequest(modelName, baseURL string) *agency.GeneratorRequest {
	return &agency.GeneratorRequest{
		Model:    modelName,
		Modality: "text",
		Content:  []agency.ContentItem{{Type: "text", Text: "Escribí un guion de 30 segundos"}},
		BaseURL:  baseURL,
		Endpoint: "/chat/completions",
	}
}

func TestOpenAITextGeneratorMatch(t *testing.T) {
	g := NewOpenAITextGenerator()

	for _, name := range []string{"ollama/qwen2.5:7b", "lmstudio/qwen2.5-7b-instruct", "ollama/llama3.2"} {
		if !g.Match(name) {
			t.Errorf("Match(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"claude-text", "gemini-nano-banana", "dreamina-seedance-2-0-260128", "qwen2.5:7b"} {
		if g.Match(name) {
			t.Errorf("Match(%q) = true, want false", name)
		}
	}
}

func TestOpenAITextGeneratorBuildPayloadStripsPrefix(t *testing.T) {
	g := NewOpenAITextGenerator()
	payload := g.BuildPayload(localRequest("ollama/qwen2.5:7b", "http://localhost:11434/v1"))

	if payload["model"] != "qwen2.5:7b" {
		t.Errorf("model = %v, want the bare server id", payload["model"])
	}
	if payload["stream"] != false {
		t.Errorf("stream = %v, want false (the pipeline submits synchronously)", payload["stream"])
	}
	messages, ok := payload["messages"].([]map[string]interface{})
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v", payload["messages"])
	}
	if messages[0]["role"] != "user" || !strings.Contains(messages[0]["content"].(string), "guion") {
		t.Errorf("messages[0] = %#v", messages[0])
	}
}

func TestOpenAITextGeneratorGenerate(t *testing.T) {
	var (
		gotPath   string
		gotAuth   string
		gotBody   map[string]interface{}
		responded = `{"choices":[{"message":{"role":"assistant","content":"LISTO: guion corto"}}]}`
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responded))
	}))
	defer srv.Close()

	g := NewOpenAITextGenerator()
	req := localRequest("lmstudio/qwen2.5-7b-instruct", srv.URL)

	result, err := g.Generate(req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want no header without a key", gotAuth)
	}
	if gotBody["model"] != "qwen2.5-7b-instruct" {
		t.Errorf("sent model = %v", gotBody["model"])
	}
	if result.Status != "succeeded" {
		t.Errorf("status = %q", result.Status)
	}
	if result.TaskID == "" || !strings.HasPrefix(result.TaskID, "local_text_") {
		t.Errorf("task id = %q", result.TaskID)
	}
	if len(result.Outputs) != 1 {
		t.Fatalf("outputs = %#v", result.Outputs)
	}

	const prefix = "data:text/plain;base64,"
	url := result.Outputs[0].URL
	if !strings.HasPrefix(url, prefix) || result.Outputs[0].Type != "text" {
		t.Fatalf("output = %#v", result.Outputs[0])
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, prefix))
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if string(decoded) != "LISTO: guion corto" {
		t.Errorf("decoded text = %q", decoded)
	}
}

// Un base URL sin /v1 (lo que la gente escribe) se completa solo: Ollama y
// LM Studio sólo exponen las rutas OpenAI bajo /v1.
func TestOpenAITextGeneratorCompletesV1(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	g := NewOpenAITextGenerator()
	if _, err := g.Generate(localRequest("ollama/qwen2.5:7b", srv.URL)); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
}

func TestOpenAITextGeneratorSendsKeyWhenConfigured(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	g := NewOpenAITextGenerator()
	req := localRequest("ollama/qwen2.5:7b", srv.URL)
	req.AuthKey = "local-token"

	if _, err := g.Generate(req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if gotAuth != "Bearer local-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

func TestOpenAITextGeneratorGenerateErrors(t *testing.T) {
	t.Run("respuesta 500", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"model not loaded"}}`))
		}))
		defer srv.Close()

		g := NewOpenAITextGenerator()
		_, err := g.Generate(localRequest("ollama/qwen2.5:7b", srv.URL))
		if err == nil || !strings.Contains(err.Error(), "model not loaded") {
			t.Fatalf("err = %v, want the provider message", err)
		}
	})

	t.Run("completion vacía", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"   "}}]}`))
		}))
		defer srv.Close()

		g := NewOpenAITextGenerator()
		if _, err := g.Generate(localRequest("ollama/qwen2.5:7b", srv.URL)); err == nil {
			t.Fatal("expected an error for an empty completion")
		}
	})

	t.Run("servidor caído", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()

		g := NewOpenAITextGenerator()
		if _, err := g.Generate(localRequest("ollama/qwen2.5:7b", url)); err == nil {
			t.Fatal("expected an error for an unreachable server")
		}
	})

	t.Run("sin base url", func(t *testing.T) {
		g := NewOpenAITextGenerator()
		req := localRequest("ollama/qwen2.5:7b", "")
		if _, err := g.Generate(req); err == nil {
			t.Fatal("expected an error without a base URL")
		}
	})
}

func TestOpenAITextGeneratorValidate(t *testing.T) {
	g := NewOpenAITextGenerator()

	if err := g.Validate(localRequest("ollama/qwen2.5:7b", "http://localhost:11434/v1")); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}

	withResolution := localRequest("ollama/qwen2.5:7b", "http://localhost:11434/v1")
	withResolution.Resolution = "1080p"
	if err := g.Validate(withResolution); err == nil {
		t.Fatal("expected resolution to be rejected for text")
	}

	notLocal := localRequest("claude-text", "http://localhost:11434/v1")
	if err := g.Validate(notLocal); err == nil {
		t.Fatal("expected a non-local model name to be rejected")
	}

	emptyContent := localRequest("ollama/qwen2.5:7b", "http://localhost:11434/v1")
	emptyContent.Content = nil
	if err := g.Validate(emptyContent); err == nil {
		t.Fatal("expected empty content to be rejected")
	}
}
