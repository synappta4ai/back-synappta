package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseLocalModel(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantProvider CredentialProvider
		wantID       string
		wantOK       bool
	}{
		{"ollama con tag", "ollama/qwen2.5:7b", ProviderOllama, "qwen2.5:7b", true},
		{"lm studio", "lmstudio/qwen2.5-7b-instruct", ProviderLMStudio, "qwen2.5-7b-instruct", true},
		{"mayúsculas y espacios", " Ollama/llama3.2:latest ", ProviderOllama, "llama3.2:latest", true},
		{"id con barra", "ollama/hf.co/user/repo", ProviderOllama, "hf.co/user/repo", true},
		{"sin proveedor local", "gemini/gemini-3-pro", "", "", false},
		{"sin id", "ollama/", "", "", false},
		{"sin separador", "qwen2.5:7b", "", "", false},
		{"vacío", "", "", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider, id, ok := ParseLocalModel(tc.input)
			if ok != tc.wantOK || provider != tc.wantProvider || id != tc.wantID {
				t.Fatalf("ParseLocalModel(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.input, provider, id, ok, tc.wantProvider, tc.wantID, tc.wantOK)
			}
		})
	}
}

func TestDiscoverLocalModels(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen2.5:7b"},{"id":" llama3.2:latest "},{"id":""}]}`))
	}))
	defer srv.Close()

	models, err := DiscoverLocalModels(context.Background(), ProviderOllama, srv.URL+"/")
	if err != nil {
		t.Fatalf("DiscoverLocalModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("path = %q, want /v1/models (OpenAI route)", gotPath)
	}
	if len(models) != 2 {
		t.Fatalf("len = %d, want 2 (empty ids are dropped): %+v", len(models), models)
	}

	// Sorted by display name: "llama3.2:latest" before "qwen2.5:7b".
	if models[0].Name != "ollama/llama3.2:latest" || models[1].Name != "ollama/qwen2.5:7b" {
		t.Fatalf("names = %q, %q", models[0].Name, models[1].Name)
	}
	first := models[0]
	if first.Modality != ModalityText {
		t.Errorf("modality = %q, want text", first.Modality)
	}
	if first.Type != TypeAPI {
		t.Errorf("type = %q, want api", first.Type)
	}
	if first.Generator != "openai-text" {
		t.Errorf("generator = %q, want openai-text", first.Generator)
	}
	if first.CredentialProvider != ProviderOllama {
		t.Errorf("provider = %q, want ollama", first.CredentialProvider)
	}
	if first.Endpoint != "/chat/completions" {
		t.Errorf("endpoint = %q, want /chat/completions", first.Endpoint)
	}
	if first.BaseURL != srv.URL+"/v1" {
		t.Errorf("base_url = %q, want %q", first.BaseURL, srv.URL+"/v1")
	}
}

func TestNormalizeLocalBaseURL(t *testing.T) {
	tests := map[string]string{
		"http://localhost:11434":        "http://localhost:11434/v1",
		"http://localhost:11434/":       "http://localhost:11434/v1",
		"http://localhost:11434/v1":     "http://localhost:11434/v1",
		"http://localhost:11434/v1/":    "http://localhost:11434/v1",
		"http://localhost:1234/v1":      "http://localhost:1234/v1",
		"https://gw.example.com/openai": "https://gw.example.com/openai",
		"  http://localhost:11434  ":    "http://localhost:11434/v1",
		"":                              "",
	}
	for in, want := range tests {
		if got := NormalizeLocalBaseURL(in); got != want {
			t.Errorf("NormalizeLocalBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiscoverLocalModelsFailsClosed(t *testing.T) {
	t.Run("servidor caído", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()

		if _, err := DiscoverLocalModels(context.Background(), ProviderLMStudio, url); err == nil {
			t.Fatal("expected an error for an unreachable server")
		}
	})

	t.Run("non-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", http.StatusInternalServerError)
		}))
		defer srv.Close()

		if _, err := DiscoverLocalModels(context.Background(), ProviderOllama, srv.URL); err == nil {
			t.Fatal("expected an error for a 500 response")
		}
	})

	t.Run("proveedor no local", func(t *testing.T) {
		if _, err := DiscoverLocalModels(context.Background(), ProviderGemini, "http://localhost:1"); err == nil {
			t.Fatal("expected an error for a non-local provider")
		}
	})
}

func TestDefaultLocalBaseURL(t *testing.T) {
	if got := DefaultLocalBaseURL(ProviderOllama); got != "http://localhost:11434/v1" {
		t.Errorf("ollama default = %q", got)
	}
	if got := DefaultLocalBaseURL(ProviderLMStudio); got != "http://localhost:1234/v1" {
		t.Errorf("lmstudio default = %q", got)
	}
	if got := DefaultLocalBaseURL(ProviderGemini); got != "" {
		t.Errorf("gemini default = %q, want empty", got)
	}
}
