package file

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	_ "github.com/jackc/pgx/v5/stdlib"

	"synapta/internal/storage"
)

const testEncKey = "0123456789abcdef0123456789abcdef"

// newTestStore opens TEST_DATABASE_URL and applies the real storage_config
// migration. It is skipped unless the variable points to a disposable database:
//
//	TEST_DATABASE_URL=postgres://postgres:pass@127.0.0.1:15432/postgres?sslmode=disable \
//	go test ./internal/modules/file -run TestStorageConfig -v
func newTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL no seteado: tests de storage_config omitidos")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// La base de TEST_DATABASE_URL es descartable: se rehace el schema para
	// que cada test aplique las migraciones reales sobre un lienzo limpio,
	// sin arrastrar tablas de una corrida anterior (roles, tenants, ...).
	if _, err := db.Exec(`DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	// Core (files) + storage_config + backend: las migraciones reales, no un
	// esquema inventado por el test.
	for _, m := range []string{"00001_core.sql", "00007_tenant_storage.sql", "00008_file_backend.sql"} {
		applyMigration(t, db, m)
	}

	store, err := NewStore(db, t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store, db
}

// stubProbe bypasses the live HEAD (the real one is covered by the live tests).
func stubProbe(t *testing.T) {
	t.Helper()
	original := bucketProbe
	bucketProbe = func(*storage.Presigner) error { return nil }
	t.Cleanup(func() { bucketProbe = original })
}

// applyMigration runs the Up section of a tenant migration.
func applyMigration(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	raw, err := os.ReadFile("../../../migrations/tenant/" + name)
	if err != nil {
		t.Fatalf("migración %s: %v", name, err)
	}
	ddl := string(raw)
	if i := strings.Index(ddl, "-- +goose Down"); i >= 0 {
		ddl = ddl[:i]
	}
	var stmt strings.Builder
	for _, line := range strings.Split(ddl, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") || strings.TrimSpace(line) == "" {
			continue
		}
		stmt.WriteString(line)
		stmt.WriteString("\n")
	}
	if _, err := db.Exec(stmt.String()); err != nil {
		t.Fatalf("aplicando %s: %v", name, err)
	}
}

func TestStorageConfigDefaultsToLocal(t *testing.T) {
	store, _ := newTestStore(t)
	svc := NewService(store, "", "acme", testEncKey, 10<<20, true)

	cfg, err := svc.StorageConfigRead()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Configured {
		t.Fatal("un tenant nuevo no debería tener bucket")
	}
	if _, err := svc.PresignUpload(PresignRequest{Filename: "x.png"}); !errors.Is(err, ErrStorageDisabled) {
		t.Fatalf("sin bucket, presign = %v, want ErrStorageDisabled", err)
	}
}

func TestStorageConfigRoundTrip(t *testing.T) {
	store, db := newTestStore(t)
	stubProbe(t)
	svc := NewService(store, "", "acme", testEncKey, 10<<20, true)

	saved, err := svc.SaveStorageConfig(StorageConfigRequest{
		Endpoint:   "https://bucket.example.com/",
		Bucket:     " Media ",
		Region:     "US-EAST-1",
		AccessKey:  "AKIAEXAMPLE123456",
		SecretKey:  "secret-key-example-1234567890",
		PublicBase: "https://cdn.example.com/",
	})
	if err != nil {
		t.Fatalf("SaveStorageConfig: %v", err)
	}
	if !saved.Configured || saved.Bucket != "media" || saved.Region != "us-east-1" {
		t.Fatalf("config guardada inesperada: %+v", saved)
	}
	if saved.Endpoint != "https://bucket.example.com" || saved.PublicBase != "https://cdn.example.com" {
		t.Errorf("normalización de URLs: %+v", saved)
	}
	if saved.AccessKey != "AKIA••••3456" {
		t.Errorf("access key no enmascarada: %q", saved.AccessKey)
	}
	if !saved.HasSecret {
		t.Error("HasSecret debería ser true")
	}
	if saved.Enabled != true {
		t.Error("enabled debería caer a true por defecto")
	}

	// Los secretos quedan cifrados en la base, nunca en claro.
	var sealedAK, sealedSK string
	if err := db.QueryRow(`SELECT access_key, secret_key FROM storage_config`).Scan(&sealedAK, &sealedSK); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealedAK, "AKIAEXAMPLE") || strings.Contains(sealedSK, "secret-key-example") {
		t.Error("las credenciales se guardaron en claro")
	}

	// El presign usa el bucket del tenant y la key namespaced.
	res, err := svc.PresignUpload(PresignRequest{Filename: "foto.PNG", ContentType: "image/png"})
	if err != nil {
		t.Fatalf("PresignUpload: %v", err)
	}
	keyRe := regexp.MustCompile(`^acme/images/\d{4}/\d{2}/[0-9a-f-]{36}\.png$`)
	if !keyRe.MatchString(res.Key) {
		t.Errorf("key = %q", res.Key)
	}
	if !strings.HasPrefix(res.UploadURL, "https://bucket.example.com/media/acme/images/") {
		t.Errorf("upload_url = %q", res.UploadURL)
	}
	if !strings.HasPrefix(res.PublicURL, "https://cdn.example.com/") {
		t.Errorf("public_url debería usar public_base: %q", res.PublicURL)
	}

	// Re-guardar sin credenciales conserva las anteriores (el front nunca las recibe).
	if _, err := svc.SaveStorageConfig(StorageConfigRequest{
		Endpoint: "https://bucket.example.com",
		Bucket:   "media",
	}); err != nil {
		t.Fatalf("update sin credenciales: %v", err)
	}
	if _, err := svc.PresignUpload(PresignRequest{Filename: "otra.png"}); err != nil {
		t.Fatalf("presign tras update sin credenciales: %v", err)
	}

	// enabled=false vuelve a local sin borrar la config.
	disabled := false
	if _, err := svc.SaveStorageConfig(StorageConfigRequest{
		Endpoint: "https://bucket.example.com",
		Bucket:   "media",
		Enabled:  &disabled,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PresignUpload(PresignRequest{Filename: "x.png"}); !errors.Is(err, ErrStorageDisabled) {
		t.Fatalf("enabled=false debería dar ErrStorageDisabled, got %v", err)
	}

	// Borrar la config devuelve al guardado local.
	if err := svc.DeleteStorageConfig(); err != nil {
		t.Fatal(err)
	}
	cfg, err := svc.StorageConfigRead()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Configured {
		t.Fatal("tras borrar, configured debería ser false")
	}
}

func TestStorageConfigRejectsInvalidPayload(t *testing.T) {
	store, _ := newTestStore(t)
	stubProbe(t)
	svc := NewService(store, "", "acme", testEncKey, 10<<20, true)

	cases := []struct {
		name string
		req  StorageConfigRequest
		want string
	}{
		{"endpoint vacío", StorageConfigRequest{Bucket: "media", AccessKey: "a", SecretKey: "b"}, "endpoint is required"},
		{"endpoint sin esquema", StorageConfigRequest{Endpoint: "bucket.example.com", Bucket: "media", AccessKey: "a", SecretKey: "b"}, "scheme must be"},
		{"bucket inválido", StorageConfigRequest{Endpoint: "https://h", Bucket: "Media_Bucket", AccessKey: "a", SecretKey: "b"}, "invalid bucket"},
		{"bucket corto", StorageConfigRequest{Endpoint: "https://h", Bucket: "ab", AccessKey: "a", SecretKey: "b"}, "invalid bucket"},
		{"región inválida", StorageConfigRequest{Endpoint: "https://h", Bucket: "media", Region: "US EAST", AccessKey: "a", SecretKey: "b"}, "invalid region"},
		{"public base inválida", StorageConfigRequest{Endpoint: "https://h", Bucket: "media", PublicBase: "ftp://x", AccessKey: "a", SecretKey: "b"}, "public_base"},
		{"sin credenciales", StorageConfigRequest{Endpoint: "https://h", Bucket: "media"}, "access_key_id and secret_access_key are required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.SaveStorageConfig(tc.req); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want contiene %q", err, tc.want)
			}
		})
	}
}

// TestStorageConfigRoutes exercises GET/PUT/DELETE /files/storage through the
// real module registration (admin middleware included).
func TestStorageConfigRoutes(t *testing.T) {
	store, _ := newTestStore(t)
	stubProbe(t)
	svc := NewService(store, "", "acme", testEncKey, 10<<20, true)
	r := newRouter(svc)

	call := func(method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/v1/files/storage", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	w := call(http.MethodGet, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"configured":false`) {
		t.Fatalf("GET sin config: %d %s", w.Code, w.Body.String())
	}

	w = call(http.MethodPut, `{"endpoint":"https://bucket.example.com","bucket":"media","access_key_id":"AKIAEXAMPLE123456","secret_access_key":"secret-secret-secret"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	var put struct {
		Data StorageConfig `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &put); err != nil {
		t.Fatal(err)
	}
	if !put.Data.Configured || put.Data.Bucket != "media" || put.Data.AccessKey != "AKIA••••3456" {
		t.Fatalf("PUT devolvió %+v", put.Data)
	}
	if strings.Contains(w.Body.String(), "secret-secret-secret") {
		t.Fatal("la respuesta filtró la secret key")
	}

	if w = call(http.MethodPut, `{"endpoint":"https://bucket.example.com","bucket":"Bad_Bucket"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("PUT inválido: %d %s", w.Code, w.Body.String())
	}

	w = call(http.MethodGet, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"configured":true`) {
		t.Fatalf("GET con config: %d %s", w.Code, w.Body.String())
	}

	if w = call(http.MethodDelete, ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
	}
	if w = call(http.MethodGet, ""); !strings.Contains(w.Body.String(), `"configured":false`) {
		t.Fatalf("GET tras DELETE: %d %s", w.Code, w.Body.String())
	}
}

func TestStorageConfigWritesRequireAdmin(t *testing.T) {
	store, _ := newTestStore(t)
	stubProbe(t)
	svc := NewService(store, "", "acme", testEncKey, 10<<20, true)
	deny := func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"success": false, "message": "admin only"})
	}
	r := newRouterWithAdmin(svc, deny)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/files/storage", strings.NewReader(`{"endpoint":"https://bucket.example.com","bucket":"media"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("PUT sin admin: %d, want 403", w.Code)
	}

	// La lectura no exige admin.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/files/storage", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET sin admin: %d, want 200", w.Code)
	}
}

func TestGuardPublicEndpoint(t *testing.T) {
	t.Parallel()

	// IP literals: el resolver no hace DNS, el test queda hermético.
	for _, endpoint := range []string{
		"http://127.0.0.1:9000",
		"http://10.0.0.5",
		"http://192.168.1.10",
		"http://172.16.0.1",
		"http://169.254.169.254",
		"http://[::1]:9000",
		"http://0.0.0.0",
	} {
		if err := guardPublicEndpoint(endpoint); err == nil {
			t.Errorf("guardPublicEndpoint(%q) = nil, want rechazo", endpoint)
		}
	}
	if err := guardPublicEndpoint("http://8.8.8.8"); err != nil {
		t.Errorf("endpoint público rechazado: %v", err)
	}
}

func TestMaskSecret(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"":                  "",
		"short":             "••••••",
		"AKIAEXAMPLE123456": "AKIA••••3456",
	}
	for in, want := range cases {
		if got := maskSecret(in); got != want {
			t.Errorf("maskSecret(%q) = %q, want %q", in, got, want)
		}
	}
}
