// Package infoproduct powers the VideoMachine "Infoproducto" agent: it
// breaks a reference video down with a multimodal brain (analyze) and writes
// the complete generation prompt for the user's product (compose).
//
// The reference video is never stored: it is uploaded to temp storage or
// downloaded from a public URL into a temp dir, compressed, sent to the
// brain and deleted. Only the structured breakdown is kept (video_analyses).
package infoproduct

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"synapta/internal/modules/credential"
	"synapta/internal/modules/file"
	"synapta/internal/utils"
)

//go:embed prompts/analyze.md
var analyzeSystem string

//go:embed prompts/compose.md
var composeSystem string

var errEmptyPrompt = errors.New("el cerebro no devolvio un prompt")

func itoa(n int) string { return strconv.Itoa(n) }

const (
	defaultMaxSeconds = 90
	maxConcurrent     = 2
	maxImages         = 8
	maxImageBytes     = 5 << 20
	maxAudios         = 2
	maxVideos         = 2
)

// CredentialResolver, FilesResolver and PoolResolver supply the tenant-scoped
// services per request (same pattern as the other modules).
type (
	CredentialResolver func(c *gin.Context) (*credential.Handler, bool)
	FilesResolver      func(c *gin.Context) (*file.Handler, bool)
	PoolResolver       func(c *gin.Context) (*sql.DB, bool)
)

// Module registers the /infoproduct routes.
type Module struct {
	creds      CredentialResolver
	files      FilesResolver
	pool       PoolResolver
	brain      *brain
	sem        chan struct{}
	maxSeconds int
}

// NewModule builds the module. INFOPRODUCT_MAX_SECONDS caps the reference
// video length (default 90 s).
func NewModule(creds CredentialResolver, files FilesResolver, pool PoolResolver) *Module {
	maxSeconds := defaultMaxSeconds
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("INFOPRODUCT_MAX_SECONDS"))); err == nil && v > 0 {
		maxSeconds = v
	}
	return &Module{
		creds: creds, files: files, pool: pool,
		brain: newBrain(), sem: make(chan struct{}, maxConcurrent), maxSeconds: maxSeconds,
	}
}

// Name implements modules.Module.
func (m *Module) Name() string { return "infoproduct" }

// Register implements modules.Module.
func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, _ gin.HandlerFunc) {
	g := rg.Group("/infoproduct")
	g.Use(authMw, tenantMw)
	{
		g.POST("/analyze", m.Analyze)
		g.POST("/compose", m.Compose)
		g.GET("/analyses/:id", m.GetAnalysis)
	}
}

type analyzeRequest struct {
	// Exactly one of FileID (uploaded video) or URL (public page/video link).
	FileID string `json:"file_id"`
	URL    string `json:"url"`
	// Model is the brain (a catalog name such as openrouter/google/...).
	Model string `json:"model"`
	// Force re-analyzes even when a cached breakdown exists.
	Force bool `json:"force"`
}

// brainAccess resolves the tenant's OpenRouter credential.
func (m *Module) brainAccess(c *gin.Context) (baseURL, key string, ok bool) {
	hdl, ok := m.creds(c)
	if !ok {
		return "", "", false
	}
	cred, err := hdl.Resolve(credential.ProviderOpenRouter)
	if err != nil {
		utils.InternalError(c, err.Error())
		return "", "", false
	}
	if cred == nil || cred.AuthKey() == "" {
		utils.BadRequest(c, "falta la API key de OpenRouter: guardala en las credenciales para usar el cerebro")
		return "", "", false
	}
	return cred.BaseURL, cred.AuthKey(), true
}

func (m *Module) store(c *gin.Context) (*store, bool) {
	db, ok := m.pool(c)
	if !ok || db == nil {
		utils.InternalError(c, "tenant database unavailable")
		return nil, false
	}
	return &store{db: db}, true
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// staleForSkill reports a cached breakdown written by the previous analyzer
// (no dcs_analyze block). With the skill enabled it is ignored, so the user
// gets the new breakdown instead of an old cached one.
func staleForSkill(sourceKey string, cached *Analysis) bool {
	if !analyzeSkillEnabled() || cached == nil || len(cached.Breakdown) == 0 {
		return false
	}
	var probe struct {
		SchemaVersion string          `json:"schema_version"`
		DCSAnalyze    json.RawMessage `json:"dcs_analyze"`
	}
	if err := json.Unmarshal(cached.Breakdown, &probe); err != nil {
		return true
	}
	if len(probe.DCSAnalyze) == 0 {
		log.Printf("[infoproduct] analyze: el analisis guardado (%s) no viene del skill: se re-analiza", sourceKey)
		return true
	}
	if missing := missingAnalyzeSections(probe.DCSAnalyze); len(missing) > 0 {
		log.Printf("[infoproduct] analyze: el analisis guardado (%s) esta incompleto (faltan %s): se re-analiza",
			sourceKey, strings.Join(missing, ", "))
		return true
	}
	if version := analysisPipelineVersion(probe.DCSAnalyze); version < analyzePipelineVersion {
		log.Printf("[infoproduct] analyze: el analisis guardado (%s) viene del pipeline v%d (actual v%d): se re-analiza",
			sourceKey, version, analyzePipelineVersion)
		return true
	}
	return false
}

// Analyze handles POST /infoproduct/analyze.
func (m *Module) Analyze(c *gin.Context) {
	var req analyzeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	req.FileID, req.URL = strings.TrimSpace(req.FileID), strings.TrimSpace(req.URL)
	if (req.FileID == "") == (req.URL == "") {
		utils.BadRequest(c, "envia el video como archivo (file_id) o como URL, no ambos")
		return
	}
	st, ok := m.store(c)
	if !ok {
		return
	}
	files, ok := m.files(c)
	if !ok {
		return
	}

	// done borra el video subido (si era temporal) cuando ya cumplio su papel.
	done := func() {
		if req.FileID != "" {
			files.DeleteIfTemp(req.FileID)
		}
	}

	// Cache key from what we already know, before any heavy work.
	var sourceKey, sourceURL string
	if req.URL != "" {
		if _, err := validateSourceURL(req.URL); err != nil {
			utils.BadRequest(c, err.Error())
			return
		}
		sourceKey, sourceURL = "url:"+hashString(req.URL), req.URL
	} else {
		info, err := files.Info(req.FileID)
		if err != nil || info == nil {
			utils.NotFound(c, "archivo no encontrado")
			return
		}
		if !strings.HasPrefix(info.MimeType, "video/") && info.Category != "videos" && info.Category != "temp" {
			utils.BadRequest(c, "el archivo no es un video")
			return
		}
		if info.SHA256 != "" {
			sourceKey = "file:" + info.SHA256
		}
	}
	if sourceKey != "" && !req.Force {
		if cached, err := st.byKey(sourceKey); err == nil && cached != nil && !staleForSkill(sourceKey, cached) {
			cached.Cached = true
			done()
			utils.Success(c, cached)
			return
		}
	}

	baseURL, apiKey, ok := m.brainAccess(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-ctx.Done():
		return
	}

	workDir, err := os.MkdirTemp("", "infoproduct-*")
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	defer os.RemoveAll(workDir)

	source, meta, err := m.prepareSource(ctx, workDir, files, req)
	if err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	if sourceKey == "" {
		h, herr := hashFile(source)
		if herr != nil {
			utils.InternalError(c, herr.Error())
			return
		}
		sourceKey = "file:" + h
		if !req.Force {
			if cached, err := st.byKey(sourceKey); err == nil && cached != nil && !staleForSkill(sourceKey, cached) {
				cached.Cached = true
				done()
				utils.Success(c, cached)
				return
			}
		}
	}

	normalized := filepath.Join(workDir, "normalized.mp4")
	if err := normalize(ctx, source, normalized, m.maxSeconds); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	data, err := os.ReadFile(normalized)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}

	metaJSON, _ := json.Marshal(meta)
	videoPayload := "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(data)

	// Step 1 runs the DCS-ANALYZE skill by default; any failure falls back to
	// the previous analyzer (analyze.md) without breaking the response.
	var answer json.RawMessage
	var u usage
	if analyzeSkillEnabled() {
		// Resolve env/default here so the stored row records the model used.
		req.Model = analyzeModel(req.Model)
		merged, skillUsage, serr := m.analyzeWithSkill(ctx, baseURL, apiKey, req.Model, meta, data)
		if serr != nil {
			log.Printf("[infoproduct] skill DCS-ANALYZE no disponible (%v): se usa el analizador anterior", serr)
		} else {
			answer, u = merged, skillUsage
		}
	}
	if len(answer) == 0 {
		intro := "MEASURED DATA (ffprobe/ffmpeg, ground truth):\n" + string(metaJSON) +
			"\n\nWatch the attached video and listen to its audio, then reply with the JSON object described in the system prompt."
		parts := []map[string]any{textPart(intro), videoPart(videoPayload)}
		legacyAnswer, legacyUsage, lerr := m.brain.jsonAnswer(ctx, baseURL, apiKey, req.Model, analyzeSystem, parts, analyzeLegacyMaxTokens)
		if lerr != nil {
			utils.Error(c, http.StatusBadGateway, lerr.Error())
			return
		}
		answer, u = legacyAnswer, legacyUsage
	}
	var head struct {
		Summary string `json:"summary"`
	}
	_ = json.Unmarshal(answer, &head)

	saved, err := st.save(&Analysis{
		SourceKey: sourceKey, SourceURL: sourceURL, Model: normalizeModel(req.Model),
		Meta: metaJSON, Breakdown: answer, Summary: head.Summary,
	}, utils.UserIDFromContext(c))
	if err != nil || saved == nil {
		utils.InternalError(c, fmt.Sprintf("no se pudo guardar el analisis: %v", err))
		return
	}
	saved.Usage = &u
	done()
	utils.Success(c, saved)
}

// prepareSource materializes the reference video in workDir and measures it.
func (m *Module) prepareSource(ctx context.Context, workDir string, files *file.Handler, req analyzeRequest) (string, MediaMeta, error) {
	var path, kind string
	if req.URL != "" {
		p, err := downloadURL(ctx, req.URL, workDir)
		if err != nil {
			return "", MediaMeta{}, err
		}
		path, kind = p, "url"
	} else {
		p, err := files.LocalPath(req.FileID)
		if err != nil {
			return "", MediaMeta{}, fmt.Errorf("no se pudo leer el archivo: %w", err)
		}
		path, kind = p, "file"
	}
	meta, err := probe(ctx, path)
	if err != nil {
		return "", MediaMeta{}, err
	}
	if meta.DurationSec > float64(m.maxSeconds)+0.5 {
		return "", MediaMeta{}, fmt.Errorf("el video dura %.0f s y el maximo es %d s: recortalo e intenta de nuevo", meta.DurationSec, m.maxSeconds)
	}
	cuts, err := detectCuts(ctx, path)
	if err != nil {
		return "", MediaMeta{}, err
	}
	meta.Cuts, meta.CutsCount, meta.Source = cuts, len(cuts)+1, kind
	return path, meta, nil
}

// GetAnalysis handles GET /infoproduct/analyses/:id.
func (m *Module) GetAnalysis(c *gin.Context) {
	st, ok := m.store(c)
	if !ok {
		return
	}
	a, err := st.byID(c.Param("id"))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if a == nil {
		utils.NotFound(c, "analisis no encontrado")
		return
	}
	utils.Success(c, a)
}

type composeRequest struct {
	// Breakdown is the (possibly user-reviewed) analysis breakdown JSON.
	Breakdown json.RawMessage `json:"breakdown"`
	// FormatID selects a preset of the "formats" group (optional).
	FormatID string `json:"format_id"`
	// Model is the brain; Target the video model limits.
	Model        string        `json:"model"`
	Target       ComposeTarget `json:"target"`
	TargetModel  string        `json:"target_model"`
	ImageFileIDs []string      `json:"image_file_ids"`
	AudioFileIDs []string      `json:"audio_file_ids"`
	// VideoFileIDs son clips de referencia (modelos con reference video).
	VideoFileIDs []string `json:"video_file_ids"`
	// ImageRoles maps file ids to person|product for the recreate skill.
	ImageRoles map[string]string `json:"image_roles"`
	// FPS and AspectRatio describe the reference video (optional).
	FPS         float64 `json:"fps"`
	AspectRatio string  `json:"aspect_ratio"`
	// Notes are the user's corrections and extra instructions.
	Notes string `json:"notes"`
}

// Compose handles POST /infoproduct/compose.
func (m *Module) Compose(c *gin.Context) {
	var req composeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	if len(req.Breakdown) == 0 || !json.Valid(req.Breakdown) {
		utils.BadRequest(c, "falta el desglose del video (breakdown)")
		return
	}
	if strings.TrimSpace(req.Target.Model) == "" {
		utils.BadRequest(c, "elige el modelo de video (target.model)")
		return
	}
	if len(req.ImageFileIDs) == 0 {
		utils.BadRequest(c, "adjunta al menos una imagen del producto")
		return
	}
	if len(req.ImageFileIDs) > maxImages {
		utils.BadRequest(c, fmt.Sprintf("maximo %d imagenes del producto", maxImages))
		return
	}
	if len(req.AudioFileIDs) > maxAudios {
		utils.BadRequest(c, fmt.Sprintf("maximo %d audios de referencia", maxAudios))
		return
	}
	if len(req.VideoFileIDs) > maxVideos {
		utils.BadRequest(c, fmt.Sprintf("maximo %d videos de referencia", maxVideos))
		return
	}
	files, ok := m.files(c)
	if !ok {
		return
	}
	baseURL, apiKey, ok := m.brainAccess(c)
	if !ok {
		return
	}

	var ref struct {
		Duration float64 `json:"duration_sec"`
		Ratio    string  `json:"aspect_ratio"`
	}
	_ = json.Unmarshal(req.Breakdown, &ref)
	n := segmentsRequired(ref.Duration, req.Target.MaxDuration)

	var formatLabel, formatMD string
	if strings.TrimSpace(req.FormatID) != "" {
		st, ok := m.store(c)
		if !ok {
			return
		}
		label, md, err := st.formatPrompt(strings.TrimSpace(req.FormatID))
		if err != nil {
			utils.InternalError(c, err.Error())
			return
		}
		if md == "" {
			utils.BadRequest(c, "el formato elegido no existe o esta inactivo")
			return
		}
		formatLabel, formatMD = label, md
	}

	workDir, err := os.MkdirTemp("", "infoproduct-compose-*")
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	defer os.RemoveAll(workDir)

	// The catalog only knows max_references for models that declare it; the
	// reference-to-video routes don't publish a number, so tell the brain how
	// many images are actually riding along (otherwise it reads 0 and avoids
	// tagging them in the prompt).
	if req.Target.MaxReferences <= 0 {
		req.Target.MaxReferences = len(req.ImageFileIDs)
	}
	targetJSON, _ := json.Marshal(req.Target)
	var legacy strings.Builder
	legacy.WriteString("REFERENCE BREAKDOWN (JSON):\n" + string(req.Breakdown))
	legacy.WriteString("\n\nTARGET MODEL CONSTRAINTS (JSON):\n" + string(targetJSON))
	fmt.Fprintf(&legacy, "\n\nreference_duration_sec: %.1f\nsegments_required: %d", ref.Duration, n)
	if formatMD != "" {
		legacy.WriteString("\n\nFORMAT GUIDE (" + formatLabel + "):\n" + formatMD)
	}
	if notes := strings.TrimSpace(req.Notes); notes != "" {
		legacy.WriteString("\n\nUSER NOTES:\n" + notes)
	}
	fmt.Fprintf(&legacy, "\n\nProduct images attached: %d (labeled @Image1..). Audio references attached: %d.", len(req.ImageFileIDs), len(req.AudioFileIDs))

	mediaParts := make([]map[string]any, 0, 2*(len(req.ImageFileIDs)+len(req.AudioFileIDs)))
	imageNames := make([]string, 0, len(req.ImageFileIDs))
	audioNames := make([]string, 0, len(req.AudioFileIDs))
	var warnings []string
	for i, id := range req.ImageFileIDs {
		info, err := files.Info(id)
		if err != nil || info == nil {
			utils.NotFound(c, "imagen no encontrada: "+id)
			return
		}
		if !strings.HasPrefix(info.MimeType, "image/") {
			utils.BadRequest(c, fmt.Sprintf("%s no es una imagen", info.Filename))
			return
		}
		imageNames = append(imageNames, info.Filename)
		path, err := files.LocalPath(id)
		if err != nil {
			utils.InternalError(c, err.Error())
			return
		}
		st, err := os.Stat(path)
		if err != nil {
			utils.InternalError(c, err.Error())
			return
		}
		if st.Size() > maxImageBytes {
			warnings = append(warnings, fmt.Sprintf("La imagen %s pesa mucho y el cerebro no la vio; se usara igual como ingrediente.", info.Filename))
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			utils.InternalError(c, err.Error())
			return
		}
		mediaParts = append(mediaParts, textPart(fmt.Sprintf("@Image%d (%s):", i+1, info.Filename)),
			imagePart("data:"+info.MimeType+";base64,"+base64.StdEncoding.EncodeToString(data)))
	}
	for i, id := range req.AudioFileIDs {
		info, err := files.Info(id)
		if err != nil || info == nil {
			utils.NotFound(c, "audio no encontrado: "+id)
			return
		}
		if !strings.HasPrefix(info.MimeType, "audio/") && info.Category != "audio" {
			utils.BadRequest(c, fmt.Sprintf("%s no es un audio", info.Filename))
			return
		}
		audioNames = append(audioNames, info.Filename)
		src, err := files.LocalPath(id)
		if err != nil {
			utils.InternalError(c, err.Error())
			return
		}
		out := filepath.Join(workDir, fmt.Sprintf("audio%d.mp3", i+1))
		if err := toMP3(c.Request.Context(), src, out); err != nil {
			utils.BadRequest(c, err.Error())
			return
		}
		data, err := os.ReadFile(out)
		if err != nil {
			utils.InternalError(c, err.Error())
			return
		}
		mediaParts = append(mediaParts, textPart(fmt.Sprintf("@Audio%d (%s):", i+1, info.Filename)),
			audioPart(base64.StdEncoding.EncodeToString(data), "mp3"))
	}

	// Referencia en video: solo se declara en el manifesto y en el prompt (no se
	// reenvian los bytes al cerebro; el desglose ya describe el clip).
	videoNames := make([]string, 0, len(req.VideoFileIDs))
	for _, id := range req.VideoFileIDs {
		info, err := files.Info(id)
		if err != nil || info == nil {
			utils.NotFound(c, "video de referencia no encontrado: "+id)
			return
		}
		if !strings.HasPrefix(info.MimeType, "video/") {
			utils.BadRequest(c, fmt.Sprintf("%s no es un video", info.Filename))
			return
		}
		videoNames = append(videoNames, info.Filename)
	}

	images := make([]recreateImage, 0, len(req.ImageFileIDs))
	for i, id := range req.ImageFileIDs {
		name := ""
		if i < len(imageNames) {
			name = imageNames[i]
		}
		images = append(images, recreateImage{FileID: id, Role: req.ImageRoles[id], Name: name})
	}
	videos := make([]recreateVideo, 0, len(req.VideoFileIDs))
	for i, id := range req.VideoFileIDs {
		name := ""
		if i < len(videoNames) {
			name = videoNames[i]
		}
		videos = append(videos, recreateVideo{FileID: id, Name: name})
	}
	audios := make([]recreateAudio, 0, len(req.AudioFileIDs))
	for i, id := range req.AudioFileIDs {
		name := ""
		if i < len(audioNames) {
			name = audioNames[i]
		}
		audios = append(audios, recreateAudio{FileID: id, Name: name})
	}

	if recreateSkillEnabled() {
		if system, serr := LoadRecreateSystemPrompt(); serr != nil {
			log.Printf("[infoproduct] recreate skill no disponible (%v): se usa el flujo anterior", serr)
		} else {
			in := recreateInput{
				Breakdown:     req.Breakdown,
				Images:        images,
				Audios:        audios,
				Videos:        videos,
				Notes:         req.Notes,
				AspectRatio:   firstNonEmpty(req.AspectRatio, ref.Ratio),
				FPS:           req.FPS,
				TotalDuration: ref.Duration,
				MaxDuration:   req.Target.MaxDuration,
				TargetModel:   firstNonEmpty(req.TargetModel, req.Target.Model),
			}
			userMsg, berr := buildRecreateUserMessage(in)
			if berr != nil {
				log.Printf("[infoproduct] no se pudo armar el mensaje de recreacion (%v): se usa el flujo anterior", berr)
			} else {
				parts := append([]map[string]any{textPart(userMsg)}, mediaParts...)
				answer, u, cerr := m.brain.chatWith(c.Request.Context(), baseURL, apiKey, recreateModel(req.Model), system, parts, recreateMaxTokens, recreateTemperature)
				if cerr != nil {
					utils.Error(c, http.StatusBadGateway, cerr.Error())
					return
				}
				prompt, perr := cleanRecreatePrompt(answer)
				if perr != nil {
					utils.Error(c, http.StatusBadGateway, perr.Error())
					return
				}
				if looksLikeQuestion(prompt) {
					log.Printf("[infoproduct] recreate devolvio una pregunta: se reintenta una vez")
					retrySystem := system + "\n\nREMINDER: your previous reply asked the user a question. This is an automated call with no user present. Do not ask anything; write the final English prompt now."
					if retry, ru, rerr := m.brain.chatWith(c.Request.Context(), baseURL, apiKey, recreateModel(req.Model), retrySystem, parts, recreateMaxTokens, recreateTemperature); rerr == nil {
						if fixed, ferr := cleanRecreatePrompt(retry); ferr == nil && !looksLikeQuestion(fixed) {
							prompt = fixed
							u.PromptTokens += ru.PromptTokens
							u.CompletionTokens += ru.CompletionTokens
							u.CostUSD += ru.CostUSD
						}
					}
				}
				if looksLikeQuestion(prompt) {
					utils.Error(c, http.StatusBadGateway, "el cerebro no devolvio un prompt de video; intenta de nuevo")
					return
				}
				vp, verr := buildRecreateVideoPrompt(prompt, in, req.Target, ref.Duration, ref.Ratio, n)
				if verr != nil {
					utils.Error(c, http.StatusBadGateway, verr.Error())
					return
				}
				vp.Warnings = append(vp.Warnings, warnings...)
				utils.Success(c, gin.H{"video_prompt": vp, "segments_required": n, "usage": u})
				return
			}
		}
	}

	parts := append([]map[string]any{textPart(legacy.String())}, mediaParts...)
	answer, u, err := m.brain.jsonAnswer(c.Request.Context(), baseURL, apiKey, req.Model, composeSystem, parts, 8192)
	if err != nil {
		utils.Error(c, http.StatusBadGateway, err.Error())
		return
	}
	vp, err := buildVideoPrompt(answer, req.Target, req.ImageFileIDs, req.AudioFileIDs, ref.Duration, ref.Ratio, n)
	if err != nil {
		utils.Error(c, http.StatusBadGateway, err.Error())
		return
	}
	vp.Warnings = append(vp.Warnings, warnings...)
	utils.Success(c, gin.H{"video_prompt": vp, "segments_required": n, "usage": u})
}
