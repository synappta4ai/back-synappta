package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"synapta/internal/utils"
)

// mediaResponse is what the editor needs to lay a clip on the timeline.
type mediaResponse struct {
	Duration float64 `json:"duration"`
	HasAudio bool    `json:"has_audio"`
	HasVideo bool    `json:"has_video"`
	Poster   string  `json:"poster,omitempty"`
}

// sourceFromQuery reads file_id / src (same pair the render job accepts).
func sourceFromQuery(c *gin.Context) (Source, bool) {
	src := Source{
		FileID: strings.TrimSpace(c.Query("file_id")),
		Output: strings.TrimSpace(c.Query("src")),
	}
	if (src.FileID == "") == (src.Output == "") {
		utils.BadRequest(c, "envia file_id o src, no ambos")
		return Source{}, false
	}
	return src, true
}

// Media handles GET /render/media: measures a clip (and, for videos, produces
// a cached poster frame) so the browser never has to decode the file itself.
func (m *Module) Media(c *gin.Context) {
	files, ok := m.files(c)
	if !ok {
		return
	}
	src, ok := sourceFromQuery(c)
	if !ok {
		return
	}
	ffprobe, err := findBin("ffprobe", "FFPROBE_PATH")
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp("", "render-media-*")
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	defer os.RemoveAll(dir)
	downloads := 0
	path, err := m.resolveSource(ctx, files, src, dir, &downloads)
	if err != nil {
		utils.NotFound(c, err.Error())
		return
	}
	info, err := probe(ctx, ffprobe, path)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	res := mediaResponse{Duration: info.Duration, HasAudio: info.HasAudio, HasVideo: info.HasVideo}
	if info.HasVideo && !strings.HasSuffix(strings.ToLower(path), ".png") {
		res.Poster = m.poster(ctx, src, path, info.Duration)
	}
	utils.Success(c, res)
}

// Poster handles GET /render/poster: returns {url} of a frame cached under
// /outputs/posters (served by the static route, so the browser gets one cheap
// image instead of streaming the whole video for a thumbnail).
func (m *Module) Poster(c *gin.Context) {
	files, ok := m.files(c)
	if !ok {
		return
	}
	src, ok := sourceFromQuery(c)
	if !ok {
		return
	}
	key := src.FileID + "|" + src.Output
	url := m.cachedPoster(key)
	if url == "" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Minute)
		defer cancel()
		dir, err := os.MkdirTemp("", "render-poster-*")
		if err != nil {
			utils.InternalError(c, err.Error())
			return
		}
		defer os.RemoveAll(dir)
		downloads := 0
		path, err := m.resolveSource(ctx, files, src, dir, &downloads)
		if err != nil {
			utils.NotFound(c, err.Error())
			return
		}
		url = m.poster(ctx, src, path, 0)
	}
	if url == "" {
		utils.InternalError(c, "no se pudo generar la miniatura")
		return
	}
	utils.Success(c, gin.H{"url": url})
}

// poster extracts one frame and stores it under <outputs>/posters. It returns
// the public /outputs path, or "" when ffmpeg is unavailable or fails.
func (m *Module) poster(ctx context.Context, src Source, path string, duration float64) string {
	key := src.FileID + "|" + src.Output
	if url := m.cachedPoster(key); url != "" {
		return url
	}
	ffmpeg, err := findBin("ffmpeg", "FFMPEG_PATH")
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:12]) + ".jpg"
	outDir := filepath.Join(m.outputsDir, "posters")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return ""
	}
	dst := filepath.Join(outDir, name)
	seek := posterSeekAt(duration)
	args := []string{"-y", "-loglevel", "error", "-ss", seek, "-i", path,
		"-frames:v", "1", "-vf", "scale=480:-2", "-q:v", "4", dst}
	if err := runFFmpeg(ctx, ffmpeg, "", args, 0, func(float64) {}); err != nil {
		return ""
	}
	if _, err := os.Stat(dst); err != nil {
		return ""
	}
	return "/" + strings.TrimPrefix(filepath.ToSlash(filepath.Join("outputs", "posters", name)), "/")
}

// cachedPoster returns the /outputs URL when the poster already exists.
func (m *Module) cachedPoster(key string) string {
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:12]) + ".jpg"
	if _, err := os.Stat(filepath.Join(m.outputsDir, "posters", name)); err != nil {
		return ""
	}
	return "/outputs/posters/" + name
}

func posterSeekAt(duration float64) string {
	s := 1.0
	if duration > 0 && duration < 2 {
		s = duration / 2
	}
	return strconv.FormatFloat(s, 'f', 2, 64)
}
