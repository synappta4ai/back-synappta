package file

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// testPNG builds a small valid PNG in memory (no fixtures on disk).
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 255), G: uint8(y % 255), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Un tenant sin bucket sigue guardando en disco, exactamente como antes.
func TestUploadWithoutBucketStoresLocally(t *testing.T) {
	store, db := newTestStore(t)
	svc := NewService(store, "", "acme", testEncKey, 10<<20, true)

	data := testPNG(t, 40, 30)
	res, err := svc.Upload(data, "foto.png", "images", "persistent", false)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	stored, err := svc.GetFile(res.ID)
	if err != nil || stored == nil {
		t.Fatalf("GetFile: %v", err)
	}
	if stored.Backend != "local" {
		t.Errorf("backend = %q, want local", stored.Backend)
	}
	if _, err := os.Stat(filepath.Join(store.uploadDir, stored.Path)); err != nil {
		t.Errorf("el archivo no está en disco: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.uploadDir, thumbnailPath(stored.Path))); err != nil {
		t.Errorf("la miniatura no está en disco: %v", err)
	}

	local, redirect, err := svc.ServeTarget(res.ID)
	if err != nil || redirect != "" || local == "" {
		t.Fatalf("ServeTarget local = (%q, %q, %v)", local, redirect, err)
	}

	if err := svc.HardDelete(res.ID); err != nil {
		t.Fatalf("HardDelete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.uploadDir, stored.Path)); !os.IsNotExist(err) {
		t.Error("el archivo debería haberse borrado del disco")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE id = $1`, res.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("la fila debería haberse borrado")
	}
}

// MAX_UPLOAD_BYTES se respeta en el mismo upload para los dos backends.
func TestUploadRejectsOversized(t *testing.T) {
	store, _ := newTestStore(t)
	svc := NewService(store, "", "acme", testEncKey, 64, true)
	if _, err := svc.Upload(testPNG(t, 128, 128), "grande.png", "images", "persistent", false); err == nil {
		t.Fatal("se esperaba error por tamaño")
	}
}

// La categoría llega del cliente y se usa como segmento de path/key: se
// rechaza cualquier cosa que intente salirse de UPLOAD_DIR.
func TestUploadRejectsUnsafeCategory(t *testing.T) {
	store, _ := newTestStore(t)
	svc := NewService(store, "", "acme", testEncKey, 10<<20, true)
	data := testPNG(t, 8, 8)

	for _, category := range []string{"../escape", "images/../../x", "/abs", "a b", ""} {
		if _, err := svc.Upload(data, "x.png", category, "persistent", true); err == nil {
			t.Errorf("categoría %q debería rechazarse", category)
		}
	}

	// Las categorías reales del front (incluidos los slots tipados) siguen OK.
	for _, category := range []string{"images", "character", "props", "location", "temp"} {
		if _, err := svc.Upload(data, category+".png", category, "temp", true); err != nil {
			t.Errorf("categoría %q rechazada: %v", category, err)
		}
	}
}

// makeThumbnail produce un PNG cuando el original es PNG y un JPEG si no.
func TestMakeThumbnail(t *testing.T) {
	t.Parallel()

	data := testPNG(t, 200, 150)
	thumb, ct, err := makeThumbnail(data, ".png")
	if err != nil {
		t.Fatalf("makeThumbnail: %v", err)
	}
	if ct != "image/png" {
		t.Errorf("content-type = %q", ct)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(thumb))
	if err != nil {
		t.Fatalf("miniatura no es PNG: %v", err)
	}
	if cfg.Width > 300 || cfg.Height > 300 {
		t.Errorf("miniatura %dx%d excede 300x300", cfg.Width, cfg.Height)
	}

	// Extensión no soportada por imagem: error, no pánico.
	if _, _, err := makeThumbnail([]byte("no soy una imagen"), ".png"); err == nil {
		t.Error("se esperaba error con bytes inválidos")
	}
}

// El backend local cumple el contrato (Put/Open/Exists/Delete).
func TestLocalBackendContract(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	b := localBackend{uploadDir: dir}
	ctx := context.Background()

	if err := b.Put(ctx, "images/a.png", []byte("hola"), "image/png"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ok, err := b.Exists(ctx, "images/a.png"); err != nil || !ok {
		t.Fatalf("Exists = (%v, %v)", ok, err)
	}
	rc, err := b.Open(ctx, "images/a.png")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "hola" {
		t.Errorf("contenido = %q", data)
	}
	if err := b.Delete(ctx, "images/a.png"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := b.Exists(ctx, "images/a.png"); ok {
		t.Error("el archivo debería haberse borrado")
	}
	if _, err := b.Open(ctx, "images/a.png"); err != ErrBlobNotFound {
		t.Errorf("Open tras Delete = %v, want ErrBlobNotFound", err)
	}
	if err := b.Delete(ctx, "images/no-existe.png"); err != nil {
		t.Errorf("Delete idempotente = %v", err)
	}
}
