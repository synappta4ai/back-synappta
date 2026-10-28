package file

import (
	"bytes"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"github.com/disintegration/imaging"
)

// Un .mp4 generado por la agencia tiene que guardarse como video/mp4, no como
// application/octet-stream: el front decide el tipo por el MIME y con
// octet-stream pintaba un ícono de documento y el contador de videos daba 0.
func TestUploadDerivesVideoMime(t *testing.T) {
	store, _ := newTestStore(t)
	fake := newFakeS3(t, "acme-media")
	svc := NewService(store, "", "acme", testEncKey, 10<<20, true)
	if _, err := svc.SaveStorageConfig(StorageConfigRequest{
		Endpoint: fake.endpoint, Bucket: fake.bucket, AccessKey: "ak", SecretKey: "sk",
	}); err != nil {
		t.Fatalf("SaveStorageConfig: %v", err)
	}
	t.Cleanup(func() { _ = svc.DeleteStorageConfig() })

	clip := append([]byte("\x00\x00\x00\x18ftypmp42"), bytes.Repeat([]byte{0}, 64)...)
	res, err := svc.Upload(clip, "clip.mp4", "videos", "persistent", false)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	stored, err := svc.GetFile(res.ID)
	if err != nil || stored == nil {
		t.Fatalf("GetFile: %v", err)
	}
	if stored.MimeType != "video/mp4" {
		t.Errorf("mime guardado = %q, want video/mp4", stored.MimeType)
	}

	fake.mu.Lock()
	obj := fake.objects[stored.Path]
	fake.mu.Unlock()
	if obj.contentType != "video/mp4" {
		t.Errorf("Content-Type del objeto = %q, want video/mp4", obj.contentType)
	}

	if got := svc.DecorateOne(stored); !strings.HasPrefix(got.MimeType, "video/") {
		t.Errorf("la respuesta sale como %q, el front no la vería como video", got.MimeType)
	}
}

// Un .webp necesita el decodificador de golang.org/x/image: sin él la miniatura
// fallaba y el API respondía 500, así que la tarjeta quedaba con el ícono roto.
func TestThumbnailTargetGeneratesForWebP(t *testing.T) {
	store, _ := newTestStore(t)
	svc := NewService(store, "", "acme", testEncKey, 10<<20, true)

	data, err := base64.StdEncoding.DecodeString(webp8x8)
	if err != nil {
		t.Fatalf("fixture webp: %v", err)
	}
	res, err := svc.Upload(data, "cartel.webp", "images", "persistent", false)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	local, redirect, err := svc.ThumbnailTarget(res.ID)
	if err != nil {
		t.Fatalf("ThumbnailTarget: %v", err)
	}
	if local == "" || redirect != "" {
		t.Fatalf("miniatura local = (%q, %q)", local, redirect)
	}
	thumb, err := os.ReadFile(local)
	if err != nil {
		t.Fatalf("leer miniatura: %v", err)
	}
	if _, err := imaging.Decode(bytes.NewReader(thumb)); err != nil {
		t.Fatalf("la miniatura guardada no es una imagen válida: %v", err)
	}
}
