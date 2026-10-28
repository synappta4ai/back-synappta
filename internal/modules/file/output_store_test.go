package file

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"synapta/internal/storage"
)

// outputService builds a Service that points at the fake bucket without a DB:
// output storage only needs the presigner and the public URL parts.
func outputService(t *testing.T, publicBase string) (*Service, *fakeS3) {
	t.Helper()
	fake := newFakeS3(t, "media")
	p, err := storage.New(storage.Config{
		Endpoint:   fake.endpoint,
		Bucket:     "media",
		AccessKey:  "ak",
		SecretKey:  "sk",
		PublicBase: publicBase,
	})
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	return &Service{
		baseURL:    "http://api.test",
		tenantSlug: "acme",
		presignerFn: func() (*storage.Presigner, error) {
			return p, nil
		},
	}, fake
}

// noBucketService is a tenant with no storage configuration: the presigner
// lookup answers nil, which is the "keep everything on local disk" case.
func noBucketService() *Service {
	return &Service{
		store:      &Store{},
		baseURL:    "http://api.test",
		tenantSlug: "acme",
		presignerFn: func() (*storage.Presigner, error) {
			return nil, nil
		},
	}
}

func TestStoreOutputUploadsToTheTenantBucket(t *testing.T) {
	svc, fake := outputService(t, "")

	url, stored := svc.StoreOutput("image/task_1.png", []byte("png-bytes"), "image/png")
	if !stored {
		t.Fatal("StoreOutput: stored = false, se esperaba true con bucket configurado")
	}
	if url != "http://api.test/api/v1/t/acme/outputs/image/task_1.png" {
		t.Errorf("url = %q", url)
	}
	fake.mu.Lock()
	obj, ok := fake.objects["outputs/image/task_1.png"]
	fake.mu.Unlock()
	if !ok {
		t.Fatalf("el objeto no se subió al bucket; keys=%v", fake.keys())
	}
	if string(obj.data) != "png-bytes" {
		t.Errorf("bytes subidos = %q", obj.data)
	}
	if obj.contentType != "image/png" {
		t.Errorf("content type = %q", obj.contentType)
	}
}

// Sin bucket el contrato es "no almacenado": el llamador deja el archivo en
// disco, igual que las subidas.
func TestStoreOutputWithoutBucketKeepsLocalDisk(t *testing.T) {
	svc := noBucketService()

	url, stored := svc.StoreOutput("image/task_1.png", []byte("png-bytes"), "image/png")
	if stored {
		t.Fatal("StoreOutput: stored = true sin bucket configurado")
	}
	if url != "" {
		t.Errorf("url = %q, se esperaba vacía", url)
	}
}

func TestStoreOutputRejectsUnsafePaths(t *testing.T) {
	svc, _ := outputService(t, "")

	for _, rel := range []string{"", "/abs.png", "../escape.png", "image//x.png", `image\x.png`} {
		if _, stored := svc.StoreOutput(rel, []byte("x"), "image/png"); stored {
			t.Errorf("StoreOutput(%q) subió un path inseguro", rel)
		}
	}
}

// La ruta pública recibe el path del wildcard con "/" adelante y debe firmar
// la key del bucket, no servir bytes desde la API.
func TestServeOutputRedirectsToTheSignedURL(t *testing.T) {
	svc, fake := outputService(t, "")
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/t/:slug/outputs/*file", NewHandler(svc).ServeOutput)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/t/acme/outputs/video/task_1.mp4", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, se esperaba 302", rec.Code)
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, fake.endpoint+"/media/outputs/video/task_1.mp4?") {
		t.Errorf("location = %q", location)
	}
}

func TestOutputTargetPrefersThePublicBase(t *testing.T) {
	svc, _ := outputService(t, "https://cdn.example.com")

	url, err := svc.OutputTarget("video/task_1.mp4")
	if err != nil {
		t.Fatalf("OutputTarget: %v", err)
	}
	if url != "https://cdn.example.com/outputs/video/task_1.mp4" {
		t.Errorf("url = %q", url)
	}
}

func TestOutputTargetSignsWhenThereIsNoPublicBase(t *testing.T) {
	svc, fake := outputService(t, "")

	url, err := svc.OutputTarget("video/task_1.mp4")
	if err != nil {
		t.Fatalf("OutputTarget: %v", err)
	}
	if !strings.HasPrefix(url, fake.endpoint+"/media/outputs/video/task_1.mp4?") {
		t.Errorf("url = %q", url)
	}
	if !strings.Contains(url, "X-Amz-Signature=") {
		t.Errorf("la URL no está firmada: %q", url)
	}
}

func TestOutputTargetWithoutBucketFails(t *testing.T) {
	svc := noBucketService()

	if _, err := svc.OutputTarget("image/task_1.png"); err == nil {
		t.Fatal("se esperaba error sin bucket configurado")
	}
}
