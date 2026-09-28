package agencyimage

import (
	"testing"

	"synapta/internal/modules/agency"
)

func TestHiggsfieldImageMatch(t *testing.T) {
	g := NewHiggsfieldImageGenerator()
	cases := map[string]bool{
		"higgsfield-soul-2-standard":  true,
		"higgsfield-ideogram-4-0":     true,
		"higgsfield-recraft-4-1":      true,
		"higgsfield-kling-3-0-turbo":  false, // video generator's
		"dreamina-seedream-4-pro":     false,
		"gemini-3.1-flash-image":      false,
	}
	for name, want := range cases {
		if got := g.Match(name); got != want {
			t.Errorf("Match(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestHiggsfieldImageBuildPayloadSoul(t *testing.T) {
	g := NewHiggsfieldImageGenerator()
	req := &agency.GeneratorRequest{
		Model:      "higgsfield-soul-2-standard",
		Content:    []agency.ContentItem{{Type: "text", Text: "retrato editorial"}},
		Ratio:      "3:4",
		Resolution: "2K",
	}
	p := g.BuildPayload(req)
	if p["prompt"] == "" {
		t.Fatal("prompt missing")
	}
	if p["aspect_ratio"] != "3:4" || p["resolution"] != "2K" {
		t.Fatalf("soul params: %v", p)
	}
}

func TestHiggsfieldImageBuildPayloadIdeogramEdit(t *testing.T) {
	g := NewHiggsfieldImageGenerator()
	req := &agency.GeneratorRequest{
		Model:   "higgsfield-ideogram-4-0",
		Content: []agency.ContentItem{{Type: "text", Text: "remix"}, {Type: "image", DataURL: "https://example.com/in.jpg"}},
		Ratio:   "1:1",
	}
	p := g.BuildPayload(req)
	if p["image_url"] != "https://example.com/in.jpg" {
		t.Fatalf("ideogram image_url: %v", p["image_url"])
	}
	if _, has := p["resolution"]; has {
		t.Fatal("ideogram has no resolution tier")
	}

	// Text-only request must omit image_url (strict schema).
	req2 := &agency.GeneratorRequest{
		Model:   "higgsfield-ideogram-4-0",
		Content: []agency.ContentItem{{Type: "text", Text: "poster limpio"}},
	}
	p2 := g.BuildPayload(req2)
	if _, has := p2["image_url"]; has {
		t.Fatal("t2i ideogram must not send image_url")
	}
}

func TestHiggsfieldImageBuildPayloadRecraft(t *testing.T) {
	g := NewHiggsfieldImageGenerator()
	req := &agency.GeneratorRequest{
		Model:      "higgsfield-recraft-4-1",
		Content:    []agency.ContentItem{{Type: "text", Text: "logo vectorial"}},
		Resolution: "1K",
	}
	p := g.BuildPayload(req)
	if p["resolution"] != "1k" {
		t.Fatalf("recraft lowercase tier: %v", p["resolution"])
	}
	if _, has := p["image_url"]; has {
		t.Fatal("recraft is t2i only: no image input")
	}
}

func TestHiggsfieldImageValidate(t *testing.T) {
	g := NewHiggsfieldImageGenerator()
	req := &agency.GeneratorRequest{
		Model:      "higgsfield-soul-2-standard",
		Content:    []agency.ContentItem{{Type: "text", Text: "x"}},
		Resolution: "720p", // video-tier value, invalid for images
		Duration:   5,
	}
	if err := g.Validate(req); err == nil {
		t.Fatal("expected validation errors for 720p + duration")
	}
}

func TestHiggsfieldImageStatusNormalization(t *testing.T) {
	if HiggsfieldStatus("completed") != "succeeded" {
		t.Fatalf("completed mapping: %s", HiggsfieldStatus("completed"))
	}
	if HiggsfieldStatus("queued") != "running" {
		t.Fatalf("queued mapping: %s", HiggsfieldStatus("queued"))
	}
	if HiggsfieldStatus("failed") != "failed" {
		t.Fatalf("failed mapping: %s", HiggsfieldStatus("failed"))
	}
}
