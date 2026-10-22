package agency

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"synapta/config"
)

// recordingPublisher simulates the tenant storage: it records what it received
// and decides whether the upload succeeded.
type recordingPublisher struct {
	stored bool
	errURL string
	got    map[string][]byte
	types  map[string]string
}

func (p *recordingPublisher) StoreOutput(relPath string, data []byte, contentType string) (string, bool) {
	if p.got == nil {
		p.got = map[string][]byte{}
		p.types = map[string]string{}
	}
	p.got[relPath] = data
	p.types[relPath] = contentType
	if !p.stored {
		return "", false
	}
	return p.errURL + relPath, true
}

// newOutputCore builds a Core whose only configured piece is the outputs dir.
func newOutputCore(t *testing.T, publisher OutputPublisher) (*Core, string) {
	t.Helper()
	dir := t.TempDir()
	core := NewCore(&config.Config{OutputsDir: dir}, nil, nil, "http://api.test", "acme")
	core.SetOutputPublisher(publisher)
	return core, dir
}

func writeOutput(t *testing.T, dir, rel, body string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestPublishLocalOutputsMovesTheArtifactToTheBucket(t *testing.T) {
	pub := &recordingPublisher{stored: true, errURL: "http://api.test/api/v1/t/acme/outputs/"}
	core, dir := newOutputCore(t, pub)
	writeOutput(t, dir, "video/task_1.mp4", "video-bytes")

	result := &GeneratorResult{
		TaskID:  "task_1",
		Outputs: []OutputResource{{URL: "/outputs/video/task_1.mp4", LocalURL: "/outputs/video/task_1.mp4", Type: "video"}},
	}
	core.publishLocalOutputs(result)

	out := result.Outputs[0]
	if out.URL != "http://api.test/api/v1/t/acme/outputs/video/task_1.mp4" {
		t.Errorf("url = %q", out.URL)
	}
	if out.LocalURL != "" {
		t.Errorf("localUrl = %q, se esperaba vacía para que el front use la del bucket", out.LocalURL)
	}
	if pub.types["video/task_1.mp4"] != "video/mp4" {
		t.Errorf("content type = %q", pub.types["video/task_1.mp4"])
	}
	if _, err := os.Stat(filepath.Join(dir, "video/task_1.mp4")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("la copia local debería borrarse tras subirla: %v", err)
	}
}

// Sin bucket (o si falla la subida) todo queda igual que antes.
func TestPublishLocalOutputsKeepsLocalWhenStorageRefuses(t *testing.T) {
	pub := &recordingPublisher{stored: false}
	core, dir := newOutputCore(t, pub)
	writeOutput(t, dir, "image/gemini_1.png", "png-bytes")

	result := &GeneratorResult{
		TaskID:  "task_2",
		Outputs: []OutputResource{{URL: "/outputs/image/gemini_1.png", Type: "image"}},
	}
	core.publishLocalOutputs(result)

	if result.Outputs[0].URL != "/outputs/image/gemini_1.png" {
		t.Errorf("url = %q, se esperaba la local", result.Outputs[0].URL)
	}
	if _, err := os.Stat(filepath.Join(dir, "image/gemini_1.png")); err != nil {
		t.Errorf("el archivo local debería seguir ahí: %v", err)
	}
}

// Los modelos que dejan la URL firmada del proveedor (sin copia local) no se
// tocan: no hay bytes propios que mover.
func TestPublishLocalOutputsIgnoresProviderURLs(t *testing.T) {
	pub := &recordingPublisher{stored: true}
	core, _ := newOutputCore(t, pub)

	result := &GeneratorResult{
		TaskID:  "task_3",
		Outputs: []OutputResource{{URL: "https://provider.example.com/v/1.mp4", Type: "video"}},
	}
	core.publishLocalOutputs(result)

	if result.Outputs[0].URL != "https://provider.example.com/v/1.mp4" {
		t.Errorf("url = %q", result.Outputs[0].URL)
	}
	if len(pub.got) != 0 {
		t.Errorf("no debía publicar nada; recibió %v", pub.got)
	}
}

// Un output ya publicado (local borrado) se saltea en el siguiente poll.
func TestPublishLocalOutputsIsIdempotent(t *testing.T) {
	pub := &recordingPublisher{stored: true, errURL: "http://api.test/api/v1/t/acme/outputs/"}
	core, dir := newOutputCore(t, pub)
	writeOutput(t, dir, "video/task_4.mp4", "video-bytes")

	result := &GeneratorResult{
		TaskID:  "task_4",
		Outputs: []OutputResource{{URL: "/outputs/video/task_4.mp4", LocalURL: "/outputs/video/task_4.mp4", Type: "video"}},
	}
	core.publishLocalOutputs(result)
	first := result.Outputs[0].URL

	// El poll siguiente reusa el mismo OutputResource ya reescrito.
	core.publishLocalOutputs(result)
	if result.Outputs[0].URL != first {
		t.Errorf("url cambió en el segundo poll: %q -> %q", first, result.Outputs[0].URL)
	}
}
