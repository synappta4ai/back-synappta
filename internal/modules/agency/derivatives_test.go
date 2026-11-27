package agency

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"synapta/config"
)

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestEnsureOutputDerivativesKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	rel := "image/task_1.png"
	original := testPNG(t, 900, 600)
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}

	core := &Core{outputsDir: dir, baseURL: "http://api.test"}
	result := &GeneratorResult{Outputs: []OutputResource{{
		URL:      config.OutPutUrl() + "/" + rel,
		LocalURL: config.OutPutUrl() + "/" + rel,
		Type:     "image",
	}}}
	core.ensureOutputDerivatives(result)

	out := result.Outputs[0]
	if out.ThumbnailURL != "/outputs/thumbnails/image/task_1.webp" {
		t.Errorf("thumbnailUrl = %q", out.ThumbnailURL)
	}
	if out.PreviewURL != "/outputs/previews/image/task_1.webp" {
		t.Errorf("previewUrl = %q", out.PreviewURL)
	}
	for _, relPath := range []string{"thumbnails/image/task_1.webp", "previews/image/task_1.webp"} {
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(relPath)))
		if err != nil {
			t.Fatalf("%s no existe: %v", relPath, err)
		}
		if info.Size() == 0 {
			t.Errorf("%s vacío", relPath)
		}
		if info.Size() >= int64(len(original)) {
			t.Errorf("%s (%d bytes) no es más chico que el original (%d)", relPath, info.Size(), len(original))
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("el original cambió: la generación no debe tocarse")
	}
}

func TestDecorateOutputUsesExistingDerivatives(t *testing.T) {
	dir := t.TempDir()
	rel := "image/task_2.png"
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, testPNG(t, 64, 64), 0644); err != nil {
		t.Fatal(err)
	}
	core := &Core{outputsDir: dir, baseURL: "http://api.test"}

	// Sin derivados todavía: no inventa URLs.
	out := OutputResource{LocalURL: config.OutPutUrl() + "/" + rel, Type: "image"}
	core.decorateOutput(&out)
	if out.ThumbnailURL != "" || out.PreviewURL != "" {
		t.Fatalf("decorate sin archivos = %+v", out)
	}

	core.ensureOutputDerivatives(&GeneratorResult{Outputs: []OutputResource{out}})
	out = OutputResource{LocalURL: config.OutPutUrl() + "/" + rel, Type: "image"}
	core.decorateOutput(&out)
	if out.ThumbnailURL != "http://api.test/outputs/thumbnails/image/task_2.webp" {
		t.Errorf("thumbnailUrl = %q", out.ThumbnailURL)
	}
	if out.PreviewURL != "http://api.test/outputs/previews/image/task_2.webp" {
		t.Errorf("previewUrl = %q", out.PreviewURL)
	}
}
