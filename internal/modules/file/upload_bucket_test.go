package file

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"synapta/internal/storage"
)

// fakeS3 is a path-style S3 stand-in: it keeps objects in memory and rejects
// any request without presigned SigV4 query parameters. It exercises the
// upload/serve/thumbnail/delete paths deterministically without a real MinIO
// (internal/storage PresignPutLive covers the real signature).
type fakeS3 struct {
	mu       sync.Mutex
	endpoint string
	bucket   string
	objects  map[string]fakeObject
}

type fakeObject struct {
	data        []byte
	contentType string
}

func newFakeS3(t *testing.T, bucket string) *fakeS3 {
	t.Helper()
	f := &fakeS3{bucket: bucket, objects: map[string]fakeObject{}}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	f.endpoint = server.URL
	return f
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("X-Amz-Signature") == "" || q.Get("X-Amz-Credential") == "" {
		http.Error(w, "unsigned request", http.StatusForbidden)
		return
	}
	bucket, key, hasKey := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != f.bucket {
		http.Error(w, "no such bucket", http.StatusNotFound)
		return
	}
	if !hasKey || key == "" {
		if r.Method != http.MethodHead {
			http.Error(w, "only HEAD on the bucket", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	switch r.Method {
	case http.MethodPut:
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.objects[key] = fakeObject{data: data, contentType: r.Header.Get("Content-Type")}
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		f.mu.Lock()
		obj, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "no such key", http.StatusNotFound)
			return
		}
		if obj.contentType != "" {
			w.Header().Set("Content-Type", obj.contentType)
		}
		w.Write(obj.data)
	case http.MethodHead:
		f.mu.Lock()
		obj, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(obj.data)))
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
	}
}

// keys lists the stored object keys, sorted.
func (f *fakeS3) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.objects))
	for k := range f.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// Con bucket configurado, el mismo POST /files/upload escribe en el bucket y
// la lectura redirige a una URL prefirmada: el front no cambia.
func TestUploadRoutesToTenantBucket(t *testing.T) {
	store, _ := newTestStore(t)
	fake := newFakeS3(t, "acme-media")
	svc := NewService(store, "", "acme", testEncKey, 10<<20, true)

	if _, err := svc.SaveStorageConfig(StorageConfigRequest{
		Endpoint:  fake.endpoint,
		Bucket:    fake.bucket,
		Region:    "us-east-1",
		AccessKey: "ak",
		SecretKey: "sk",
	}); err != nil {
		t.Fatalf("SaveStorageConfig (probe firmado): %v", err)
	}
	t.Cleanup(func() { _ = svc.DeleteStorageConfig() })

	data := testPNG(t, 64, 48)
	res, err := svc.Upload(data, "bucket.png", "images", "persistent", false)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	stored, err := svc.GetFile(res.ID)
	if err != nil || stored == nil {
		t.Fatalf("GetFile: %v", err)
	}
	if stored.Backend != "s3" {
		t.Fatalf("backend = %q, want s3", stored.Backend)
	}
	if _, err := os.Stat(filepath.Join(store.uploadDir, stored.Path)); !os.IsNotExist(err) {
		t.Error("los bytes no deberían escribirse en disco cuando hay bucket")
	}
	if keys := fake.keys(); !contains(keys, stored.Path) {
		t.Fatalf("objeto %q ausente del bucket: %v", stored.Path, keys)
	}

	// Servir: redirige al bucket y la URL prefirmada devuelve los bytes.
	local, redirect, err := svc.ServeTarget(res.ID)
	if err != nil || redirect == "" || local != "" {
		t.Fatalf("ServeTarget s3 = (%q, %q, %v)", local, redirect, err)
	}
	got, err := http.Get(redirect)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if got.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("GET prefirmado: status=%d bytes=%d (want %d)", got.StatusCode, len(body), len(data))
	}

	// Miniaturas: se generan on-demand desde el objeto del bucket y se
	// guardan en el mismo bucket.
	thumbLocal, thumbURL, err := svc.ThumbnailTarget(res.ID)
	if err != nil || thumbURL == "" || thumbLocal != "" {
		t.Fatalf("ThumbnailTarget = (%q, %q, %v)", thumbLocal, thumbURL, err)
	}
	if keys := fake.keys(); !contains(keys, thumbnailPath(stored.Path)) {
		t.Fatalf("miniatura %q ausente del bucket: %v", thumbnailPath(stored.Path), keys)
	}
	tres, err := http.Get(thumbURL)
	if err != nil {
		t.Fatal(err)
	}
	tbody, _ := io.ReadAll(tres.Body)
	tres.Body.Close()
	if tres.StatusCode != http.StatusOK || len(tbody) == 0 {
		t.Fatalf("GET miniatura prefirmada: status=%d bytes=%d", tres.StatusCode, len(tbody))
	}

	// GetServePath sigue devolviendo un path local para consumidores internos
	// (agencia, proveedores) aunque el archivo viva en el bucket.
	cachePath, err := svc.GetServePath(res.ID)
	if err != nil {
		t.Fatalf("GetServePath: %v", err)
	}
	if cached, err := os.ReadFile(cachePath); err != nil || !bytes.Equal(cached, data) {
		t.Fatalf("GetServePath devolvió %q sin los bytes: %v", cachePath, err)
	}

	// Borrado definitivo: sale del bucket, del cache y de la base.
	if err := svc.HardDelete(res.ID); err != nil {
		t.Fatalf("HardDelete: %v", err)
	}
	if keys := fake.keys(); len(keys) != 0 {
		t.Fatalf("quedaron objetos en el bucket: %v", keys)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Error("el cache local debería haberse borrado")
	}
}

// El mismo POST /files/upload (multipart) del flujo actual guarda en el
// bucket cuando el tenant lo tiene configurado, y GET /serve redirige.
func TestUploadEndpointStoresInBucket(t *testing.T) {
	store, _ := newTestStore(t)
	fake := newFakeS3(t, "acme-media")
	svc := NewService(store, "", "acme", testEncKey, 10<<20, true)
	if _, err := svc.SaveStorageConfig(StorageConfigRequest{
		Endpoint: fake.endpoint, Bucket: fake.bucket, AccessKey: "ak", SecretKey: "sk",
	}); err != nil {
		t.Fatalf("SaveStorageConfig: %v", err)
	}
	t.Cleanup(func() { _ = svc.DeleteStorageConfig() })
	r := newRouter(svc)

	data := testPNG(t, 80, 60)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("category", "images"); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("file", "handler.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/files/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("upload status = %d (%s)", w.Code, w.Body.String())
	}
	var created struct {
		Data UploadResult `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("respuesta no es JSON: %v", err)
	}
	if created.Data.ID == "" {
		t.Fatalf("sin id en la respuesta: %s", w.Body.String())
	}
	if keys := fake.keys(); len(keys) != 2 { // el archivo y su miniatura
		t.Fatalf("el bucket debería tener el objeto y su miniatura, tiene %v", keys)
	}

	// GET /serve responde 302 hacia el objeto del bucket.
	sreq := httptest.NewRequest(http.MethodGet, "/api/v1/files/"+created.Data.ID+"/serve", nil)
	sw := httptest.NewRecorder()
	r.ServeHTTP(sw, sreq)
	if sw.Code != http.StatusFound {
		t.Fatalf("serve status = %d (%s)", sw.Code, sw.Body.String())
	}
	loc := sw.Header().Get("Location")
	if !strings.Contains(loc, "X-Amz-Signature=") {
		t.Fatalf("Location no está firmado: %q", loc)
	}
	res, err := http.Get(loc)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !bytes.Equal(got, data) {
		t.Fatalf("GET del bucket: status=%d bytes=%d (want %d)", res.StatusCode, len(got), len(data))
	}
}

// Un archivo guardado en el bucket no se puede servir si el tenant borra la
// configuración: se avisa en vez de devolver bytes de otro lado.
func TestBucketFileWithoutConfigFailsLoudly(t *testing.T) {
	store, _ := newTestStore(t)
	fake := newFakeS3(t, "acme-media")
	svc := NewService(store, "", "acme", testEncKey, 10<<20, true)
	if _, err := svc.SaveStorageConfig(StorageConfigRequest{
		Endpoint: fake.endpoint, Bucket: fake.bucket, AccessKey: "ak", SecretKey: "sk",
	}); err != nil {
		t.Fatalf("SaveStorageConfig: %v", err)
	}
	res, err := svc.Upload(testPNG(t, 16, 16), "x.png", "images", "persistent", false)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if err := svc.DeleteStorageConfig(); err != nil {
		t.Fatalf("DeleteStorageConfig: %v", err)
	}
	if _, _, err := svc.ServeTarget(res.ID); err == nil {
		t.Fatal("se esperaba error al servir un archivo de bucket sin configuración")
	}
}

// El backend local sigue siendo el default: sin configuración de bucket no se
// toca la red.
func TestUploadBackendSelection(t *testing.T) {
	store, _ := newTestStore(t)
	svc := NewService(store, "", "acme", testEncKey, 10<<20, true)

	b, err := svc.uploadBackend()
	if err != nil {
		t.Fatalf("uploadBackend: %v", err)
	}
	if b.Name() != "local" {
		t.Errorf("backend sin bucket = %q, want local", b.Name())
	}

	p, err := storage.New(storage.Config{
		Endpoint: "http://127.0.0.1:9000", Bucket: "b", AccessKey: "ak", SecretKey: "sk",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.presignerFn = func() (*storage.Presigner, error) { return p, nil }
	b, err = svc.uploadBackend()
	if err != nil {
		t.Fatalf("uploadBackend: %v", err)
	}
	if b.Name() != "s3" {
		t.Errorf("backend con bucket = %q, want s3", b.Name())
	}
	if _, err := svc.backend("s3"); err != nil {
		t.Errorf("backend(s3) = %v", err)
	}
}
