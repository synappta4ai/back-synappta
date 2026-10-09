package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"synapta/internal/modules/credential"
)

func TestExtractAngles(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		raw      string
		wantN    int
		wantOK   bool
		title    string
		hook     string
		selected bool
	}{
		{
			name:     "json puro",
			raw:      `{"angles":[{"title":"Vista al mar","hook":"Hook","narrative_pitch":"Pitch","target_audience":"Familias","selected":true}]}`,
			wantN:    1,
			wantOK:   true,
			title:    "Vista al mar",
			hook:     "Hook",
			selected: true,
		},
		{
			name:   "json en fence markdown",
			raw:    "```json\n{\"angles\":[{\"title\":\"A\",\"hook\":\"H\"}]}\n```",
			wantN:  1,
			wantOK: true,
			title:  "A",
			hook:   "H",
		},
		{
			name:   "json con prosa alrededor",
			raw:    "Acá van los ángulos pedidos:\n{\"angles\":[{\"title\":\"A\"}]}\nEspero que sirva.",
			wantN:  1,
			wantOK: true,
			title:  "A",
		},
		{
			name:   "sin json devuelve false",
			raw:    "No puedo ayudarte con eso.",
			wantOK: false,
		},
		{
			name:   "angles vacío devuelve false",
			raw:    `{"angles":[]}`,
			wantOK: false,
		},
		{
			name:   "campos por defecto desde description",
			raw:    `{"angles":[{"description":"D compartida"}]}`,
			wantN:  1,
			wantOK: true,
			title:  "Ángulo 1",
			hook:   "D compartida",
		},
		{
			name:   "title no string usa default",
			raw:    `{"angles":[{"title":42}]}`,
			wantN:  1,
			wantOK: true,
			title:  "Ángulo 1",
		},
		{
			name:   "varios ángulos",
			raw:    `{"angles":[{"title":"A"},{"title":"B"},{"title":"C"}]}`,
			wantN:  3,
			wantOK: true,
			title:  "A",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			angles, ok := extractAngles(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if len(angles) != tc.wantN {
				t.Fatalf("len=%d, want %d", len(angles), tc.wantN)
			}
			if angles[0]["title"] != tc.title {
				t.Errorf("title=%v, want %q", angles[0]["title"], tc.title)
			}
			if tc.hook != "" && angles[0]["hook"] != tc.hook {
				t.Errorf("hook=%v, want %q", angles[0]["hook"], tc.hook)
			}
			if tc.selected != angles[0]["selected"] {
				t.Errorf("selected=%v, want %v", angles[0]["selected"], tc.selected)
			}
		})
	}
}

func TestParseLLMText(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		raw       string
		anthropic bool
		want      string
		wantErr   string
	}{
		{name: "openai ok", raw: `{"choices":[{"message":{"content":"hola"}}]}`, want: "hola"},
		{name: "openai sin choices", raw: `{}`, wantErr: "vacía"},
		{name: "openai error de api", raw: `{"error":{"message":"model not found"}}`, wantErr: "model not found"},
		{name: "anthropic ok", raw: `{"content":[{"type":"text","text":"hola"}]}`, anthropic: true, want: "hola"},
		{name: "anthropic error de api", raw: `{"error":{"message":"invalid x-api-key"}}`, anthropic: true, wantErr: "invalid x-api-key"},
		{name: "json ilegible", raw: `<html>502</html>`, wantErr: "ilegible"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseLLMText([]byte(tc.raw), tc.anthropic)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want que contenga %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err inesperado: %v", err)
			}
			if got != tc.want {
				t.Errorf("got=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestLLMAnswer(t *testing.T) {
	t.Parallel()

	t.Run("openrouter usa bearer y /chat/completions", func(t *testing.T) {
		t.Parallel()
		var gotPath, gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		}))
		defer srv.Close()

		cred := &credential.Resolve{BaseURL: srv.URL, APIKey: "sk-test"}
		got, err := llmAnswer(context.Background(), srv.Client(), credential.ProviderOpenRouter, cred, "openai/gpt-4o-mini", "sys", "hola")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got != "ok" {
			t.Errorf("got=%q", got)
		}
		if gotPath != "/chat/completions" {
			t.Errorf("path=%q", gotPath)
		}
		if gotAuth != "Bearer sk-test" {
			t.Errorf("auth=%q", gotAuth)
		}
	})

	t.Run("anthropic usa x-api-key y /v1/messages", func(t *testing.T) {
		t.Parallel()
		var gotPath, gotKey string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotKey = r.Header.Get("x-api-key")
			w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
		}))
		defer srv.Close()

		cred := &credential.Resolve{BaseURL: srv.URL, APIKey: "sk-ant"}
		got, err := llmAnswer(context.Background(), srv.Client(), credential.ProviderAnthropic, cred, "claude-3-haiku-20240307", "sys", "hola")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got != "ok" {
			t.Errorf("got=%q", got)
		}
		if gotPath != "/v1/messages" {
			t.Errorf("path=%q", gotPath)
		}
		if gotKey != "sk-ant" {
			t.Errorf("x-api-key=%q", gotKey)
		}
	})

	t.Run("status no-200 lleva el cuerpo al error", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":{"message":"insufficient credits"}}`, http.StatusPaymentRequired)
		}))
		defer srv.Close()

		cred := &credential.Resolve{BaseURL: srv.URL, APIKey: "sk-test"}
		_, err := llmAnswer(context.Background(), srv.Client(), credential.ProviderOpenRouter, cred, "m", "s", "hola")
		if err == nil || !strings.Contains(err.Error(), "insufficient credits") {
			t.Fatalf("err=%v, want insufficient credits", err)
		}
	})

	t.Run("provider sin soporte directo", func(t *testing.T) {
		t.Parallel()
		cred := &credential.Resolve{APIKey: "k"}
		_, err := llmAnswer(context.Background(), http.DefaultClient, credential.ProviderGemini, cred, "m", "s", "hola")
		if err == nil || !strings.Contains(err.Error(), "openrouter") {
			t.Fatalf("err=%v, want mención a openrouter", err)
		}
	})
}

func TestSSEWrite(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	if !sseWrite(c, "meta", map[string]string{"conversation_id": "abc"}) {
		t.Fatal("sseWrite devolvió false")
	}
	want := "event: meta\ndata: {\"conversation_id\":\"abc\"}\n\n"
	if got := w.Body.String(); got != want {
		t.Errorf("body=%q, want %q", got, want)
	}
}

func TestDefaultLLMModel(t *testing.T) {
	t.Parallel()
	if got := defaultLLMModel(credential.ProviderAnthropic); got != anthropicModel {
		t.Errorf("anthropic=%q", got)
	}
	if got := defaultLLMModel(credential.ProviderOpenRouter); got != openrouterModel {
		t.Errorf("openrouter=%q", got)
	}
}
