package render

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func f64(v float64) *float64 { return &v }

func TestNormalizeDefaultsAndLimits(t *testing.T) {
	p := Project{Video: []VideoClip{{Source: Source{FileID: "a"}}}}
	if err := p.normalize(); err != nil {
		t.Fatal(err)
	}
	if p.Width != 1280 || p.Height != 720 || p.FPS != 30 || p.Video[0].Kind != "video" {
		t.Fatalf("defaults not applied: %+v", p)
	}

	bad := []Project{
		{},                                       // no clips
		{Video: []VideoClip{{Source: Source{}}}}, // no source
		{Video: []VideoClip{{Source: Source{FileID: "a", Output: "/outputs/x"}}}}, // two sources
		{Width: 5000, Video: []VideoClip{{Source: Source{FileID: "a"}}}},
		{FPS: 200, Video: []VideoClip{{Source: Source{FileID: "a"}}}},
		{Video: []VideoClip{{Source: Source{FileID: "a"}, In: -1}}},
		{Video: []VideoClip{{Source: Source{FileID: "a"}}}, Texts: []TextClip{{Text: "x", Color: "red"}}},
		{Video: []VideoClip{{Source: Source{FileID: "a"}}}, Texts: []TextClip{{Text: "", Color: "#FFFFFF"}}},
		{Video: []VideoClip{{Source: Source{FileID: "a"}}}, Audio: []AudioClip{{Source: Source{FileID: "b"}, Volume: f64(5)}}},
	}
	for i, b := range bad {
		if err := b.normalize(); err == nil {
			t.Errorf("case %d should be rejected: %+v", i, b)
		}
	}
}

func TestBuildPlan(t *testing.T) {
	p := &Project{
		Width: 1280, Height: 720, FPS: 30,
		Video: []VideoClip{
			{Kind: "video", Source: Source{FileID: "a"}, In: 1, Out: 4, Volume: f64(0.5)},
			{Kind: "image", Source: Source{FileID: "b"}, Length: 2},
		},
		Audio: []AudioClip{
			{Source: Source{FileID: "m"}, Start: 1, Volume: f64(0.8)},
			{Source: Source{FileID: "late"}, Start: 99}, // past the end: skipped
		},
		Texts: []TextClip{
			{Text: "Hola: 50% 'x'", Start: 0, End: 2, X: 0.5, Y: 0.9, Size: 48, Color: "#FFCC00"},
			{Text: "never", Start: 50, End: 60, Size: 48, Color: "#FFFFFF"},
		},
	}
	if err := p.normalize(); err != nil {
		t.Fatal(err)
	}
	vin := []resolved{
		{Path: "/in/a.mp4", Info: mediaInfo{Duration: 10, HasVideo: true, HasAudio: true}},
		{Path: "/in/b.png", Info: mediaInfo{HasVideo: true}},
	}
	ain := []resolved{
		{Path: "/in/m.mp3", Info: mediaInfo{Duration: 30, HasAudio: true}},
		{Path: "/in/late.mp3", Info: mediaInfo{Duration: 30, HasAudio: true}},
	}
	pl, err := buildPlan(p, vin, ain, "font.ttf", 2)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(pl.TotalSec-5) > 1e-9 {
		t.Fatalf("total = %v, want 5 (3s clip + 2s image)", pl.TotalSec)
	}
	joined := strings.Join(pl.Args, " ")
	for _, want := range []string{
		"trim=start=1.000:end=4.000", "volume=0.500",
		"-loop 1 -framerate 30 -t 2.000 -i /in/b.png",
		"concat=n=2:v=1:a=1[vc][ac]",
		"drawtext=textfile=t0.txt:expansion=none:fontfile=font.ttf",
		"fontcolor=0xFFCC00", "enable='between(t,0.000,2.000)'",
		"adelay=1000|1000", "amix=inputs=2", "normalize=0",
		"-map [vt0] -map [aout]", "-c:v libx264", "-c:a aac", "-movflags +faststart",
		"-threads 2", "out.mp4",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	// The audio source is input #2 (after the two video inputs); the skipped
	// late clip must not shift or add inputs.
	if !strings.Contains(joined, "[2:a]atrim") || strings.Contains(joined, "late.mp3") {
		t.Errorf("audio input indexing wrong:\n%s", joined)
	}
	if len(pl.TextFiles) != 1 || pl.TextFiles["t0.txt"] != "Hola: 50% 'x'" {
		t.Errorf("text files = %v (text must travel verbatim, unescaped)", pl.TextFiles)
	}
}

func TestBuildPlanFormats(t *testing.T) {
	p := &Project{Video: []VideoClip{{Kind: "video", Source: Source{FileID: "a"}, Out: 3}}}
	if err := p.normalize(); err != nil {
		t.Fatal(err)
	}
	vin := []resolved{{Path: "/in/a.mp4", Info: mediaInfo{Duration: 4, HasVideo: true, HasAudio: true}}}
	ain := []resolved{{Path: "/in/m.mp3", Info: mediaInfo{Duration: 4, HasAudio: true}}}
	p.Audio = []AudioClip{{Source: Source{FileID: "m"}, Out: 3}}

	cases := []struct {
		format string
		want   []string
		not    []string
	}{
		{"mp4", []string{"-c:v libx264", "-c:a aac", "out.mp4", "-map [aout]"}, nil},
		{"h265", []string{"-c:v libx265", "-tag:v hvc1", "out.mp4"}, nil},
		{"mov", []string{"-c:v libx264", "out.mov", "-c:a aac"}, nil},
		{"webm", []string{"-c:v libvpx-vp9", "-c:a libopus", "out.webm"}, nil},
		{"gif", []string{"palettegen", "paletteuse", "-loop 0", "out.gif"}, []string{"-map [aout]", "-c:a"}},
	}
	for _, c := range cases {
		pl, err := buildPlan(p, vin, ain, "", 0, c.format)
		if err != nil {
			t.Fatalf("%s: %v", c.format, err)
		}
		joined := strings.Join(pl.Args, " ")
		for _, w := range c.want {
			if !strings.Contains(joined, w) {
				t.Errorf("%s: missing %q in:\n%s", c.format, w, joined)
			}
		}
		for _, n := range c.not {
			if strings.Contains(joined, n) {
				t.Errorf("%s: unexpected %q in:\n%s", c.format, n, joined)
			}
		}
	}
	if _, err := normalizeFormat("avi"); err == nil {
		t.Error("avi must be rejected")
	}
	if f, _ := normalizeFormat("HEVC"); f != "mp4_h265" {
		t.Errorf("alias HEVC => %q", f)
	}
}

func TestBuildPlanDetachedAudioIsNotDoubled(t *testing.T) {
	p := &Project{
		Video: []VideoClip{{Kind: "video", Source: Source{FileID: "a"}, Out: 4, AudioDetached: true}},
		Audio: []AudioClip{{Source: Source{FileID: "a"}, Start: 0, Out: 4}},
	}
	if err := p.normalize(); err != nil {
		t.Fatal(err)
	}
	vin := []resolved{{Path: "/in/a.mp4", Info: mediaInfo{Duration: 5, HasVideo: true, HasAudio: true}}}
	ain := []resolved{{Path: "/in/a.mp4", Info: mediaInfo{Duration: 5, HasAudio: true}}}
	pl, err := buildPlan(p, vin, ain, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(pl.Args, " ")
	// The base clip contributes silence (its sound lives in the audio track)...
	if !strings.Contains(joined, "[0:v]trim=") || !strings.Contains(joined, "anullsrc=r=48000:cl=stereo,atrim=duration=4.000") {
		t.Errorf("detached clip must be silent in the video track:\n%s", joined)
	}
	// ...and exactly one mixed audio input remains.
	if strings.Count(joined, "atrim=start=0.000:end=4.000") != 1 {
		t.Errorf("expected a single mixed audio input:\n%s", joined)
	}
}

func TestBuildPlanMultiTrack(t *testing.T) {
	p := &Project{
		Width: 1280, Height: 720, FPS: 30,
		Video: []VideoClip{
			{Kind: "video", Track: 0, Source: Source{FileID: "base"}, Out: 3},
			{Kind: "video", Track: 1, Start: 1, Source: Source{FileID: "over"}, Out: 2, Volume: f64(0.5)},
			{Kind: "video", Track: 1, Start: 8, Source: Source{FileID: "late"}, Out: 1},
		},
	}
	if err := p.normalize(); err != nil {
		t.Fatal(err)
	}
	vin := []resolved{
		{Path: "/in/base.mp4", Info: mediaInfo{Duration: 5, HasVideo: true, HasAudio: true}},
		{Path: "/in/over.mp4", Info: mediaInfo{Duration: 5, HasVideo: true, HasAudio: true}},
		{Path: "/in/late.mp4", Info: mediaInfo{Duration: 5, HasVideo: true}},
	}
	pl, err := buildPlan(p, vin, nil, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	// Base clip is 3s; the late overlay pushes the timeline to 9s.
	if math.Abs(pl.TotalSec-9) > 1e-9 {
		t.Fatalf("total = %v, want 9", pl.TotalSec)
	}
	joined := strings.Join(pl.Args, " ")
	for _, want := range []string{
		"concat=n=1:v=1:a=1[vc][ac]",
		"overlay=0:0:eof_action=pass:enable='between(t,1.000,3.000)'",
		"overlay=0:0:eof_action=pass:enable='between(t,8.000,9.000)'",
		"adelay=1000|1000", // the overlay clip's own sound is mixed at its Start
		"-map [vx1] -map [aout]",
		"-t 9.000",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	// Only the overlay with audio contributes a mix input.
	if strings.Count(joined, "adelay=") != 1 {
		t.Errorf("expected exactly one mixed overlay audio:\n%s", joined)
	}
}

func TestBuildPlanRequiresBaseTrack(t *testing.T) {
	p := &Project{Video: []VideoClip{{Kind: "video", Track: 2, Start: 0, Source: Source{FileID: "a"}, Out: 2}}}
	if err := p.normalize(); err != nil {
		t.Fatal(err)
	}
	// A single clip on "track 2" is still the lowest track in use: it is base.
	if _, err := buildPlan(p, []resolved{{Info: mediaInfo{Duration: 4, HasVideo: true}}}, nil, "", 0); err != nil {
		t.Fatalf("single-track project must render as base: %v", err)
	}
	p.Video = nil
	if _, err := buildPlan(p, nil, nil, "", 0); err == nil {
		t.Fatal("expected error without video")
	}
}

func TestBuildPlanRejectsBadInputs(t *testing.T) {
	mk := func() *Project {
		p := &Project{Video: []VideoClip{{Source: Source{FileID: "a"}}}}
		_ = p.normalize()
		return p
	}
	// audio-only file used as video
	if _, err := buildPlan(mk(), []resolved{{Info: mediaInfo{Duration: 5, HasAudio: true}}}, nil, "", 0); err == nil {
		t.Error("expected error for source without video")
	}
	// empty trim
	p := mk()
	p.Video[0].In = 9
	p.Video[0].Out = 9.01
	if _, err := buildPlan(p, []resolved{{Info: mediaInfo{Duration: 10, HasVideo: true}}}, nil, "", 0); err == nil {
		t.Error("expected error for empty trim")
	}
	// misaligned inputs
	if _, err := buildPlan(mk(), nil, nil, "", 0); err == nil {
		t.Error("expected error for misaligned inputs")
	}
	// music source without sound
	p = mk()
	p.Audio = []AudioClip{{Source: Source{FileID: "m"}}}
	_, err := buildPlan(p, []resolved{{Info: mediaInfo{Duration: 5, HasVideo: true}}},
		[]resolved{{Info: mediaInfo{Duration: 5}}}, "", 0)
	if err == nil {
		t.Error("expected error for audio clip without sound")
	}
}

func TestParseOutputPath(t *testing.T) {
	cases := []struct {
		in    string
		rel   string
		local bool
	}{
		{"/outputs/video/a.mp4", "video/a.mp4", true},
		{"http://localhost:8099/outputs/video/a.mp4", "video/a.mp4", true},
		{"/api/v1/t/acme/outputs/video/a.mp4", "video/a.mp4", false},
		{"https://back-dev.example.cloud/api/v1/t/other/outputs/img_1.png", "img_1.png", false},
	}
	for _, c := range cases {
		rel, local, err := parseOutputPath(c.in)
		if err != nil || rel != c.rel || local != c.local {
			t.Errorf("%q => %q local=%v err=%v", c.in, rel, local, err)
		}
	}
	for _, bad := range []string{
		"", "/etc/passwd", "/outputs/", "/outputs/../x", "/api/v1/t/acme/outputs/../x",
		"/api/v1/t/acme/files/abc/serve", "https://evil.example/other/a.mp4", "/outputs/a\\b.mp4",
	} {
		if _, _, err := parseOutputPath(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestResolveOutputRejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "video"), 0o755); err != nil {
		t.Fatal(err)
	}
	ok := filepath.Join(dir, "video", "a.mp4")
	if err := os.WriteFile(ok, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveOutput(dir, "/outputs/video/a.mp4"); err != nil || !strings.HasSuffix(got, "a.mp4") {
		t.Fatalf("valid path rejected: %v %v", got, err)
	}
	if _, err := resolveOutput(dir, "http://localhost:8099/outputs/video/a.mp4"); err != nil {
		t.Fatalf("full URL rejected: %v", err)
	}
	for _, bad := range []string{
		"/outputs/../secret.txt", "/outputs/video/../../x", "/etc/passwd", "/outputs/",
		"/outputs/video/missing.mp4", "https://evil.example/other/a.mp4",
	} {
		if _, err := resolveOutput(dir, bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

// TestRenderWithRealFFmpeg renders a real project end to end. Skipped when
// ffmpeg/ffprobe are not installed.
func TestRenderWithRealFFmpeg(t *testing.T) {
	ffmpeg, err := findBin("ffmpeg", "FFMPEG_PATH")
	if err != nil {
		t.Skip("ffmpeg not available")
	}
	ffprobe, err := findBin("ffprobe", "FFPROBE_PATH")
	if err != nil {
		t.Skip("ffprobe not available")
	}
	dir := t.TempDir()
	gen := func(name string, args ...string) string {
		out := filepath.Join(dir, name)
		full := append([]string{"-y", "-loglevel", "error"}, args...)
		full = append(full, out)
		if b, err := exec.Command(ffmpeg, full...).CombinedOutput(); err != nil {
			t.Fatalf("generate %s: %v\n%s", name, err, b)
		}
		return out
	}
	clipA := gen("a.mp4", "-f", "lavfi", "-i", "testsrc=size=640x360:rate=25:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3", "-shortest", "-pix_fmt", "yuv420p")
	clipB := gen("b.mp4", "-f", "lavfi", "-i", "color=c=blue:size=320x240:rate=30:duration=2", "-pix_fmt", "yuv420p") // no audio
	img := gen("c.png", "-f", "lavfi", "-i", "color=c=red:size=200x200", "-frames:v", "1")
	music := gen("m.wav", "-f", "lavfi", "-i", "sine=frequency=880:duration=10")

	p := &Project{
		Width: 640, Height: 360, FPS: 25,
		Video: []VideoClip{
			{Kind: "video", Source: Source{FileID: "a"}, In: 0.5, Out: 2.5},
			{Kind: "video", Source: Source{FileID: "b"}},
			{Kind: "image", Source: Source{FileID: "c"}, Length: 1},
		},
		Audio: []AudioClip{{Source: Source{FileID: "m"}, Start: 0.5, Volume: f64(0.6)}},
		Texts: []TextClip{{Text: "Hola mundo", Start: 0, End: 2, X: 0.5, Y: 0.8, Size: 32, Color: "#FFFFFF"}},
	}
	if err := p.normalize(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	loc := func(path string) resolved {
		info, err := probe(ctx, ffprobe, path)
		if err != nil {
			t.Fatal(err)
		}
		return resolved{Path: path, Info: *info}
	}
	vin := []resolved{loc(clipA), loc(clipB), loc(img)}
	ain := []resolved{loc(music)}

	font := findFont()
	fontName := ""
	work := t.TempDir()
	if font != "" {
		fontName = "font.ttf"
		if err := copyFile(font, filepath.Join(work, fontName)); err != nil {
			t.Fatal(err)
		}
	}
	pl, err := buildPlan(p, vin, ain, fontName, 2)
	if err != nil {
		t.Fatal(err)
	}
	if font == "" {
		t.Log("no system font found: skipping drawtext part of the check")
		p.Texts = nil
		if pl, err = buildPlan(p, vin, ain, "", 2); err != nil {
			t.Fatal(err)
		}
	}
	for name, text := range pl.TextFiles {
		if err := os.WriteFile(filepath.Join(work, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var lastProgress float64
	if err := runFFmpeg(ctx, ffmpeg, work, pl.Args, pl.TotalSec, func(f float64) { lastProgress = f }); err != nil {
		t.Fatalf("render failed: %v\nargs: %v", err, pl.Args)
	}
	out := loc(filepath.Join(work, pl.OutputName))
	if !out.Info.HasVideo || !out.Info.HasAudio {
		t.Fatalf("output lacks a stream: %+v", out.Info)
	}
	want := 2.0 + 2.0 + 1.0
	if math.Abs(out.Info.Duration-want) > 0.3 {
		t.Fatalf("duration = %.2f, want ~%.1f", out.Info.Duration, want)
	}
	if lastProgress <= 0 {
		t.Error("no progress was reported")
	}
}
