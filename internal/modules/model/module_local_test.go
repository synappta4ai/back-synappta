package model

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// fakeCredentialLister stands in for the tenant credential handler: it reports
// one usable provider and the base URL of the local servers it configured.
type fakeCredentialLister struct {
	usable    map[string]bool
	endpoints map[string]string
}

func (f fakeCredentialLister) UsableProviders() (map[string]bool, error) { return f.usable, nil }

func (f fakeCredentialLister) LocalModelEndpoints() (map[string]string, error) {
	return f.endpoints, nil
}

// listModels serves GET /models through the real handler and decodes the
// catalog it answered.
func listModels(t *testing.T, lister CredentialLister, query string) []Model {
	t.Helper()
	gin.SetMode(gin.TestMode)

	m := NewModule(func(*gin.Context) (CredentialLister, bool) { return lister, true })
	router := gin.New()
	router.GET("/models", m.List)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/models"+query, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var payload struct {
		Data []Model `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
	}
	return payload.Data
}

func TestListIncludesDiscoveredLocalModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen2.5:7b"}]}`))
	}))
	defer srv.Close()

	lister := fakeCredentialLister{
		usable:    map[string]bool{string(ProviderOllama): true},
		endpoints: map[string]string{string(ProviderOllama): srv.URL},
	}

	models := listModels(t, lister, "?modality=text")
	var found *Model
	for i := range models {
		if models[i].Name == "ollama/qwen2.5:7b" {
			found = &models[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("local model missing from listing: %+v", models)
	}
	if found.CredentialProvider != ProviderOllama || found.Generator != "openai-text" {
		t.Errorf("entry = %+v", found)
	}
	if found.Type != TypeAPI || found.Modality != ModalityText {
		t.Errorf("type/modality = %q/%q", found.Type, found.Modality)
	}
}

func TestListSkipsUnreachableLocalServers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	lister := fakeCredentialLister{
		endpoints: map[string]string{string(ProviderLMStudio): url},
	}

	models := listModels(t, lister, "")
	for _, m := range models {
		if m.CredentialProvider == ProviderLMStudio {
			t.Fatalf("stopped server contributed %q", m.Name)
		}
	}
}

func TestListKeepsLocalModelsOutOfMediaModalities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen2.5:7b"}]}`))
	}))
	defer srv.Close()

	lister := fakeCredentialLister{
		usable:    map[string]bool{string(ProviderOllama): true},
		endpoints: map[string]string{string(ProviderOllama): srv.URL},
	}

	for _, m := range listModels(t, lister, "?modality=video") {
		if m.CredentialProvider == ProviderOllama {
			t.Fatalf("text model %q leaked into the video listing", m.Name)
		}
	}
}
