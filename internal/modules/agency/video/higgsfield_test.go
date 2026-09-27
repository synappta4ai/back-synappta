package agencyvideo

import (
	"encoding/json"
	"testing"

	"synapta/internal/modules/agency"
)

func TestHiggsfieldBuildPayload(t *testing.T) {
	g := NewHiggsfieldGenerator()
	req := &agency.GeneratorRequest{
		Model:      "higgsfield-kling-3-0-turbo-t2v",
		Content:    []agency.ContentItem{{Type: "text", DataURL: "", Text: "ola cinematica"}},
		Ratio:      "16:9",
		Duration:   5,
		Resolution: "720p",
	}
	payload := g.BuildPayload(req)
	if payload["prompt"] != "ola cinematica." {
		t.Fatalf("prompt: %v", payload["prompt"])
	}
	if payload["aspect_ratio"] != "16:9" || payload["duration"] != 5 || payload["resolution"] != "720p" {
		t.Fatalf("params: %v", payload)
	}
	if _, has := payload["image_urls"]; has {
		t.Fatalf("image_urls should be absent without image content")
	}

	// Reference-to-video: image refs land in image_urls.
	req2 := &agency.GeneratorRequest{
		Model:   "higgsfield-wan-2-6-reference",
		Content: []agency.ContentItem{{Type: "text", Text: "remix"}, {Type: "image", DataURL: "https://example.com/a.jpg"}},
	}
	p2 := g.BuildPayload(req2)
	if arr, ok := p2["image_urls"].([]string); !ok || len(arr) != 1 || arr[0] != "https://example.com/a.jpg" {
		t.Fatalf("image_urls: %v", p2["image_urls"])
	}
	out, _ := json.Marshal(p2)
	if len(out) == 0 {
		t.Fatal("payload not serializable")
	}
}

func TestHiggsfieldBuildPayloadImageToVideo(t *testing.T) {
	g := NewHiggsfieldGenerator()
	req := &agency.GeneratorRequest{
		Model:      "higgsfield-seedance-2-5-i2v",
		Content:    []agency.ContentItem{{Type: "image", DataURL: "https://example.com/frame.jpg"}},
		Duration:   5,
		Resolution: "720p",
	}
	p := g.BuildPayload(req)
	if p["image_url"] != "https://example.com/frame.jpg" {
		t.Fatalf("image_url singular: %v", p["image_url"])
	}
	if _, has := p["image_urls"]; has {
		t.Fatal("i2v payload must not carry image_urls array")
	}
	if _, has := p["aspect_ratio"]; has {
		t.Fatal("i2v payload should not send aspect_ratio (framing follows image)")
	}

	// Multiple images on an i2v endpoint: the strict schema only accepts one,
	// so the first image is sent and extras are dropped.
	req2 := &agency.GeneratorRequest{
		Model: "higgsfield-seedance-2-5-i2v",
		Content: []agency.ContentItem{
			{Type: "image", DataURL: "https://example.com/a.jpg"},
			{Type: "image", DataURL: "https://example.com/b.jpg"},
		},
	}
	p2 := g.BuildPayload(req2)
	if p2["image_url"] != "https://example.com/a.jpg" {
		t.Fatalf("multi-image i2v should send the first image: %v", p2)
	}
	if _, has := p2["image_urls"]; has {
		t.Fatal("i2v payload must not carry image_urls array")
	}
}

func TestHiggsfieldStatusMapping(t *testing.T) {
	if higgsfieldStatus("completed") != "succeeded" {
		t.Fatalf("completed mapping: %s", higgsfieldStatus("completed"))
	}
	if higgsfieldStatus("queued") != "running" {
		t.Fatalf("queued mapping: %s", higgsfieldStatus("queued"))
	}
	if higgsfieldStatus("failed") != "failed" {
		t.Fatalf("failed mapping: %s", higgsfieldStatus("failed"))
	}
}

func TestHiggsfieldMatch(t *testing.T) {
	g := NewHiggsfieldGenerator()
	if !g.Match("higgsfield-kling-3-0-turbo-t2v") {
		t.Fatal("should match higgsfield models")
	}
	if g.Match("dreamina-seedance-2-5-260628") {
		t.Fatal("should not match seedance")
	}
}
