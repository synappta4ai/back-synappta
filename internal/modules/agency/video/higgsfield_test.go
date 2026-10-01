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

func TestHiggsfieldBuildPayloadT2VWithImageRoutesToI2V(t *testing.T) {
	g := NewHiggsfieldGenerator()
	req := &agency.GeneratorRequest{
		Model:         "higgsfield-seedance-2-5-t2v",
		Content:       []agency.ContentItem{{Type: "text", Text: "anima el logo"}, {Type: "image", DataURL: "https://example.com/logo.png"}},
		Ratio:         "16:9",
		Duration:      5,
		Resolution:    "480p",
		Endpoint:      "/bytedance/seedance-2.5/text-to-video",
		ImageEndpoint: "/bytedance/seedance-2.5/image-to-video",
	}
	p := g.BuildPayload(req)
	if req.Endpoint != "/bytedance/seedance-2.5/image-to-video" {
		t.Fatalf("endpoint should switch to the i2v twin: %s", req.Endpoint)
	}
	if p["image_url"] != "https://example.com/logo.png" {
		t.Fatalf("image_url singular: %v", p["image_url"])
	}
	if _, has := p["image_urls"]; has {
		t.Fatal("routed i2v payload must not carry image_urls array")
	}
	if _, has := p["aspect_ratio"]; has {
		t.Fatal("routed i2v payload should drop aspect_ratio (framing follows image)")
	}
	if p["duration"] != 5 || p["resolution"] != "480p" {
		t.Fatalf("i2v keeps duration/resolution: %v", p)
	}
}

func TestHiggsfieldBuildPayloadT2VWithoutImageKeepsRoute(t *testing.T) {
	g := NewHiggsfieldGenerator()
	req := &agency.GeneratorRequest{
		Model:         "higgsfield-seedance-2-5-t2v",
		Content:       []agency.ContentItem{{Type: "text", Text: "ciudad cyberpunk"}},
		Ratio:         "16:9",
		Endpoint:      "/bytedance/seedance-2.5/text-to-video",
		ImageEndpoint: "/bytedance/seedance-2.5/image-to-video",
	}
	p := g.BuildPayload(req)
	if req.Endpoint != "/bytedance/seedance-2.5/text-to-video" {
		t.Fatalf("text-only payload must keep the t2v endpoint: %s", req.Endpoint)
	}
	if _, has := p["image_url"]; has {
		t.Fatal("no image_url without image content")
	}
	if p["aspect_ratio"] != "16:9" {
		t.Fatalf("t2v keeps aspect_ratio: %v", p["aspect_ratio"])
	}
}

func TestHiggsfieldBuildPayloadT2VMultiImageRoutesToReference(t *testing.T) {
	g := NewHiggsfieldGenerator()
	req := &agency.GeneratorRequest{
		Model:             "higgsfield-seedance-2-5-t2v",
		Content:           []agency.ContentItem{{Type: "text", Text: "mezcla los logos"}, {Type: "image", DataURL: "https://example.com/a.png"}, {Type: "image", DataURL: "https://example.com/b.png"}},
		Ratio:             "16:9",
		Duration:          8,
		Resolution:        "720p",
		Endpoint:          "/bytedance/seedance-2.5/text-to-video",
		ImageEndpoint:     "/bytedance/seedance-2.5/image-to-video",
		ReferenceEndpoint: "/bytedance/seedance-2.5/reference-to-video",
	}
	p := g.BuildPayload(req)
	if req.Endpoint != "/bytedance/seedance-2.5/reference-to-video" {
		t.Fatalf("multi-image should switch to the reference twin: %s", req.Endpoint)
	}
	arr, ok := p["image_urls"].([]string)
	if !ok || len(arr) != 2 {
		t.Fatalf("image_urls array of 2: %v", p["image_urls"])
	}
	if p["aspect_ratio"] != "16:9" {
		t.Fatalf("reference route keeps aspect_ratio: %v", p["aspect_ratio"])
	}
	if _, has := p["image_url"]; has {
		t.Fatal("multi-image payload must not carry singular image_url")
	}
}

func TestHiggsfieldBuildPayloadVideoRefRoutesToReference(t *testing.T) {
	g := NewHiggsfieldGenerator()
	req := &agency.GeneratorRequest{
		Model:             "higgsfield-seedance-2-5-t2v",
		Content:           []agency.ContentItem{{Type: "text", Text: "remix"}, {Type: "video", DataURL: "https://example.com/clip.mp4"}},
		Endpoint:          "/bytedance/seedance-2.5/text-to-video",
		ReferenceEndpoint: "/bytedance/seedance-2.5/reference-to-video",
	}
	p := g.BuildPayload(req)
	if req.Endpoint != "/bytedance/seedance-2.5/reference-to-video" {
		t.Fatalf("video reference should switch to the reference twin: %s", req.Endpoint)
	}
	if arr, ok := p["video_urls"].([]string); !ok || len(arr) != 1 {
		t.Fatalf("video_urls: %v", p["video_urls"])
	}
}

func TestHiggsfieldBuildPayloadI2VTwinUnaffectedByReferenceEndpoint(t *testing.T) {
	// The dedicated i2v model keeps its behavior even when a
	// ReferenceEndpoint is defined: one image_url, aspect_ratio dropped.
	g := NewHiggsfieldGenerator()
	req := &agency.GeneratorRequest{
		Model:             "higgsfield-seedance-2-5-i2v",
		Content:           []agency.ContentItem{{Type: "image", DataURL: "https://example.com/frame.jpg"}},
		Ratio:             "16:9",
		Endpoint:          "/bytedance/seedance-2.5/image-to-video",
		ReferenceEndpoint: "/bytedance/seedance-2.5/reference-to-video",
	}
	p := g.BuildPayload(req)
	if req.Endpoint != "/bytedance/seedance-2.5/image-to-video" {
		t.Fatalf("i2v endpoint must stay: %s", req.Endpoint)
	}
	if p["image_url"] != "https://example.com/frame.jpg" {
		t.Fatalf("singular image_url: %v", p["image_url"])
	}
	if _, has := p["aspect_ratio"]; has {
		t.Fatal("i2v payload should drop aspect_ratio")
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
