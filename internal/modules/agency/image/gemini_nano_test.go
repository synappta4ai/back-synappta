package agencyimage

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/jpeg"
	"image/png"
	"math/rand"
	"testing"

	"synapta/internal/modules/agency"
)

// noisyPNG returns a PNG that does not compress well on purpose, so it
// reproduces a full-size photo/reference from the library.
func noisyPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	rnd := rand.New(rand.NewSource(7))
	for i := range img.Pix {
		img.Pix[i] = byte(rnd.Intn(256))
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func dataURL(raw []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
}

func inlineParts(t *testing.T, payload map[string]interface{}) []map[string]interface{} {
	t.Helper()
	contents, ok := payload["contents"].([]map[string]interface{})
	if !ok || len(contents) == 0 {
		t.Fatalf("payload has no contents: %#v", payload)
	}
	parts, ok := contents[0]["parts"].([]map[string]interface{})
	if !ok {
		t.Fatalf("payload has no parts: %#v", contents[0])
	}
	images := make([]map[string]interface{}, 0, len(parts))
	for _, part := range parts {
		if inline, ok := part["inline_data"].(map[string]interface{}); ok {
			images = append(images, inline)
		}
	}
	return images
}

func TestGeminiMatchRejectsHostedInferenceNames(t *testing.T) {
	nano := NewGeminiNanoGenerator()
	pro := NewGeminiProGenerator()

	hosted := []string{
		"openrouter/google/gemini-2.5-flash-image",
		"openrouter/google/gemini-3-pro-image-preview",
		"together/google/gemini-3-pro-image-preview",
	}
	for _, name := range hosted {
		if nano.Match(name) || pro.Match(name) {
			t.Errorf("Match(%q) = true, want false (hosted models use the OpenAI-compatible generator)", name)
		}
	}

	direct := map[string]bool{
		"gemini-nano-banana":             true,
		"gemini-3.1-flash-image-preview": true,
		"gemini-3-pro-image-preview":     false,
	}
	for name, expectedNano := range direct {
		if got := nano.Match(name); got != expectedNano {
			t.Errorf("nano.Match(%q) = %v, want %v", name, got, expectedNano)
		}
	}
	if !pro.Match("gemini-3-pro-image-preview") {
		t.Errorf("pro.Match(gemini-3-pro-image-preview) = false, want true")
	}
}

func TestGeminiBuildPayloadKeepsSmallReferenceUntouched(t *testing.T) {
	raw := noisyPNG(t, 64, 64)
	gen := NewGeminiNanoGenerator()

	payload := gen.BuildPayload(&agency.GeneratorRequest{
		Content: []agency.ContentItem{
			{Type: "text", Text: "story"},
			{Type: "image", DataURL: dataURL(raw)},
		},
	})

	images := inlineParts(t, payload)
	if len(images) != 1 {
		t.Fatalf("inline images = %d, want 1", len(images))
	}
	if got := images[0]["data"].(string); got != base64.StdEncoding.EncodeToString(raw) {
		t.Errorf("small reference was re-encoded; want byte-identical base64")
	}
	if got := images[0]["mime_type"]; got != "image/png" {
		t.Errorf("mime_type = %v, want image/png", got)
	}
}

func TestGeminiBuildPayloadCompactsOversizedReference(t *testing.T) {
	raw := noisyPNG(t, 3000, 3000)
	if base64.StdEncoding.EncodedLen(len(raw)) <= geminiInlineBudget {
		t.Fatalf("test fixture is too small: %d base64 bytes", base64.StdEncoding.EncodedLen(len(raw)))
	}
	gen := NewGeminiNanoGenerator()

	payload := gen.BuildPayload(&agency.GeneratorRequest{
		Content: []agency.ContentItem{
			{Type: "text", Text: "storyboard"},
			{Type: "image", DataURL: dataURL(raw)},
		},
	})

	images := inlineParts(t, payload)
	if len(images) != 1 {
		t.Fatalf("inline images = %d, want 1", len(images))
	}
	encoded := images[0]["data"].(string)
	if len(encoded) > geminiInlineBudget {
		t.Errorf("inline base64 = %d bytes, want <= %d", len(encoded), geminiInlineBudget)
	}
	if got := images[0]["mime_type"]; got != "image/jpeg" {
		t.Errorf("mime_type = %v, want image/jpeg", got)
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode compacted image: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(decoded))
	if err != nil {
		t.Fatalf("decode compacted image: %v", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() > geminiInlineMaxSide || bounds.Dy() > geminiInlineMaxSide {
		t.Errorf("compacted image is %dx%d, want longest side <= %d", bounds.Dx(), bounds.Dy(), geminiInlineMaxSide)
	}
}

func TestGeminiBuildPayloadSharesBudgetAcrossReferences(t *testing.T) {
	gen := NewGeminiNanoGenerator()
	content := []agency.ContentItem{{Type: "text", Text: "storyboard"}}
	for i := 0; i < 4; i++ {
		content = append(content, agency.ContentItem{Type: "image", DataURL: dataURL(noisyPNG(t, 1200, 1200))})
	}

	payload := gen.BuildPayload(&agency.GeneratorRequest{Content: content})
	images := inlineParts(t, payload)
	if len(images) != 4 {
		t.Fatalf("inline images = %d, want 4", len(images))
	}

	perImage := geminiInlineBudget / 4
	total := 0
	for i, inline := range images {
		size := len(inline["data"].(string))
		total += size
		if size > perImage {
			t.Errorf("image %d base64 = %d bytes, want <= %d", i, size, perImage)
		}
	}
	if total > geminiInlineBudget {
		t.Errorf("total inline base64 = %d bytes, want <= %d", total, geminiInlineBudget)
	}
}
