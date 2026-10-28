package render

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// findBin locates an external tool: explicit env var first, then PATH, then
// the usual per-user Windows install folders. (Deliberately a local copy: the
// infoproduct package keeps its own unexported version and its tests call it.)
func findBin(name, envVar string) (string, error) {
	if p := strings.TrimSpace(os.Getenv(envVar)); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	if runtime.GOOS == "windows" {
		local := os.Getenv("LOCALAPPDATA")
		exe := name + ".exe"
		candidates := []string{
			filepath.Join(local, "yt-dlp", exe),
			filepath.Join(local, "Microsoft", "WinGet", "Links", exe),
		}
		if m, _ := filepath.Glob(filepath.Join(local, "Microsoft", "WinGet", "Packages", "*", "*", "bin", exe)); len(m) > 0 {
			candidates = append(candidates, m...)
		}
		for _, c := range candidates {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				return c, nil
			}
		}
	}
	return "", fmt.Errorf("%s no esta instalado en el servidor (instalalo o define %s)", name, envVar)
}

// mediaInfo is what ffprobe reports about one input.
type mediaInfo struct {
	Duration float64
	HasVideo bool
	HasAudio bool
}

// probe measures an input file with ffprobe.
func probe(ctx context.Context, ffprobe, path string) (*mediaInfo, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, ffprobe, "-v", "error", "-print_format", "json",
		"-show_format", "-show_streams", path)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffprobe: %s", firstLine(errb.String(), err.Error()))
	}
	var doc struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
			Duration  string `json:"duration"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		return nil, fmt.Errorf("ffprobe: respuesta ilegible: %w", err)
	}
	mi := &mediaInfo{}
	mi.Duration, _ = strconv.ParseFloat(doc.Format.Duration, 64)
	for _, s := range doc.Streams {
		switch s.CodecType {
		case "video":
			mi.HasVideo = true
			if mi.Duration == 0 {
				mi.Duration, _ = strconv.ParseFloat(s.Duration, 64)
			}
		case "audio":
			mi.HasAudio = true
			if mi.Duration == 0 {
				mi.Duration, _ = strconv.ParseFloat(s.Duration, 64)
			}
		}
	}
	return mi, nil
}

// runFFmpeg executes ffmpeg and reports progress (0..1 of totalSec) through
// onProgress. cmd.Dir is dir so drawtext textfile paths can stay relative.
func runFFmpeg(ctx context.Context, ffmpeg, dir string, args []string, totalSec float64, onProgress func(float64)) error {
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	cmd.Dir = dir
	var errb bytes.Buffer
	cmd.Stderr = &errb
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := sc.Text()
		// out_time_us is microseconds of output produced so far.
		if v, ok := strings.CutPrefix(line, "out_time_us="); ok && totalSec > 0 {
			if us, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && us >= 0 {
				p := us / 1e6 / totalSec
				if p > 1 {
					p = 1
				}
				onProgress(p)
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("render cancelado o con tiempo agotado: %w", ctx.Err())
		}
		return fmt.Errorf("ffmpeg: %s", lastLines(errb.String(), 3))
	}
	return nil
}

func firstLine(s, fallback string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return fallback
}

func lastLines(s string, n int) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			keep = append(keep, l)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	if len(keep) == 0 {
		return "error desconocido"
	}
	return strings.Join(keep, " | ")
}
