package file

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"synapta/internal/storage"
)

// newRouter registers the file module exactly like main does (auth/tenant
// middleware stubbed) so the test exercises the real route wiring.
func newRouter(svc *Service) *gin.Engine {
	return newRouterWithAdmin(svc, func(c *gin.Context) { c.Next() })
}

// newRouterWithAdmin lets a test swap the admin guard (storage writes use it).
func newRouterWithAdmin(svc *Service, adminMw gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	noop := func(c *gin.Context) { c.Next() }
	h := NewHandler(svc)
	mod := NewModule(func(*gin.Context) (*Handler, bool) { return h, true }, nil)
	mod.Register(r.Group("/api/v1"), noop, noop, adminMw)
	return r
}

func postPresign(t *testing.T, r http.Handler, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/files/presign", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// /files/presign must not be captured by /files/:id.
func TestPresignRoute(t *testing.T) {
	t.Parallel()

	r := newRouter(testService(t, 10<<20))
	w := postPresign(t, r, PresignRequest{Filename: "foto.png", Size: 100})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var env struct {
		Success bool          `json:"success"`
		Data    PresignResult `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("respuesta no es JSON: %v (%s)", err, w.Body.String())
	}
	if !env.Success || env.Data.UploadURL == "" || env.Data.Method != "PUT" {
		t.Fatalf("respuesta incompleta: %+v", env)
	}
	if got := env.Data.Headers["Content-Type"]; got != "image/png" {
		t.Errorf("headers = %v", env.Data.Headers)
	}
}

func TestPresignRouteValidation(t *testing.T) {
	t.Parallel()

	r := newRouter(testService(t, 10))

	if w := postPresign(t, r, PresignRequest{Filename: "x.exe"}); w.Code != http.StatusBadRequest {
		t.Errorf("ext no permitida: status %d", w.Code)
	}
	if w := postPresign(t, r, PresignRequest{Filename: "x.png", Size: 999}); w.Code != http.StatusBadRequest {
		t.Errorf("size excedida: status %d", w.Code)
	}
	w := postPresign(t, r, map[string]any{"filename": 1})
	if w.Code != http.StatusBadRequest {
		t.Errorf("body inválido: status %d", w.Code)
	}
}

func TestPresignRouteWithoutStorage(t *testing.T) {
	t.Parallel()

	r := newRouter(testServiceWith(t, nil, 0))
	w := postPresign(t, r, PresignRequest{Filename: "x.png"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", w.Code, w.Body.String())
	}
}

// TestPresignEndpointLive goes through the HTTP endpoint and then uploads the
// bytes to a real MinIO with the returned URL — the exact flow the front will
// use. Skipped unless MINIO_TEST_* is set (see storage/live_test.go).
func TestPresignEndpointLive(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT no seteado: test live omitido")
	}
	ak, sk := os.Getenv("MINIO_TEST_ACCESS_KEY"), os.Getenv("MINIO_TEST_SECRET_KEY")
	if ak == "" || sk == "" {
		t.Skip("MINIO_TEST_ACCESS_KEY / MINIO_TEST_SECRET_KEY no seteados")
	}
	bucket := os.Getenv("MINIO_TEST_BUCKET")
	if bucket == "" {
		bucket = "synapta-test"
	}

	p, err := storage.New(storage.Config{
		Endpoint:  endpoint,
		Bucket:    bucket,
		AccessKey: ak,
		SecretKey: sk,
		Region:    "us-east-1",
		TTL:       5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := newRouter(testServiceWith(t, p, 10<<20))

	body := make([]byte, 2048)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	w := postPresign(t, r, PresignRequest{Filename: "prueba.png", ContentType: "image/png", Size: int64(len(body))})
	if w.Code != http.StatusOK {
		t.Fatalf("presign status = %d (%s)", w.Code, w.Body.String())
	}
	var env struct {
		Data PresignResult `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(env.Data.Key, "acme/images/") {
		t.Errorf("key = %q", env.Data.Key)
	}

	req, err := http.NewRequest(http.MethodPut, env.Data.UploadURL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range env.Data.Headers {
		req.Header.Set(name, value)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT directo al bucket falló: %v", err)
	}
	defer res.Body.Close()
	msg, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PUT %s: status %d (%s)", env.Data.Key, res.StatusCode, msg)
	}

	// Self-cleaning: borra el objeto con un DELETE prefirmado.
	t.Cleanup(func() {
		req, err := http.NewRequest(http.MethodDelete, p.PresignDelete(env.Data.Key, time.Now()), nil)
		if err != nil {
			t.Errorf("DELETE de %s: %v", env.Data.Key, err)
			return
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("DELETE de %s falló: %v", env.Data.Key, err)
			return
		}
		defer res.Body.Close()
		if res.StatusCode >= 300 {
			msg, _ := io.ReadAll(res.Body)
			t.Errorf("DELETE de %s: status %d (%s)", env.Data.Key, res.StatusCode, msg)
		}
	})

	if os.Getenv("MINIO_TEST_PUBLIC_READ") != "1" {
		return
	}
	got, err := http.Get(env.Data.PublicURL)
	if err != nil {
		t.Fatalf("GET público falló: %v", err)
	}
	defer got.Body.Close()
	if got.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", env.Data.PublicURL, got.StatusCode)
	}
	data, _ := io.ReadAll(got.Body)
	if !bytes.Equal(data, body) {
		t.Fatalf("contenido distinto: %d vs %d bytes", len(data), len(body))
	}
	t.Logf("endpoint OK: %s → %s", env.Data.Key, env.Data.PublicURL)
}
