package infoproduct

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// MediaMeta is what ffprobe / ffmpeg measured on the reference video. The
// brain receives it as ground truth, so it never has to guess durations or
// count cuts by eye.
type MediaMeta struct {
	DurationSec float64   `json:"duration_sec"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	FPS         float64   `json:"fps"`
	AspectRatio string    `json:"aspect_ratio"`
	HasAudio    bool      `json:"has_audio"`
	Cuts        []float64 `json:"cuts"`
	CutsCount   int       `json:"cuts_count"`
	Source      string    `json:"source"` // file | url
}

const (
	maxCutsReported = 200
	sceneThreshold  = "0.30"
	// normalizedMaxBytes bounds the clip sent to the brain: base64 inflates
	// it by a third and providers cap the request body.
	normalizedMaxBytes = 18 << 20
)

// findBin locates an external tool: explicit env var first, then PATH, then
// the usual per-user Windows install folders.
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

// run executes a tool and returns stdout and stderr separately.
func run(ctx context.Context, bin string, args ...string) (stdout, stderr string, err error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err = cmd.Run()
	return out.String(), errb.String(), err
}

// ── URL safety ───────────────────────────────────────────────────────────

// isPrivateIP reports addresses the server must never be pointed at.
func isPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// validateSourceURL accepts only public http(s) URLs. The host is resolved
// and every address checked, so a name pointing at an internal service is
// rejected (SSRF guard) before any tool touches it.
func validateSourceURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil, errors.New("la URL no es valida")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("solo se aceptan URLs http o https")
	}
	if u.User != nil {
		return nil, errors.New("la URL no puede incluir usuario o clave")
	}
	if p := u.Port(); p != "" && p != "80" && p != "443" {
		return nil, errors.New("puerto no permitido")
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return nil, errors.New("direccion no permitida")
		}
		return u, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("no se pudo resolver el dominio de la URL")
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return nil, errors.New("direccion no permitida")
		}
	}
	return u, nil
}

// ── Download (yt-dlp) ────────────────────────────────────────────────────

var errorLine = regexp.MustCompile(`(?m)^ERROR:.*$`)

// downloadURL fetches the video behind a public page URL (Instagram, TikTok,
// YouTube, direct mp4...) into dir with yt-dlp and returns the file path.
func downloadURL(ctx context.Context, rawURL, dir string) (string, error) {
	ytdlp, err := findBin("yt-dlp", "YTDLP_PATH")
	if err != nil {
		return "", err
	}
	args := []string{
		"--no-playlist", "--no-warnings", "--no-progress",
		"--max-filesize", "300M",
		"--socket-timeout", "20", "--retries", "2",
		"--match-filter", "duration<=?600",
		"-f", "bv*[height<=1080]+ba/b[height<=1080]/b",
		"--merge-output-format", "mp4",
		"-o", filepath.Join(dir, "source.%(ext)s"),
	}
	if ff, err := findBin("ffmpeg", "FFMPEG_PATH"); err == nil {
		args = append(args, "--ffmpeg-location", ff)
	}
	args = append(args, "--", rawURL)

	dctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	_, stderr, runErr := run(dctx, ytdlp, args...)

	matches, _ := filepath.Glob(filepath.Join(dir, "source.*"))
	for _, m := range matches {
		if !strings.HasSuffix(m, ".part") && !strings.HasSuffix(m, ".ytdl") {
			return m, nil
		}
	}
	if dctx.Err() == context.DeadlineExceeded {
		return "", errors.New("la descarga del video tardo demasiado")
	}
	return "", errors.New(downloadErrorMessage(stderr, runErr))
}

// downloadErrorMessage turns yt-dlp's stderr into something a user can act
// on: private/login-gated posts are the common case on social networks.
func downloadErrorMessage(stderr string, runErr error) string {
	line := strings.TrimSpace(errorLine.FindString(stderr))
	low := strings.ToLower(line + " " + stderr)
	switch {
	case strings.Contains(low, "login") || strings.Contains(low, "cookies") || strings.Contains(low, "private") || strings.Contains(low, "rate-limit"):
		return "la red social pide iniciar sesion para ver este video (cuenta privada o limite de acceso): sube el archivo en su lugar"
	case strings.Contains(low, "unsupported url"):
		return "esa URL no es un video que se pueda descargar: pega el enlace directo del video o sube el archivo"
	case line != "":
		return "no se pudo descargar el video: " + strings.TrimPrefix(line, "ERROR: ")
	case runErr != nil:
		return "no se pudo descargar el video (el video supera el limite de tamano o duracion, o no esta disponible)"
	}
	return "no se pudo descargar el video"
}

// ── Probe / cuts / normalize (ffmpeg) ────────────────────────────────────

type ffprobeOut struct {
	Streams []struct {
		CodecType  string `json:"codec_type"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		RFrameRate string `json:"r_frame_rate"`
		Duration   string `json:"duration"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// probe reads duration, resolution, fps and audio presence.
func probe(ctx context.Context, path string) (MediaMeta, error) {
	ffprobe, err := findBin("ffprobe", "FFPROBE_PATH")
	if err != nil {
		return MediaMeta{}, err
	}
	out, stderr, runErr := run(ctx, ffprobe, "-v", "error", "-print_format", "json", "-show_streams", "-show_format", path)
	if runErr != nil {
		return MediaMeta{}, fmt.Errorf("el archivo no es un video valido: %s", strings.TrimSpace(firstLine(stderr)))
	}
	var parsed ffprobeOut
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return MediaMeta{}, fmt.Errorf("no se pudo leer la informacion del video: %w", err)
	}
	meta := MediaMeta{}
	for _, s := range parsed.Streams {
		switch s.CodecType {
		case "video":
			if meta.Width == 0 {
				meta.Width, meta.Height = s.Width, s.Height
				meta.FPS = parseFraction(s.RFrameRate)
				if d, err := strconv.ParseFloat(s.Duration, 64); err == nil && d > 0 {
					meta.DurationSec = d
				}
			}
		case "audio":
			meta.HasAudio = true
		}
	}
	if d, err := strconv.ParseFloat(parsed.Format.Duration, 64); err == nil && d > 0 {
		meta.DurationSec = d
	}
	if meta.Width == 0 || meta.DurationSec <= 0 {
		return MediaMeta{}, errors.New("el archivo no contiene un video reproducible")
	}
	meta.DurationSec = math.Round(meta.DurationSec*100) / 100
	meta.FPS = math.Round(meta.FPS*100) / 100
	meta.AspectRatio = nearestRatio(meta.Width, meta.Height)
	return meta, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func parseFraction(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	n, err1 := strconv.ParseFloat(num, 64)
	d, err2 := strconv.ParseFloat(den, 64)
	if err1 != nil || err2 != nil || d == 0 {
		return 0
	}
	return n / d
}

var knownRatios = []struct {
	label string
	value float64
}{
	{"9:16", 9.0 / 16}, {"16:9", 16.0 / 9}, {"1:1", 1}, {"4:5", 4.0 / 5},
	{"5:4", 5.0 / 4}, {"3:4", 3.0 / 4}, {"4:3", 4.0 / 3}, {"21:9", 21.0 / 9},
}

// nearestRatio maps pixel dimensions to the closest common aspect ratio.
func nearestRatio(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	r := float64(w) / float64(h)
	best, bestDiff := "", math.MaxFloat64
	for _, k := range knownRatios {
		if d := math.Abs(k.value - r); d < bestDiff {
			best, bestDiff = k.label, d
		}
	}
	return best
}

var ptsTime = regexp.MustCompile(`pts_time:([0-9]+(?:\.[0-9]+)?)`)

// parseSceneTimes pulls the scene-change timestamps out of showinfo output.
func parseSceneTimes(stderr string) []float64 {
	var cuts []float64
	for _, m := range ptsTime.FindAllStringSubmatch(stderr, -1) {
		if t, err := strconv.ParseFloat(m[1], 64); err == nil && t > 0.05 {
			cuts = append(cuts, math.Round(t*100)/100)
		}
		if len(cuts) >= maxCutsReported {
			break
		}
	}
	return cuts
}

// detectCuts measures scene changes with ffmpeg's scene score.
func detectCuts(ctx context.Context, path string) ([]float64, error) {
	ffmpeg, err := findBin("ffmpeg", "FFMPEG_PATH")
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	_, stderr, runErr := run(cctx, ffmpeg, "-hide_banner", "-nostats", "-i", path,
		"-vf", "select='gt(scene,"+sceneThreshold+")',showinfo", "-an", "-f", "null", "-")
	if runErr != nil && cctx.Err() != nil {
		return nil, errors.New("la deteccion de cortes tardo demasiado")
	}
	return parseSceneTimes(stderr), nil
}

// normalize re-encodes the clip small enough to send inline to the brain:
// short side <= 720 px, modest bitrate, mono AAC audio kept (music and voice
// matter for the analysis). The second attempt trades quality for size.
func normalize(ctx context.Context, in, out string, maxSeconds int) error {
	ffmpeg, err := findBin("ffmpeg", "FFMPEG_PATH")
	if err != nil {
		return err
	}
	attempts := []struct{ crf, rate, short string }{
		{"30", "1500k", "720"},
		{"35", "700k", "480"},
	}
	for _, a := range attempts {
		scale := fmt.Sprintf("scale='if(gt(iw,ih),-2,min(%[1]s,iw))':'if(gt(iw,ih),min(%[1]s,ih),-2)'", a.short)
		nctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		_, stderr, runErr := run(nctx, ffmpeg, "-y", "-hide_banner", "-loglevel", "error",
			"-i", in, "-t", strconv.Itoa(maxSeconds), "-vf", scale,
			"-c:v", "libx264", "-preset", "veryfast", "-crf", a.crf,
			"-maxrate", a.rate, "-bufsize", "3000k", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-b:a", "64k", "-ac", "1", "-movflags", "+faststart", out)
		cancel()
		if runErr != nil {
			return fmt.Errorf("no se pudo preparar el video: %s", firstLine(stderr))
		}
		if st, err := os.Stat(out); err == nil && st.Size() <= normalizedMaxBytes {
			return nil
		}
	}
	return errors.New("el video sigue siendo demasiado pesado tras comprimirlo: recortalo o usa uno mas corto")
}

// toMP3 converts any audio reference to a short mono mp3 the brain can hear.
func toMP3(ctx context.Context, in, out string) error {
	ffmpeg, err := findBin("ffmpeg", "FFMPEG_PATH")
	if err != nil {
		return err
	}
	actx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	_, stderr, runErr := run(actx, ffmpeg, "-y", "-hide_banner", "-loglevel", "error",
		"-i", in, "-t", "90", "-vn", "-ac", "1", "-b:a", "64k", out)
	if runErr != nil {
		return fmt.Errorf("no se pudo leer el audio: %s", firstLine(stderr))
	}
	return nil
}
