package infoproduct

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Analysis is a stored breakdown of a reference video.
type Analysis struct {
	ID        string          `json:"id"`
	SourceKey string          `json:"-"`
	SourceURL string          `json:"source_url"`
	Model     string          `json:"model"`
	Meta      json.RawMessage `json:"meta"`
	Breakdown json.RawMessage `json:"breakdown"`
	Summary   string          `json:"summary"`
	CreatedAt time.Time       `json:"created_at"`
	Cached    bool            `json:"cached"`
	Usage     *usage          `json:"usage,omitempty"`
}

// store persists analyses in the tenant schema (video_analyses).
type store struct{ db *sql.DB }

const analysisCols = `id, source_key, source_url, model, meta, breakdown, summary, created_at`

func scanAnalysis(row interface{ Scan(...any) error }) (*Analysis, error) {
	a := &Analysis{}
	var meta, breakdown []byte
	if err := row.Scan(&a.ID, &a.SourceKey, &a.SourceURL, &a.Model, &meta, &breakdown, &a.Summary, &a.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	a.Meta, a.Breakdown = meta, breakdown
	return a, nil
}

func (s *store) byKey(key string) (*Analysis, error) {
	return scanAnalysis(s.db.QueryRow(`SELECT `+analysisCols+` FROM video_analyses WHERE source_key = $1`, key))
}

func (s *store) byID(id string) (*Analysis, error) {
	return scanAnalysis(s.db.QueryRow(`SELECT `+analysisCols+` FROM video_analyses WHERE id::text = $1`, id))
}

// save inserts the analysis or refreshes the one stored for the same source.
func (s *store) save(a *Analysis, userID int64) (*Analysis, error) {
	row := s.db.QueryRow(`
		INSERT INTO video_analyses (source_key, source_url, model, meta, breakdown, summary, user_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (source_key) DO UPDATE SET
			source_url = EXCLUDED.source_url, model = EXCLUDED.model, meta = EXCLUDED.meta,
			breakdown = EXCLUDED.breakdown, summary = EXCLUDED.summary,
			user_id = EXCLUDED.user_id, created_at = NOW()
		RETURNING `+analysisCols,
		a.SourceKey, a.SourceURL, a.Model, []byte(a.Meta), []byte(a.Breakdown), a.Summary, userID)
	return scanAnalysis(row)
}

// formatPrompt returns the markdown of an active preset of the "formats"
// group, or "" when it does not exist.
func (s *store) formatPrompt(id string) (label, prompt string, err error) {
	err = s.db.QueryRow(`
		SELECT p.label, p.prompt
		FROM presets p JOIN preset_groups g ON g.id = p.group_id
		WHERE p.id::text = $1 AND g.slug = 'formats'
		  AND p.active AND p.deleted_at IS NULL AND g.deleted_at IS NULL`, id).Scan(&label, &prompt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return label, prompt, err
}
