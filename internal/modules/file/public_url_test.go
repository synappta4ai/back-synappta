package file

import (
	"strings"
	"testing"
)

// Con public_base configurado, los assets del bucket se leen directo desde el
// bucket/CDN: cero tráfico de API por vista y sin firmas que caducan.
func TestDecorateUsesPublicBaseForBucketFiles(t *testing.T) {
	const base = "https://cdn.example.com/acme"
	svc := testServiceWith(t, presignerWithBase(t, "https://bucket.example.com", "media", base), 10<<20)

	files := svc.decorate([]File{
		{ID: "id-1", Path: "images/a.png", Backend: "s3", MimeType: "image/png"},
		{ID: "id-2", Path: "videos/b.mp4", Backend: "s3", MimeType: "video/mp4"},
	})

	if want := base + "/images/a.png"; files[0].URL != want {
		t.Errorf("url = %q, want %q", files[0].URL, want)
	}
	if want := base + "/thumbnails/images/a.png"; files[0].ThumbnailURL != want {
		t.Errorf("thumbnail_url = %q, want %q", files[0].ThumbnailURL, want)
	}
	if want := base + "/videos/b.mp4"; files[1].URL != want {
		t.Errorf("url video = %q, want %q", files[1].URL, want)
	}
	if files[1].ThumbnailURL != "" {
		t.Errorf("un video no lleva miniatura: %q", files[1].ThumbnailURL)
	}
}

// Sin public_base (o para archivos en disco) la URL sigue siendo la del API,
// que sirve los bytes o redirige a una firma fresca.
func TestDecorateFallsBackToAPIURLs(t *testing.T) {
	signed := testServiceWith(t, presignerFor(t, "https://bucket.example.com", "media"), 10<<20)
	files := signed.decorate([]File{
		{ID: "id-1", Path: "images/a.png", Backend: "s3", MimeType: "image/png"},
	})
	if want := "/api/v1/t/acme/files/id-1/serve"; files[0].URL != want {
		t.Errorf("url sin public_base = %q, want %q", files[0].URL, want)
	}
	if want := "/api/v1/t/acme/files/id-1/thumbnail"; files[0].ThumbnailURL != want {
		t.Errorf("thumbnail_url sin public_base = %q, want %q", files[0].ThumbnailURL, want)
	}

	public := testServiceWith(t, presignerWithBase(t, "https://bucket.example.com", "media", "https://cdn.example.com/acme"), 10<<20)
	local := public.decorate([]File{
		{ID: "id-2", Path: "images/c.png", Backend: "local", MimeType: "image/png"},
	})
	if want := "/api/v1/t/acme/files/id-2/serve"; local[0].URL != want {
		t.Errorf("un archivo local no puede salir del disco vía bucket: %q", local[0].URL)
	}
}

// DecorateOne (GET /files/:id) tiene que coincidir con el listado.
func TestDecorateOneMatchesListURLs(t *testing.T) {
	svc := testServiceWith(t, presignerWithBase(t, "https://bucket.example.com", "media", "https://cdn.example.com/acme"), 10<<20)
	f := svc.DecorateOne(&File{ID: "id-1", Path: "images/a.png", Backend: "s3", MimeType: "image/png"})
	if !strings.HasPrefix(f.URL, "https://cdn.example.com/acme/images/") {
		t.Fatalf("DecorateOne url = %q", f.URL)
	}
	if !strings.HasPrefix(f.ThumbnailURL, "https://cdn.example.com/acme/thumbnails/images/") {
		t.Fatalf("DecorateOne thumbnail_url = %q", f.ThumbnailURL)
	}
}
