package tenant

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"synapta/internal/modules/file"
)

// fakeStorage stands in for a tenant's file handler: it records what the
// platform endpoint forwarded without touching a bucket or a database.
type fakeStorage struct {
	cfg     *file.StorageConfig
	saved   *file.StorageConfigRequest
	deleted bool
	err     error
}

func (f *fakeStorage) StorageConfigForAdmin() (*file.StorageConfig, error) {
	return f.cfg, f.err
}

func (f *fakeStorage) SaveStorageConfigForAdmin(req file.StorageConfigRequest) (*file.StorageConfig, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.saved = &req
	return &file.StorageConfig{
		Configured: true, Endpoint: req.Endpoint, Bucket: req.Bucket,
		Enabled: true, AccessKey: "AKIA••••3456", HasSecret: true,
	}, nil
}

func (f *fakeStorage) DeleteStorageConfigForAdmin() error {
	f.deleted = true
	return f.err
}

// newStorageRouter registers the tenant module like main does, with an auth
// stub that sets role_level so the platform guard can be exercised.
func newStorageRouter(level float64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	noop := func(c *gin.Context) { c.Next() }
	auth := func(c *gin.Context) {
		c.Set("role_level", level)
		c.Next()
	}
	hdl := NewHandler(NewService(NewStore(nil), nil, nil, ""))
	NewModule(hdl).Register(r.Group("/api/v1"), auth, noop, noop)
	return r
}

func withStorageResolver(t *testing.T, fake *fakeStorage, found bool) {
	t.Helper()
	SetTenantStorageResolver(func(id int64) (StorageAdmin, error) {
		if !found {
			return nil, nil
		}
		return fake, nil
	})
	t.Cleanup(func() { SetTenantStorageResolver(nil) })
}

func doStorage(t *testing.T, r http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestTenantStorageReadAndDelete(t *testing.T) {
	fake := &fakeStorage{cfg: &file.StorageConfig{Configured: true, Bucket: "acme-media", Enabled: true}}
	withStorageResolver(t, fake, true)
	r := newStorageRouter(0)

	w := doStorage(t, r, http.MethodGet, "/api/v1/tenants/7/storage", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d (%s)", w.Code, w.Body.String())
	}
	var got struct {
		Data file.StorageConfig `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("respuesta no es JSON: %v", err)
	}
	if !got.Data.Configured || got.Data.Bucket != "acme-media" {
		t.Fatalf("config = %+v", got.Data)
	}

	w = doStorage(t, r, http.MethodDelete, "/api/v1/tenants/7/storage", nil)
	if w.Code != http.StatusOK || !fake.deleted {
		t.Fatalf("DELETE status = %d deleted = %v", w.Code, fake.deleted)
	}
}

func TestTenantStorageSaveForwardsPayload(t *testing.T) {
	fake := &fakeStorage{}
	withStorageResolver(t, fake, true)
	r := newStorageRouter(0)

	body := map[string]any{
		"endpoint":          "https://minio.example.com",
		"bucket":            "acme-media",
		"region":            "us-east-1",
		"access_key_id":     "AKIAEXAMPLE123456",
		"secret_access_key": "secret",
		"public_base":       "https://cdn.example.com/acme",
		"enabled":           true,
	}
	w := doStorage(t, r, http.MethodPut, "/api/v1/tenants/7/storage", body)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d (%s)", w.Code, w.Body.String())
	}
	if fake.saved == nil || fake.saved.Bucket != "acme-media" || fake.saved.SecretKey != "secret" {
		t.Fatalf("payload no llegó al servicio: %+v", fake.saved)
	}
	var got struct {
		Data file.StorageConfig `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("respuesta no es JSON: %v", err)
	}
	// Los secretos nunca vuelven en claro.
	if got.Data.AccessKey != "AKIA••••3456" || !got.Data.HasSecret {
		t.Fatalf("config devuelta = %+v", got.Data)
	}
}

func TestTenantStorageRequiresPlatformSuperadmin(t *testing.T) {
	withStorageResolver(t, &fakeStorage{}, true)
	r := newStorageRouter(2)

	if w := doStorage(t, r, http.MethodGet, "/api/v1/tenants/7/storage", nil); w.Code != http.StatusForbidden {
		t.Fatalf("GET como admin de tenant: status %d, want 403", w.Code)
	}
}

func TestTenantStorageUnknownTenant(t *testing.T) {
	withStorageResolver(t, &fakeStorage{}, false)
	r := newStorageRouter(0)

	if w := doStorage(t, r, http.MethodGet, "/api/v1/tenants/999/storage", nil); w.Code != http.StatusNotFound {
		t.Fatalf("tenant inexistente: status %d, want 404 (%s)", w.Code, w.Body.String())
	}
}

func TestTenantStorageSaveReportsProbeFailure(t *testing.T) {
	withStorageResolver(t, &fakeStorage{err: errors.New("el bucket rechazó las credenciales (403)")}, true)
	r := newStorageRouter(0)

	w := doStorage(t, r, http.MethodPut, "/api/v1/tenants/7/storage", map[string]any{
		"endpoint": "https://minio.example.com", "bucket": "acme-media",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("fallo del probe: status %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("403")) {
		t.Fatalf("el mensaje del probe debería llegar al cliente: %s", w.Body.String())
	}
}
