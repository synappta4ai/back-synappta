package file

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"synapta/internal/storage"
)

// testService wires a service whose tenant bucket is a fixed presigner (the
// production path reads storage_config from the tenant schema).
func testService(t *testing.T, maxUpload int64) *Service {
	t.Helper()
	return testServiceWith(t, testPresigner(t), maxUpload)
}

func testPresigner(t *testing.T) *storage.Presigner {
	t.Helper()
	p, err := storage.New(storage.Config{
		Endpoint:  "https://bucket.example.com",
		Bucket:    "media",
		AccessKey: "AK",
		SecretKey: "SK",
		Region:    "us-east-1",
	})
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	return p
}

func testServiceWith(t *testing.T, p *storage.Presigner, maxUpload int64) *Service {
	t.Helper()
	svc := NewService(nil, "", "acme", "", maxUpload, true)
	svc.presignerFn = func() (*storage.Presigner, error) { return p, nil }
	return svc
}

func TestPresignUploadBuildsTenantScopedKey(t *testing.T) {
	t.Parallel()

	svc := testService(t, 10<<20)
	res, err := svc.PresignUpload(PresignRequest{
		Filename:    "Foto Vacaciones.PNG",
		ContentType: "image/png; charset=utf-8", // parameters are stripped
		Size:        1234,
	})
	if err != nil {
		t.Fatalf("PresignUpload: %v", err)
	}

	// <tenant>/<category>/<year>/<month>/<uuid>.<ext>
	keyRe := regexp.MustCompile(`^acme/images/\d{4}/\d{2}/[0-9a-f-]{36}\.png$`)
	if !keyRe.MatchString(res.Key) {
		t.Errorf("key = %q, no matchea %s", res.Key, keyRe)
	}
	if res.Method != "PUT" {
		t.Errorf("method = %q, want PUT", res.Method)
	}
	if got := res.Headers["Content-Type"]; got != "image/png" {
		t.Errorf("content-type firmado = %q, want image/png", got)
	}
	if res.Bucket != "media" {
		t.Errorf("bucket = %q", res.Bucket)
	}
	if res.ExpiresIn != 900 {
		t.Errorf("expires_in = %d, want 900", res.ExpiresIn)
	}
	if res.MaxSize != 10<<20 {
		t.Errorf("max_size = %d", res.MaxSize)
	}

	u, err := url.Parse(res.UploadURL)
	if err != nil {
		t.Fatalf("upload_url no parsea: %v", err)
	}
	if u.Host != "bucket.example.com" {
		t.Errorf("host = %q", u.Host)
	}
	if want := "/media/" + res.Key; u.Path != want {
		t.Errorf("path = %q, want %q", u.Path, want)
	}
	if u.Query().Get("X-Amz-Signature") == "" {
		t.Error("upload_url sin firma")
	}
	if !strings.HasPrefix(res.PublicURL, "https://bucket.example.com/media/acme/images/") {
		t.Errorf("public_url = %q", res.PublicURL)
	}
}

func TestPresignUploadDerivesContentType(t *testing.T) {
	t.Parallel()

	svc := testService(t, 0)
	res, err := svc.PresignUpload(PresignRequest{Filename: "clip.mov"})
	if err != nil {
		t.Fatalf("PresignUpload: %v", err)
	}
	if !strings.HasPrefix(res.Headers["Content-Type"], "video/") {
		t.Errorf("content-type derivado = %q", res.Headers["Content-Type"])
	}
	if !strings.HasPrefix(res.Key, "acme/videos/") {
		t.Errorf("key = %q, want prefijo acme/videos/", res.Key)
	}
}

func TestPresignUploadRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	svc := testService(t, 1000)
	cases := []struct {
		name string
		req  PresignRequest
		want string
	}{
		{"sin filename", PresignRequest{}, "filename is required"},
		{"sin extensión", PresignRequest{Filename: "foto"}, "unsupported file type"},
		{"html no permitido", PresignRequest{Filename: "x.html", ContentType: "text/html"}, "unsupported file type"},
		{"svg no permitido", PresignRequest{Filename: "logo.svg", ContentType: "image/svg+xml"}, "unsupported file type"},
		{"mime no coincide", PresignRequest{Filename: "x.png", ContentType: "application/pdf"}, "does not match extension"},
		{"tipo mime incoherente", PresignRequest{Filename: "x.mp4", ContentType: "image/png"}, "does not match extension"},
		{"size negativa", PresignRequest{Filename: "x.png", Size: -1}, "size must be >= 0"},
		{"size excedida", PresignRequest{Filename: "x.png", Size: 2000}, "file too large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := svc.PresignUpload(tc.req)
			if err == nil {
				t.Fatalf("se aceptó un request inválido: %+v", tc.req)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want contiene %q", err, tc.want)
			}
		})
	}
}

func TestPresignUploadIgnoresPathTraversal(t *testing.T) {
	t.Parallel()

	svc := testService(t, 0)
	res, err := svc.PresignUpload(PresignRequest{Filename: "../../etc/passwd.png"})
	if err != nil {
		t.Fatalf("PresignUpload: %v", err)
	}
	if strings.Contains(res.Key, "..") {
		t.Errorf("key conserva traversal: %q", res.Key)
	}
	if !strings.HasPrefix(res.Key, "acme/images/") {
		t.Errorf("key = %q", res.Key)
	}
}

func TestPresignUploadWithoutStorage(t *testing.T) {
	t.Parallel()

	// Sin bucket configurado el tenant guarda en local: presign responde 503.
	svc := testServiceWith(t, nil, 0)
	_, err := svc.PresignUpload(PresignRequest{Filename: "x.png"})
	if !errors.Is(err, ErrStorageDisabled) {
		t.Fatalf("err = %v, want ErrStorageDisabled", err)
	}
}
