package render

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"synapta/config"
	"synapta/internal/modules/file"
	"synapta/internal/tenancy"
)

// runJob is the goroutine body of one render. It never lets a panic escape:
// the job is marked failed and the process keeps serving everything else.
func (m *Module) runJob(st *store, files *file.Handler, jobID string, p *Project, format string, keepInLibrary bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[render] job %s panic: %v\n%s", jobID, r, debug.Stack())
			st.markFailed(jobID, fmt.Sprintf("error interno del render: %v", r))
		}
	}()

	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-m.ctx.Done():
		st.markFailed(jobID, "el servidor se esta deteniendo")
		return
	}
	if err := st.markRunning(jobID); err != nil {
		log.Printf("[render] job %s markRunning: %v", jobID, err)
	}
	ctx, cancel := context.WithTimeout(m.ctx, m.timeout)
	defer cancel()

	fileID, outURL, tempPath, err := m.render(ctx, st, files, jobID, p, format, keepInLibrary)
	if err != nil {
		log.Printf("[render] job %s failed: %v", jobID, err)
		st.markFailed(jobID, err.Error())
		return
	}
	if keepInLibrary {
		err = st.markSucceeded(jobID, fileID, outURL)
	} else {
		err = st.markSucceededTemp(jobID, tempPath, "/api/v1/render/jobs/"+jobID+"/download")
	}
	if err != nil {
		log.Printf("[render] job %s markSucceeded: %v", jobID, err)
	}
}

// render resolves the inputs, runs ffmpeg and registers the MP4 in the library.
func (m *Module) render(ctx context.Context, st *store, files *file.Handler, jobID string, p *Project, format string, keepInLibrary bool) (fileID, outURL, tempPath string, err error) {
	ffmpeg, err := findBin("ffmpeg", "FFMPEG_PATH")
	if err != nil {
		return "", "", "", err
	}
	ffprobe, err := findBin("ffprobe", "FFPROBE_PATH")
	if err != nil {
		return "", "", "", err
	}
	dir, err := os.MkdirTemp("", "render-*")
	if err != nil {
		return "", "", "", err
	}
	defer os.RemoveAll(dir)

	// Sources used by several clips (e.g. a split video) are located once.
	paths := map[string]string{}
	downloads := 0
	locate := func(s Source) (resolved, error) {
		key := s.FileID + "|" + s.Output
		path, ok := paths[key]
		if !ok {
			var err error
			path, err = m.resolveSource(ctx, files, s, dir, &downloads)
			if err != nil {
				return resolved{}, err
			}
			paths[key] = path
		}
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		info, err := probe(pctx, ffprobe, path)
		if err != nil {
			return resolved{}, err
		}
		return resolved{Path: path, Info: *info}, nil
	}
	vin := make([]resolved, len(p.Video))
	for i, c := range p.Video {
		if vin[i], err = locate(c.Source); err != nil {
			return "", "", "", fmt.Errorf("clip de video %d: %w", i+1, err)
		}
	}
	ain := make([]resolved, len(p.Audio))
	for i, a := range p.Audio {
		if ain[i], err = locate(a.Source); err != nil {
			return "", "", "", fmt.Errorf("clip de audio %d: %w", i+1, err)
		}
	}

	fontName := ""
	if len(p.Texts) > 0 {
		if src := findFont(); src != "" {
			fontName = "font" + strings.ToLower(filepath.Ext(src))
			if err := copyFile(src, filepath.Join(dir, fontName)); err != nil {
				return "", "", "", fmt.Errorf("fuente de texto: %w", err)
			}
		}
	}

	pl, err := buildPlan(p, vin, ain, fontName, m.threads, format)
	if err != nil {
		return "", "", "", err
	}
	if pl.TotalSec > m.maxSeconds {
		return "", "", "", fmt.Errorf("el video dura %.0f s y el maximo es %.0f s", pl.TotalSec, m.maxSeconds)
	}
	for name, text := range pl.TextFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			return "", "", "", err
		}
	}

	last := -1
	err = runFFmpeg(ctx, ffmpeg, dir, pl.Args, pl.TotalSec, func(frac float64) {
		pct := int(frac * 99) // 100 is reserved for "registered in the library"
		if pct >= last+2 {
			last = pct
			st.setProgress(jobID, pct)
		}
	})
	if err != nil {
		return "", "", "", err
	}

	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = "Render " + time.Now().Format("2006-01-02 15-04")
	}
	if keepInLibrary {
		res, err := files.RegisterLocalFile(filepath.Join(dir, pl.OutputName), name+"."+formatOf(format).Ext, "videos")
		if err != nil {
			return "", "", "", fmt.Errorf("no se pudo guardar el video renderizado: %w", err)
		}
		return res.ID, res.URL, "", nil
	}
	// Download-only: the file waits for the user in a temp folder and is
	// deleted as soon as it is delivered.
	tmpDir := filepath.Join(m.tempDir, jobID)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", "", "", err
	}
	tmpPath := filepath.Join(tmpDir, "export."+formatOf(format).Ext)
	// Copia (no rename): el directorio temporal puede estar en otro disco.
	if err := copyFile(filepath.Join(dir, pl.OutputName), tmpPath); err != nil {
		return "", "", "", err
	}
	return "", "", tmpPath, nil
}

// resolveSource locates a clip's media on local disk (downloading it from the
// tenant bucket into dir when it only lives there).
func (m *Module) resolveSource(ctx context.Context, files *file.Handler, s Source, dir string, downloads *int) (string, error) {
	if id := strings.TrimSpace(s.FileID); id != "" {
		f, err := files.Info(id)
		if err != nil || f == nil {
			return "", fmt.Errorf("el archivo ya no existe en la biblioteca")
		}
		if f.Trashed {
			return "", fmt.Errorf("el archivo esta en la papelera")
		}
		path, err := files.LocalPath(id)
		if err != nil {
			return "", err
		}
		// ffmpeg runs inside a temp working dir: relative library paths
		// (UPLOAD_DIR is ./uploads by default) must be made absolute here.
		return filepath.Abs(path)
	}
	rel, local, err := parseOutputPath(s.Output)
	if err != nil {
		return "", err
	}
	if local {
		if p, err := resolveOutput(m.outputsDir, s.Output); err == nil {
			return p, nil
		}
	}
	// Not on local disk: published artifacts live in the tenant bucket.
	*downloads++
	dst := filepath.Join(dir, fmt.Sprintf("in%d%s", *downloads, strings.ToLower(filepath.Ext(rel))))
	if err := files.FetchOutput(ctx, rel, dst); err != nil {
		log.Printf("[render] fetch output %q: %v", rel, err)
		return "", fmt.Errorf("el archivo generado ya no esta disponible en el servidor")
	}
	return dst, nil
}

// parseOutputPath extracts the outputs-relative path of a generated artifact
// from either of its URL forms: /outputs/<rel> (local disk) or
// /api/v1/t/<slug>/outputs/<rel> (tenant bucket). The slug is ignored: the
// bucket read always goes through the caller's own tenant.
func parseOutputPath(raw string) (rel string, local bool, err error) {
	p := strings.TrimSpace(raw)
	if u, perr := url.Parse(p); perr == nil && u.Path != "" {
		p = u.Path
	}
	if r, ok := strings.CutPrefix(p, config.OutPutUrl()+"/"); ok {
		rel, local = r, true
	} else if r, ok := strings.CutPrefix(p, "/api/v1/t/"); ok {
		_, after, found := strings.Cut(r, "/outputs/")
		if !found {
			return "", false, fmt.Errorf("la fuente no es un archivo generado")
		}
		rel = after
	} else {
		return "", false, fmt.Errorf("la fuente no es un archivo generado")
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." || seg == "" || strings.ContainsAny(seg, "\\\x00") {
			return "", false, fmt.Errorf("ruta de fuente invalida")
		}
	}
	return rel, local, nil
}

// resolveOutput maps an /outputs/... path (or a full URL to one) to a file
// inside outputsDir, rejecting anything that escapes it.
func resolveOutput(outputsDir, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	p := raw
	if u, err := url.Parse(raw); err == nil && u.Path != "" {
		p = u.Path
	}
	prefix := config.OutPutUrl() + "/"
	if !strings.HasPrefix(p, prefix) {
		return "", fmt.Errorf("la fuente no es un archivo generado local")
	}
	rel := strings.TrimPrefix(p, prefix)
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." || seg == "" {
			return "", fmt.Errorf("ruta de fuente invalida")
		}
	}
	base, err := filepath.Abs(outputsDir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(base, filepath.FromSlash(rel))
	if r, err := filepath.Rel(base, full); err != nil || strings.HasPrefix(r, "..") {
		return "", fmt.Errorf("ruta de fuente invalida")
	}
	if st, err := os.Stat(full); err != nil || st.IsDir() {
		return "", fmt.Errorf("el archivo generado ya no esta en el servidor (puede estar en un bucket): agregalo desde la biblioteca")
	}
	return full, nil
}

// findFont picks a TTF for drawtext: RENDER_FONT_FILE, then common system fonts.
func findFont() string {
	if p := strings.TrimSpace(os.Getenv("RENDER_FONT_FILE")); p != "" {
		return p
	}
	candidates := []string{
		`C:\Windows\Fonts\arial.ttf`,
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		"/usr/share/fonts/dejavu/DejaVuSans.ttf",
		"/usr/share/fonts/TTF/DejaVuSans.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf",
		"/Library/Fonts/Arial.ttf",
		"/System/Library/Fonts/Supplemental/Arial.ttf",
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ─── Orphan reconciliation ──────────────────────────────────────

// ReconcileLoop fails jobs left unfinished by a restart (once, right after
// boot) and then jobs that stopped reporting progress. It recovers from
// panics and stops when stop is closed.
func (m *Module) ReconcileLoop(reg *tenancy.Registry, stop <-chan struct{}) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[render] reconcile panic: %v", r)
			}
		}()
		sweep := func(maxAge time.Duration, includeQueued bool) {
			tenants, err := reg.ListActiveTenants()
			if err != nil {
				log.Printf("[render] reconcile list tenants: %v", err)
				return
			}
			for i := range tenants {
				pool, err := reg.Pool(tenants[i].Slug)
				if err != nil {
					continue
				}
				failStaleSafe(pool, tenants[i].Slug, maxAge, includeQueued)
				purgeTempRenders(pool, tenants[i].Slug)
			}
		}
		sweep(0, true)
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				sweep(m.timeout+5*time.Minute, false)
			}
		}
	}()
}

// purgeTempRenders removes download-only renders nobody picked up.
func purgeTempRenders(pool *sql.DB, slug string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[render] purge %q panic: %v", slug, r)
		}
	}()
	paths, err := (&store{db: pool}).staleTempOutputs(6 * time.Hour)
	if err != nil {
		log.Printf("[render] purge %q: %v", slug, err)
		return
	}
	for _, p := range paths {
		_ = os.Remove(p)
		_ = os.Remove(filepath.Dir(p))
	}
}

func failStaleSafe(pool *sql.DB, slug string, maxAge time.Duration, includeQueued bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[render] reconcile tenant %q panic: %v", slug, r)
		}
	}()
	n, err := (&store{db: pool}).failStale(maxAge, includeQueued)
	if err != nil {
		log.Printf("[render] reconcile tenant %q: %v", slug, err)
		return
	}
	if n > 0 {
		log.Printf("[render] tenant %q: %d orphaned render job(s) marked failed", slug, n)
	}
}
