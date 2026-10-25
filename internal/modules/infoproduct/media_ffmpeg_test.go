package infoproduct

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestMediaPipelineWithFFmpeg builds a 6 s clip with two hard cuts and runs
// the real probe / cut detection / normalize steps. Skipped without ffmpeg.
func TestMediaPipelineWithFFmpeg(t *testing.T) {
	ffmpeg, err := findBin("ffmpeg", "FFMPEG_PATH")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := findBin("ffprobe", "FFPROBE_PATH"); err != nil {
		t.Skip("ffprobe not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	// Three 2 s segments of different patterns, portrait 1080x1920, with audio.
	_, stderr, err := run(ctx, ffmpeg, "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=red:s=1080x1920:d=2:r=24",
		"-f", "lavfi", "-i", "color=c=blue:s=1080x1920:d=2:r=24",
		"-f", "lavfi", "-i", "color=c=green:s=1080x1920:d=2:r=24",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6",
		"-filter_complex", "[0:v][1:v][2:v]concat=n=3:v=1:a=0[v]",
		"-map", "[v]", "-map", "3:a", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", src)
	if err != nil {
		t.Fatalf("could not build fixture: %v %s", err, stderr)
	}

	meta, err := probe(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if meta.AspectRatio != "9:16" || !meta.HasAudio || meta.DurationSec < 5.5 || meta.DurationSec > 6.5 {
		t.Fatalf("meta = %+v", meta)
	}
	cuts, err := detectCuts(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(cuts) != 2 {
		t.Fatalf("cuts = %v, want 2 (at ~2s and ~4s)", cuts)
	}

	out := filepath.Join(dir, "norm.mp4")
	if err := normalize(ctx, src, out, 90); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() == 0 || st.Size() > normalizedMaxBytes {
		t.Fatalf("normalized file bad: %v size=%v", err, st)
	}
	nm, err := probe(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if nm.Width > 720 || nm.Height > 1280 || !nm.HasAudio {
		t.Fatalf("normalized meta = %+v, want short side <= 720 with audio", nm)
	}
}
