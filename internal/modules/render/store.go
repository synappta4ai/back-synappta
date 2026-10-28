package render

import (
	"database/sql"
	"encoding/json"
	"time"
)

// ProjectRow is an editor project as stored.
type ProjectRow struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Project   json.RawMessage `json:"project,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Job is a render job as exposed to the API.
type Job struct {
	ID        string    `json:"id"`
	ProjectID *string   `json:"project_id,omitempty"`
	Status    string    `json:"status"` // queued | running | succeeded | failed
	Progress  int       `json:"progress"`
	Error     string    `json:"error,omitempty"`
	FileID    *string   `json:"file_id,omitempty"`
	OutputURL string    `json:"output_url,omitempty"`
	Format    string    `json:"format,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type store struct{ db *sql.DB }

// ─── Projects ───────────────────────────────────────────────────

func (s *store) createProject(userID int64, name string, doc []byte) (*ProjectRow, error) {
	p := &ProjectRow{Name: name, Project: doc}
	err := s.db.QueryRow(`INSERT INTO editor_projects (user_id, name, project_json)
		VALUES ($1, $2, $3::jsonb) RETURNING id, created_at, updated_at`, userID, name, string(doc)).
		Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func (s *store) listProjects(userID int64) ([]ProjectRow, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at, updated_at FROM editor_projects
		WHERE user_id = $1 AND deleted_at IS NULL ORDER BY updated_at DESC LIMIT 200`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectRow{}
	for rows.Next() {
		var p ProjectRow
		if err := rows.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// getProject returns nil when the project does not exist or is not the user's.
func (s *store) getProject(userID int64, id string) (*ProjectRow, error) {
	p := &ProjectRow{}
	var raw []byte
	err := s.db.QueryRow(`SELECT id, name, project_json, created_at, updated_at FROM editor_projects
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`, id, userID).
		Scan(&p.ID, &p.Name, &raw, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	p.Project = raw
	return p, err
}

func (s *store) updateProject(userID int64, id string, name *string, doc []byte) (bool, error) {
	res, err := s.db.Exec(`UPDATE editor_projects SET
			name = COALESCE($3, name),
			project_json = CASE WHEN $4::text = '' THEN project_json ELSE $4::text::jsonb END,
			updated_at = NOW()
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`, id, userID, name, string(doc))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *store) deleteProject(userID int64, id string) (bool, error) {
	res, err := s.db.Exec(`UPDATE editor_projects SET deleted_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`, id, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ─── Jobs ───────────────────────────────────────────────────────

const jobCols = `id, project_id, status, progress, error_message, file_id, output_url, format, created_at, updated_at`

func scanJob(sc interface{ Scan(...any) error }) (*Job, error) {
	j := &Job{}
	if err := sc.Scan(&j.ID, &j.ProjectID, &j.Status, &j.Progress, &j.Error, &j.FileID,
		&j.OutputURL, &j.Format, &j.CreatedAt, &j.UpdatedAt); err != nil {
		return nil, err
	}
	return j, nil
}

func (s *store) createJob(userID int64, projectID *string, doc []byte, format string) (*Job, error) {
	row := s.db.QueryRow(`INSERT INTO render_jobs (user_id, project_id, project_json, format)
		VALUES ($1, $2, $3::jsonb, $4) RETURNING `+jobCols, userID, projectID, string(doc), format)
	return scanJob(row)
}

func (s *store) getJob(userID int64, id string) (*Job, error) {
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM render_jobs WHERE id = $1 AND user_id = $2`, id, userID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return j, err
}

func (s *store) listJobs(userID int64) ([]Job, error) {
	rows, err := s.db.Query(`SELECT `+jobCols+` FROM render_jobs WHERE user_id = $1
		ORDER BY created_at DESC LIMIT 50`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

func (s *store) markRunning(id string) error {
	_, err := s.db.Exec(`UPDATE render_jobs SET status = 'running', progress = 0, updated_at = NOW()
		WHERE id = $1 AND status = 'queued'`, id)
	return err
}

func (s *store) setProgress(id string, pct int) {
	_, _ = s.db.Exec(`UPDATE render_jobs SET progress = $2, updated_at = NOW()
		WHERE id = $1 AND status = 'running'`, id, pct)
}

func (s *store) markSucceeded(id, fileID, url string) error {
	_, err := s.db.Exec(`UPDATE render_jobs SET status = 'succeeded', progress = 100, file_id = $2,
		output_url = $3, error_message = '', updated_at = NOW() WHERE id = $1`, id, fileID, url)
	return err
}

// markSucceededTemp records a render that only lives until the user downloads
// it (nothing is added to the library).
func (s *store) markSucceededTemp(id, path, url string) error {
	_, err := s.db.Exec(`UPDATE render_jobs SET status = 'succeeded', progress = 100,
		output_path = $2, output_url = $3, error_message = '', updated_at = NOW() WHERE id = $1`,
		id, path, url)
	return err
}

// tempOutput is the on-disk file of a download-only render.
func (s *store) tempOutput(userID int64, id string) (path, format string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT output_path, format FROM render_jobs
		WHERE id = $1 AND user_id = $2 AND status = 'succeeded'`, id, userID).Scan(&path, &format)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return path, format, path != "", nil
}

// clearTempOutput forgets the file once it was delivered.
func (s *store) clearTempOutput(id string) {
	_, _ = s.db.Exec(`UPDATE render_jobs SET output_path = '' WHERE id = $1`, id)
}

// staleTempOutputs lists download-only renders old enough to be purged.
func (s *store) staleTempOutputs(maxAge time.Duration) ([]string, error) {
	rows, err := s.db.Query(`SELECT output_path FROM render_jobs
		WHERE output_path <> '' AND updated_at < NOW() - make_interval(secs => $1)`, maxAge.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *store) markFailed(id, msg string) {
	if len(msg) > 800 {
		msg = msg[:800]
	}
	_, _ = s.db.Exec(`UPDATE render_jobs SET status = 'failed', error_message = $2, updated_at = NOW()
		WHERE id = $1 AND status IN ('queued','running')`, id, msg)
}

// failStale marks as failed the jobs that cannot be alive anymore. At boot
// (includeQueued, maxAge 0) every unfinished job goes: no render goroutine can
// still be running. Periodically only 'running' jobs that stopped reporting
// progress are failed; 'queued' ones may legitimately wait for a free slot.
func (s *store) failStale(maxAge time.Duration, includeQueued bool) (int64, error) {
	statuses := `('running')`
	if includeQueued {
		statuses = `('queued','running')`
	}
	res, err := s.db.Exec(`UPDATE render_jobs SET status = 'failed',
			error_message = 'el servidor se reinicio o el render se interrumpio', updated_at = NOW()
		WHERE status IN `+statuses+` AND updated_at < NOW() - make_interval(secs => $1)`,
		maxAge.Seconds())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// countActive is the number of unfinished jobs of a user (abuse guard).
func (s *store) countActive(userID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM render_jobs
		WHERE user_id = $1 AND status IN ('queued','running')`, userID).Scan(&n)
	return n, err
}
