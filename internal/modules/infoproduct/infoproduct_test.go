package infoproduct

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateSourceURL(t *testing.T) {
	bad := []string{
		"", "ftp://example.com/v.mp4", "file:///etc/passwd", "http://127.0.0.1/x",
		"http://localhost:8099/x", "https://10.0.0.5/v.mp4", "http://169.254.169.254/latest",
		"https://user:pass@example.com/v", "https://example.com:8080/v", "http://[::1]/x",
	}
	for _, raw := range bad {
		if _, err := validateSourceURL(raw); err == nil {
			t.Errorf("validateSourceURL(%q) accepted an unsafe URL", raw)
		}
	}
	if _, err := validateSourceURL("https://8.8.8.8/video.mp4"); err != nil {
		t.Errorf("public IP rejected: %v", err)
	}
}

func TestParseSceneTimes(t *testing.T) {
	stderr := `[Parsed_showinfo_1 @ 000] n:   0 pts:  30000 pts_time:1.25 pos: 1 fmt:yuv420p
[Parsed_showinfo_1 @ 000] n:   1 pts:  90000 pts_time:3.5 pos: 2
[Parsed_showinfo_1 @ 000] n:   2 pts:      0 pts_time:0 pos: 3`
	got := parseSceneTimes(stderr)
	if len(got) != 2 || got[0] != 1.25 || got[1] != 3.5 {
		t.Fatalf("cuts = %v, want [1.25 3.5]", got)
	}
}

func TestNearestRatio(t *testing.T) {
	cases := map[[2]int]string{{1080, 1920}: "9:16", {1920, 1080}: "16:9", {1080, 1080}: "1:1", {1080, 1350}: "4:5"}
	for dims, want := range cases {
		if got := nearestRatio(dims[0], dims[1]); got != want {
			t.Errorf("nearestRatio(%v) = %s, want %s", dims, got, want)
		}
	}
}

func TestExtractJSON(t *testing.T) {
	out, err := extractJSON("Here you go:\n```json\n{\"a\": {\"b\": 1}}\n```")
	if err != nil || string(out) != `{"a": {"b": 1}}` {
		t.Fatalf("got %q, %v", out, err)
	}
	if _, err := extractJSON("no json here"); err == nil {
		t.Fatal("expected error without JSON")
	}
	if _, err := extractJSON("{broken"); err == nil {
		t.Fatal("expected error with broken JSON")
	}
}

func TestSegmentsRequired(t *testing.T) {
	cases := []struct {
		total float64
		max   int
		want  int
	}{{12, 15, 1}, {15, 15, 1}, {15.4, 15, 2}, {30, 15, 2}, {31, 15, 3}, {0, 15, 1}, {40, 0, 1}, {500, 5, maxSegments}}
	for _, tc := range cases {
		if got := segmentsRequired(tc.total, tc.max); got != tc.want {
			t.Errorf("segmentsRequired(%v,%d) = %d, want %d", tc.total, tc.max, got, tc.want)
		}
	}
}

func TestPickDurationAndString(t *testing.T) {
	if got := pickDuration(12, []int{5, 10, 15}, 15); got != 10 {
		t.Errorf("duration = %d, want 10", got)
	}
	if got := pickDuration(30, []int{5, 10, 15}, 15); got != 15 {
		t.Errorf("duration = %d, want 15", got)
	}
	if got := pickDuration(2, []int{5, 10}, 10); got != 5 {
		t.Errorf("duration = %d, want 5 (smallest allowed)", got)
	}
	if got := pickDuration(8, nil, 6); got != 6 {
		t.Errorf("duration = %d, want 6 (clamped)", got)
	}
	if got := pickString("21:9", "9:16", []string{"16:9", "9:16"}); got != "9:16" {
		t.Errorf("ratio = %s, want preferred 9:16", got)
	}
	if got := pickString("720P", "", []string{"480p", "720p"}); got != "720p" {
		t.Errorf("resolution = %s, want 720p", got)
	}
}

func TestBuildVideoPrompt(t *testing.T) {
	raw := json.RawMessage(`{
		"prompt":"@Image1 on a table",
		"duration":12,"ratio":"1:1","resolution":"1080p","generate_audio":true,
		"image_roles":["front"],
		"audio_roles":[{"kind":"voiceover","note":"narration"}],
		"segments":[{"index":9,"start":0,"end":12,"prompt":"@Image1 on a table"}],
		"warnings":["ojo"]}`)
	target := ComposeTarget{Model: "m", MaxDuration: 15, Durations: []int{5, 10, 15},
		Ratios: []string{"16:9", "9:16"}, Resolutions: []string{"720p", "1080p"}, SupportsAudio: false}
	vp, err := buildVideoPrompt(raw, target, []string{"f1"}, []string{"a1"}, 12, "9:16", 1)
	if err != nil {
		t.Fatal(err)
	}
	if vp.Duration != 10 || vp.Ratio != "9:16" || vp.Resolution != "1080p" || vp.GenerateAudio {
		t.Fatalf("settings not clamped to the model: %+v", vp)
	}
	if len(vp.Ingredients) != 1 || vp.Ingredients[0].Tag != "@Image1" || vp.Ingredients[0].FileID != "f1" || vp.Ingredients[0].Role != "front" {
		t.Fatalf("ingredients = %+v", vp.Ingredients)
	}
	if len(vp.AudioRefs) != 1 || vp.AudioRefs[0].Kind != "voiceover" || vp.AudioRefs[0].Tag != "@Audio1" {
		t.Fatalf("audio refs = %+v", vp.AudioRefs)
	}
	if len(vp.Segments) != 1 || vp.Segments[0].Index != 1 {
		t.Fatalf("segments = %+v", vp.Segments)
	}
}

func TestBuildVideoPromptWarnsOnSegmentMismatch(t *testing.T) {
	raw := json.RawMessage(`{"prompt":"p","duration":15,"ratio":"9:16","resolution":"720p"}`)
	vp, err := buildVideoPrompt(raw, ComposeTarget{Model: "m", MaxDuration: 15}, []string{"f"}, nil, 30, "9:16", 2)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range vp.Warnings {
		if strings.Contains(w, "2 generaciones") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected segment mismatch warning, got %v", vp.Warnings)
	}
	if _, err := buildVideoPrompt(json.RawMessage(`{"prompt":""}`), ComposeTarget{Model: "m"}, nil, nil, 0, "", 1); err == nil {
		t.Fatal("empty prompt must fail")
	}
}

func TestDownloadErrorMessage(t *testing.T) {
	msg := downloadErrorMessage("ERROR: [Instagram] abc: login required to view this", nil)
	if !strings.Contains(msg, "sube el archivo") {
		t.Fatalf("login errors must suggest uploading: %s", msg)
	}
}
