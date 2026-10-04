package agency

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ─── Generation logs ────────────────────────────────────────────

// GenerationLog stores the complete log for a generation task,
// linking the client payload with the AI response via task ID.
type GenerationLog struct {
	ID               string           `json:"id"`
	TaskID           string           `json:"task_id"`
	ModelName        string           `json:"model_name"`
	UserID           *int             `json:"user_id"`
	EventID          string           `json:"event_id"`
	ProgramID        string           `json:"program_id"`
	PieceID          string           `json:"piece_id"`
	PieceCode        string           `json:"piece_code"`
	GenerationNumber int              `json:"generation_number"`
	Request          string           `json:"request"` // original client payload (JSON)
	Outputs          []OutputResource `json:"outputs"`
	Status           string           `json:"status"`
	ErrorMessage     string           `json:"error_message"`
	CreatedAt        time.Time        `json:"created_at"`
	UpdatedAt        time.Time        `json:"updated_at,omitempty"`
	DeletedAt        *time.Time       `json:"deleted_at,omitempty"`
	// Tipo de recurso de la generacion (video, image, audio, text).
	ResourceType string `json:"resource_type"`
	// Tipos de contenido enviados (ej. "text,image").
	ContentTypes string `json:"content_types"`
	// Costo estimado de la generacion en USD.
	EstimatedCost float64 `json:"estimated_cost"`
	// Fuente del costo: "api_response", "calculator", "pending",
	// "provider_estimate" (informado por la API del proveedor), "provider_refund".
	CostSource string `json:"cost_source"`
	// Costo en creditos del proveedor (Higgsfield cobra en creditos).
	CostCredits float64 `json:"cost_credits"`
	// Identificador con el que la generacion aparece como "Transaction ID"
	// en la consola del proveedor (request_id de Higgsfield).
	ProviderTransactionID string `json:"provider_transaction_id"`
	// Provider usage tokens (video/text generation accounting).
	UsageTokens           int64 `json:"usage_tokens"`
	UsageCompletionTokens int64 `json:"usage_completion_tokens"`
	// Video metadata from the provider response.
	VideoDuration   int    `json:"video_duration"`
	VideoResolution string `json:"video_resolution"`
	VideoRatio      string `json:"video_ratio"`
	VideoSeed       int64  `json:"video_seed"`
	VideoFPS        int    `json:"video_fps"`
	// Estimated progress percent 0-100 (100 on terminal states).
	Progress int `json:"progress"`
	// Two-check rating set by the creator ("Buena toma" / "Elegida final").
	RatingGood  bool `json:"rating_good"`
	RatingFinal bool `json:"rating_final"`
	// Enriched fields (LEFT JOIN, not stored in generation_logs)
	EventName       string `json:"event_name"`
	UserDisplayName string `json:"user_display_name"`
	PieceName       string `json:"piece_name"`
}

// ListLogsFilter holds pagination and filter params for listing logs.
type ListLogsFilter struct {
	Page         int    `form:"page"`
	Limit        int    `form:"limit"`
	EventID      string `form:"event_id"`
	ProgramID    string `form:"program_id"`
	PieceID      string `form:"piece_id"`
	Status       string `form:"status"`
	ModelName    string `form:"model_name"`
	UserID       int    `form:"user_id"`
	DateFrom     string `form:"date_from"`
	DateTo       string `form:"date_to"`
	ResourceType string `form:"resource_type"`
	// HasOutputs filters to generations with at least one stored output.
	HasOutputs bool `form:"-"`
}

// ListLogsResponse holds the paginated response for listing logs.
type ListLogsResponse struct {
	Logs       []GenerationLog `json:"logs"`
	Total      int             `json:"total"`
	Page       int             `json:"page"`
	Limit      int             `json:"limit"`
	TotalPages int             `json:"total_pages"`
}

// CostSummaryResponse holds the total cost for filtered generation logs.
type CostSummaryResponse struct {
	TotalCost float64 `json:"total_cost"`
}

// ─── GenerationLogStore ─────────────────────────────────────────

// GenerationLogStore persists generation logs in the tenant schema.
type GenerationLogStore struct {
	db *sql.DB
}

// NewGenerationLogStore builds a log store over a tenant pool.
func NewGenerationLogStore(db *sql.DB) *GenerationLogStore {
	return &GenerationLogStore{db: db}
}

const genLogListCols = `gl.id, gl.task_id, gl.model_name,
		gl.status,
		COALESCE(gl.error_message, '') AS error_message,
		gl.user_id,
		COALESCE(gl.event_id::text, '') AS event_id,
		COALESCE(gl.program_id::text, '') AS program_id,
		COALESCE(gl.piece_id::text, '') AS piece_id,
		COALESCE(gl.piece_code, '') AS piece_code,
		COALESCE(gl.generation_number, 0) AS generation_number,
		COALESCE(gl.outputs, '') AS outputs,
		gl.resource_type, gl.content_types,
		gl.estimated_cost, gl.cost_source,
		gl.cost_credits, COALESCE(gl.provider_transaction_id, '') AS provider_transaction_id,
		gl.usage_tokens, gl.usage_completion_tokens,
		gl.video_duration, COALESCE(gl.video_resolution, '') AS video_resolution,
		COALESCE(gl.video_ratio, '') AS video_ratio,
		gl.video_seed, gl.video_fps, gl.progress,
		gl.rating_good, gl.rating_final,
		gl.created_at, gl.updated_at`

const genLogFullCols = `gl.id, gl.task_id, gl.model_name,
		COALESCE(gl.request_payload, '') AS request_payload,
		COALESCE(gl.outputs, '') AS outputs,
		gl.status,
		COALESCE(gl.error_message, '') AS error_message,
		gl.user_id,
		COALESCE(gl.event_id::text, '') AS event_id,
		COALESCE(gl.program_id::text, '') AS program_id,
		COALESCE(gl.piece_id::text, '') AS piece_id,
		COALESCE(gl.piece_code, '') AS piece_code,
		COALESCE(gl.generation_number, 0) AS generation_number,
		gl.resource_type, gl.content_types,
		gl.estimated_cost, gl.cost_source,
		gl.cost_credits, COALESCE(gl.provider_transaction_id, '') AS provider_transaction_id,
		gl.usage_tokens, gl.usage_completion_tokens,
		gl.video_duration, COALESCE(gl.video_resolution, '') AS video_resolution,
		COALESCE(gl.video_ratio, '') AS video_ratio,
		gl.video_seed, gl.video_fps, gl.progress,
		gl.rating_good, gl.rating_final,
		gl.created_at, gl.updated_at, gl.deleted_at`

const genLogJoinCols = `COALESCE(ev.name, '') AS event_name,
		COALESCE(u.name || ' ' || u.surname, '') AS user_display_name,
		COALESCE(pc.name, '') AS piece_name`

const genLogFromJoins = `FROM generation_logs gl
		LEFT JOIN events ev ON ev.id = gl.event_id
		LEFT JOIN pieces pc ON pc.id = gl.piece_id
		LEFT JOIN users u ON u.id = gl.user_id`

// scanListRow scans a list query row (without request payload).
func (s *GenerationLogStore) scanListRow(row *GenerationLog, scanner interface {
	Scan(dest ...interface{}) error
}) error {
	var outputsStr string
	err := scanner.Scan(
		&row.ID, &row.TaskID, &row.ModelName,
		&row.Status, &row.ErrorMessage,
		&row.UserID, &row.EventID, &row.ProgramID, &row.PieceID, &row.PieceCode,
		&row.GenerationNumber,
		&outputsStr,
		&row.ResourceType, &row.ContentTypes,
		&row.EstimatedCost, &row.CostSource,
		&row.CostCredits, &row.ProviderTransactionID,
		&row.UsageTokens, &row.UsageCompletionTokens,
		&row.VideoDuration, &row.VideoResolution, &row.VideoRatio,
		&row.VideoSeed, &row.VideoFPS, &row.Progress,
		&row.RatingGood, &row.RatingFinal,
		&row.CreatedAt, &row.UpdatedAt,
		&row.EventName, &row.UserDisplayName, &row.PieceName,
	)
	if err != nil {
		return err
	}
	if outputsStr != "" {
		json.Unmarshal([]byte(outputsStr), &row.Outputs)
	}
	return nil
}

// scanDetailRow scans a detail query row (includes request payload and outputs).
func (s *GenerationLogStore) scanDetailRow(row *GenerationLog, scanner interface {
	Scan(dest ...interface{}) error
}) error {
	var outputsStr string
	err := scanner.Scan(
		&row.ID, &row.TaskID, &row.ModelName,
		&row.Request, &outputsStr,
		&row.Status, &row.ErrorMessage,
		&row.UserID, &row.EventID, &row.ProgramID, &row.PieceID, &row.PieceCode,
		&row.GenerationNumber,
		&row.ResourceType, &row.ContentTypes,
		&row.EstimatedCost, &row.CostSource,
		&row.CostCredits, &row.ProviderTransactionID,
		&row.UsageTokens, &row.UsageCompletionTokens,
		&row.VideoDuration, &row.VideoResolution, &row.VideoRatio,
		&row.VideoSeed, &row.VideoFPS, &row.Progress,
		&row.RatingGood, &row.RatingFinal,
		&row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
		&row.EventName, &row.UserDisplayName, &row.PieceName,
	)
	if err != nil {
		return err
	}
	if outputsStr != "" {
		json.Unmarshal([]byte(outputsStr), &row.Outputs)
	}
	return nil
}

// Create inserts a new generation log entry.
func (s *GenerationLogStore) Create(log *GenerationLog) error {
	query := `INSERT INTO generation_logs (task_id, model_name, request_payload, outputs, status, error_message, user_id, event_id, program_id, piece_id, piece_code, generation_number, resource_type, content_types, estimated_cost, cost_source, cost_credits, provider_transaction_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		RETURNING id, created_at, updated_at`

	outputsStr := marshalOutputs(log.Outputs)

	return s.db.QueryRow(query,
		log.TaskID,
		log.ModelName,
		nullIfEmptyStr(log.Request),
		nullIfEmptyStr(outputsStr),
		log.Status,
		nullIfEmptyStr(log.ErrorMessage),
		log.UserID,
		nullIfEmptyStr(log.EventID),
		nullIfEmptyStr(log.ProgramID),
		nullIfEmptyStr(log.PieceID),
		nullIfEmptyStr(log.PieceCode),
		log.GenerationNumber,
		log.ResourceType,
		log.ContentTypes,
		log.EstimatedCost,
		log.CostSource,
		log.CostCredits,
		log.ProviderTransactionID,
	).Scan(&log.ID, &log.CreatedAt, &log.UpdatedAt)
}

// GetByID returns a single log entry by its ID (includes full payload).
func (s *GenerationLogStore) GetByID(id string) (*GenerationLog, error) {
	log := &GenerationLog{}
	query := `SELECT ` + genLogFullCols + `, ` + genLogJoinCols + ` ` + genLogFromJoins + ` WHERE gl.id = $1 AND gl.deleted_at IS NULL`
	if err := s.scanDetailRow(log, s.db.QueryRow(query, id)); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return log, nil
}

// GetByTaskID returns a log entry by its task ID (includes full payload).
func (s *GenerationLogStore) GetByTaskID(taskID string) (*GenerationLog, error) {
	log := &GenerationLog{}
	query := `SELECT ` + genLogFullCols + `, ` + genLogJoinCols + ` ` + genLogFromJoins + ` WHERE gl.task_id = $1 AND gl.deleted_at IS NULL`
	if err := s.scanDetailRow(log, s.db.QueryRow(query, taskID)); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return log, nil
}

// UpdateByTaskID updates a log entry by its task ID (when async tasks complete).
func (s *GenerationLogStore) UpdateByTaskID(taskID string, outputs []OutputResource, status, errorMessage string) error {
	query := `UPDATE generation_logs
		SET outputs = $1, status = $2, error_message = $3, updated_at = NOW()
		WHERE task_id = $4 AND deleted_at IS NULL`

	outputsStr := marshalOutputs(outputs)

	result, err := s.db.Exec(query, nullIfEmptyStr(outputsStr), status, nullIfEmptyStr(errorMessage), taskID)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("generation log not found for task: %s", taskID)
	}
	return nil
}

// RefundProviderCostByTaskID zeroes the provider-reported spend of a task
// (Higgsfield refunds failed/nsfw/canceled requests). Only rows whose cost
// came from the provider estimate are touched, so locally-calculated costs
// (Seedance, Seedream) keep their behavior. Returns the affected row count.
func (s *GenerationLogStore) RefundProviderCostByTaskID(taskID string) (int64, error) {
	query := `UPDATE generation_logs SET
		estimated_cost = 0, cost_credits = 0, cost_source = 'provider_refund', updated_at = NOW()
		WHERE task_id = $1 AND deleted_at IS NULL AND cost_source = 'provider_estimate'`
	res, err := s.db.Exec(query, taskID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// UpdateMetadataByTaskID persists video generation metadata + progress sampled
// from the provider response. Only non-zero values overwrite existing ones so
// partial responses never erase previously stored data.
func (s *GenerationLogStore) UpdateMetadataByTaskID(taskID string, md VideoMetadata) error {
	query := `UPDATE generation_logs SET
		usage_tokens = GREATEST(usage_tokens, $1),
		usage_completion_tokens = GREATEST(usage_completion_tokens, $2),
		video_duration = CASE WHEN $3 > 0 THEN $3 ELSE video_duration END,
		video_resolution = CASE WHEN $4 <> '' THEN $4 ELSE video_resolution END,
		video_ratio = CASE WHEN $5 <> '' THEN $5 ELSE video_ratio END,
		video_seed = CASE WHEN $6 > 0 THEN $6 ELSE video_seed END,
		video_fps = CASE WHEN $7 > 0 THEN $7 ELSE video_fps END,
		progress = GREATEST(progress, $8),
		updated_at = NOW()
		WHERE task_id = $9 AND deleted_at IS NULL`
	_, err := s.db.Exec(query,
		md.UsageTokens, md.UsageCompletionTokens,
		md.Duration, md.Resolution, md.Ratio, md.Seed, md.FPS,
		md.Progress, taskID,
	)
	return err
}

// GetProgressByTaskID returns the stored progress percent for a task.
func (s *GenerationLogStore) GetProgressByTaskID(taskID string) (int, error) {
	var progress int
	err := s.db.QueryRow(
		`SELECT progress FROM generation_logs WHERE task_id = $1 AND deleted_at IS NULL`,
		taskID,
	).Scan(&progress)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return progress, err
}

// List returns paginated generation logs, newest first (light columns).
func (s *GenerationLogStore) List(page, limit int) ([]GenerationLog, int, error) {
	return s.ListByFilter(ListLogsFilter{Page: page, Limit: limit})
}

// ListByFilter returns paginated generation logs filtered by the given criteria.
func (s *GenerationLogStore) ListByFilter(f ListLogsFilter) ([]GenerationLog, int, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.Limit < 1 || f.Limit > 100 {
		f.Limit = 20
	}
	offset := (f.Page - 1) * f.Limit

	where, args := buildLogWhere(f)
	argIdx := len(args) + 1

	var total int
	countQuery := "SELECT COUNT(*) FROM generation_logs gl " + where
	if err := s.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := "SELECT " + genLogListCols + ", " + genLogJoinCols + " " + genLogFromJoins + " " + where +
		" ORDER BY gl.created_at DESC LIMIT $" + itoa(argIdx) + " OFFSET $" + itoa(argIdx+1)
	args = append(args, f.Limit, offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var logs []GenerationLog
	for rows.Next() {
		var l GenerationLog
		if err := s.scanListRow(&l, rows); err != nil {
			return nil, 0, err
		}
		logs = append(logs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return logs, total, nil
}

// SumCostByFilter returns the total estimated_cost matching the filters.
func (s *GenerationLogStore) SumCostByFilter(f ListLogsFilter) (float64, error) {
	where, args := buildLogWhere(f)
	query := "SELECT COALESCE(SUM(gl.estimated_cost), 0) FROM generation_logs gl " + where
	var total float64
	if err := s.db.QueryRow(query, args...).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// ListRecentByUser returns the user's most recent generations (own rows only).
// Includes the request payload so clients can rebuild the original studio
// request (prompt, ratio, resolution, duration) after a page reload.
func (s *GenerationLogStore) ListRecentByUser(userID int64, limit int) ([]GenerationLog, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT `+genLogFullCols+`, `+genLogJoinCols+` `+genLogFromJoins+`
		WHERE gl.deleted_at IS NULL AND gl.user_id = $1
		ORDER BY gl.created_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []GenerationLog
	for rows.Next() {
		var l GenerationLog
		if err := s.scanDetailRow(&l, rows); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

// UserHistoryFilter selects the caller's generations for the studio
// date-history (session recovery). FromDate/ToDate are optional bounds on
// created_at (inclusive/exclusive respectively); ResourceType filters by modality.
type UserHistoryFilter struct {
	FromDate     *time.Time
	ToDate       *time.Time
	ResourceType string
	Limit        int
}

// ListHistoryByUser returns the user's generations inside an optional
// created_at window (own rows only, newest first), including the request
// payload so the client can rebuild and re-run the original studio request.
func (s *GenerationLogStore) ListHistoryByUser(userID int64, f UserHistoryFilter) ([]GenerationLog, error) {
	if f.Limit < 1 || f.Limit > 200 {
		f.Limit = 100
	}
	query := `SELECT ` + genLogFullCols + `, ` + genLogJoinCols + ` ` + genLogFromJoins + `
		WHERE gl.deleted_at IS NULL AND gl.user_id = $1`
	args := []interface{}{userID}
	if f.FromDate != nil {
		args = append(args, *f.FromDate)
		query += fmt.Sprintf(" AND gl.created_at >= $%d", len(args))
	}
	if f.ToDate != nil {
		args = append(args, *f.ToDate)
		query += fmt.Sprintf(" AND gl.created_at < $%d", len(args))
	}
	if f.ResourceType != "" {
		args = append(args, f.ResourceType)
		query += fmt.Sprintf(" AND gl.resource_type = $%d", len(args))
	}
	query += fmt.Sprintf(" ORDER BY gl.created_at DESC LIMIT $%d", len(args)+1)
	args = append(args, f.Limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []GenerationLog
	for rows.Next() {
		var l GenerationLog
		if err := s.scanDetailRow(&l, rows); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

// UpdateRatingByTaskID sets the two-check rating of one task. Only the owner
// may rate: the update matches user_id too and returns rows affected (0 =
// not found or not the caller's row).
func (s *GenerationLogStore) UpdateRatingByTaskID(taskID string, userID int64, good, final bool) (int64, error) {
	res, err := s.db.Exec(`UPDATE generation_logs
		SET rating_good = $2, rating_final = $3, updated_at = CURRENT_TIMESTAMP
		WHERE task_id = $1 AND user_id = $4 AND deleted_at IS NULL`,
		taskID, good, final, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListNonFinalTaskIDs returns task ids with a non-terminal status (reconciler input).
func (s *GenerationLogStore) ListNonFinalTaskIDs(limit int) ([]GenerationLog, error) {
	rows, err := s.db.Query(`SELECT `+genLogFullCols+`, `+genLogJoinCols+` `+genLogFromJoins+`
		WHERE gl.deleted_at IS NULL AND gl.status NOT IN ('succeeded', 'failed', 'cancelled')
		ORDER BY gl.created_at ASC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []GenerationLog
	for rows.Next() {
		var l GenerationLog
		if err := s.scanDetailRow(&l, rows); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

func buildLogWhere(f ListLogsFilter) (string, []interface{}) {
	where := "WHERE gl.deleted_at IS NULL"
	args := []interface{}{}
	argIdx := 1
	add := func(col string, val interface{}, ilike bool) {
		if ilike {
			where += fmt.Sprintf(" AND %s ILIKE $%d", col, argIdx)
			args = append(args, "%"+fmt.Sprint(val)+"%")
		} else {
			where += fmt.Sprintf(" AND %s = $%d", col, argIdx)
			args = append(args, val)
		}
		argIdx++
	}
	if f.EventID != "" {
		add("gl.event_id", f.EventID, false)
	}
	if f.ProgramID != "" {
		add("gl.program_id", f.ProgramID, false)
	}
	if f.PieceID != "" {
		add("gl.piece_id", f.PieceID, false)
	}
	if f.Status != "" {
		add("gl.status", f.Status, false)
	}
	if f.ModelName != "" {
		add("gl.model_name", f.ModelName, true)
	}
	if f.UserID > 0 {
		add("gl.user_id", f.UserID, false)
	}
	if f.DateFrom != "" {
		add("gl.created_at", f.DateFrom, false)
	}
	if f.DateTo != "" {
		add("gl.created_at", f.DateTo+"T23:59:59Z", false)
	}
	if f.ResourceType != "" {
		add("gl.resource_type", f.ResourceType, false)
	}
	// Only generations that actually produced an output (used by the videos
	// gallery; empty outputs have nothing to show).
	if f.HasOutputs {
		where += " AND gl.outputs IS NOT NULL AND gl.outputs <> '' AND gl.outputs <> '[]'"
	}
	return where, args
}

func marshalOutputs(outputs []OutputResource) string {
	if len(outputs) == 0 {
		return ""
	}
	b, err := json.Marshal(outputs)
	if err != nil {
		return ""
	}
	return string(b)
}

func nullIfEmptyStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ─── Server communications ─────────────────────────────────────

// ServerCommunication stores a trace of every request sent to an external AI API.
// Credential fields are ALWAYS stored masked (see credential.MaskPublic) — never raw secrets.
type ServerCommunication struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	ModelName string `json:"model_name"`
	Endpoint  string `json:"endpoint"`
	Method    string `json:"method"`
	// phase separates the generation submit ("generate") from status polls
	// ("poll") so both traces of one task group into a single logical record.
	Phase string `json:"phase,omitempty"`
	// Polling aggregates: how many polls happened and the submit→finish span.
	PollCount       int       `json:"poll_count"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	FinishedAt      time.Time `json:"finished_at,omitempty"`
	TotalDurationMs int64     `json:"total_duration_ms"`
	RequestBody     string    `json:"request_body,omitempty"`
	ResponseBody    string    `json:"response_body,omitempty"`
	StatusCode      int       `json:"status_code"`
	DurationMs      int64     `json:"duration_ms"`
	ErrorMessage    string    `json:"error_message,omitempty"`
	// Audit: who triggered the call.
	UserID     int64  `json:"user_id"`
	Username   string `json:"username"`
	TenantSlug string `json:"tenant_slug"`
	// Audit: which credentials were used (masked).
	CredentialProvider string    `json:"credential_provider"`
	APIKeyMask         string    `json:"api_key_mask"`
	AccessKeyMask      string    `json:"access_key_mask"`
	SecretKeyMask      string    `json:"secret_key_mask"`
	AuthType           string    `json:"auth_type"` // bearer | ak_sk | none
	CreatedAt          time.Time `json:"created_at"`
}

// ServerCommunicationStore persists external API traces.
type ServerCommunicationStore struct {
	db *sql.DB
}

// NewServerCommunicationStore builds a comm store over a tenant pool.
func NewServerCommunicationStore(db *sql.DB) *ServerCommunicationStore {
	return &ServerCommunicationStore{db: db}
}

const serverCommCols = `id, task_id, model_name, endpoint, method,
	COALESCE(phase, '') AS phase,
	COALESCE(poll_count, 0) AS poll_count,
	COALESCE(started_at, '0001-01-01T00:00:00Z') AS started_at,
	COALESCE(finished_at, '0001-01-01T00:00:00Z') AS finished_at,
	COALESCE(total_duration_ms, 0) AS total_duration_ms,
	COALESCE(request_body, '') AS request_body,
	COALESCE(response_body, '') AS response_body,
	status_code, duration_ms,
	COALESCE(error_message, '') AS error_message,
	COALESCE(user_id, 0) AS user_id,
	COALESCE(username, '') AS username,
	COALESCE(tenant_slug, '') AS tenant_slug,
	COALESCE(credential_provider, '') AS credential_provider,
	COALESCE(api_key_mask, '') AS api_key_mask,
	COALESCE(access_key_mask, '') AS access_key_mask,
	COALESCE(secret_key_mask, '') AS secret_key_mask,
	COALESCE(auth_type, '') AS auth_type,
	created_at`

// Create inserts a server communication trace.
//
// Polling traces (phase "poll") are task-focused: when a poll trace for the
// same task already exists, it is UPDATED in place — the response body and
// status refresh to the latest terminal poll, poll_count accumulates, and
// total_duration_ms is recomputed as submit→finish so the log shows one row
// per task event instead of one row per polling tick.
func (s *ServerCommunicationStore) Create(log *ServerCommunication) error {
	if log.ID == "" {
		log.ID = uuid.New().String()
	}
	if log.Phase == "poll" && log.TaskID != "" {
		// Task-focused upsert: keep a single poll trace per task, refreshed on
		// every terminal poll (newest response_body / status wins). poll_count
		// accumulates; total_duration_ms = finished_at - started_at spans the
		// whole generation so progress can be estimated while polling.
		query := `UPDATE server_communications SET
			response_body = $1,
			status_code = $2,
			error_message = $3,
			poll_count = COALESCE(poll_count, 0) + 1,
			started_at = COALESCE(started_at, $6::timestamptz),
			finished_at = CASE WHEN $5::timestamptz IS NOT NULL THEN NOW() ELSE finished_at END,
			total_duration_ms = CASE
				WHEN $5::timestamptz IS NOT NULL AND started_at IS NOT NULL
					AND started_at > '0001-01-01'::timestamptz AND started_at < NOW()
				THEN LEAST(
					(EXTRACT(EPOCH FROM (NOW() - started_at)) * 1000)::BIGINT,
					999999999
				)
				ELSE total_duration_ms END,
			created_at = NOW()
			WHERE task_id = $4 AND phase = 'poll'
			RETURNING id, COALESCE(started_at, '0001-01-01T00:00:00Z'),
				COALESCE(finished_at, '0001-01-01T00:00:00Z'), poll_count, total_duration_ms`
		var existingID string
		err := s.db.QueryRow(query,
			nullIfEmptyStr(log.ResponseBody), log.StatusCode, nullIfEmptyStr(log.ErrorMessage), log.TaskID, nullTimePtr(log.FinishedAt), nullTimePtr(log.StartedAt),
		).Scan(&existingID, &log.StartedAt, &log.FinishedAt, &log.PollCount, &log.TotalDurationMs)
		if err == nil {
			log.ID = existingID
			log.CreatedAt = log.FinishedAt
			return nil
		}
		// No existing poll trace for this task — fall through to insert.
	}
	query := `INSERT INTO server_communications
		(id, task_id, model_name, endpoint, method, phase, poll_count, started_at, finished_at, total_duration_ms,
		 request_body, response_body, status_code, duration_ms, error_message,
		 user_id, username, tenant_slug, credential_provider, api_key_mask, access_key_mask, secret_key_mask, auth_type, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7,
			CASE WHEN $8::timestamptz IS NULL THEN NOW() ELSE $8::timestamptz END,
			CASE WHEN $9::timestamptz IS NULL THEN NOW() ELSE $9::timestamptz END,
			$10,
			$11, $12, $13, $14, $15,
			$16, $17, $18, $19, $20, $21, $22, $23, NOW())
		RETURNING created_at`
	return s.db.QueryRow(query, log.ID, log.TaskID, log.ModelName, log.Endpoint, log.Method,
		nullIfEmptyStr(log.Phase), log.PollCount,
		nullTimePtr(log.StartedAt), nullTimePtr(log.FinishedAt), log.TotalDurationMs,
		nullIfEmptyStr(log.RequestBody), nullIfEmptyStr(log.ResponseBody),
		log.StatusCode, log.DurationMs, nullIfEmptyStr(log.ErrorMessage),
		log.UserID, log.Username, log.TenantSlug, log.CredentialProvider,
		log.APIKeyMask, log.AccessKeyMask, log.SecretKeyMask, log.AuthType).
		Scan(&log.CreatedAt)
}

// nullTimePtr returns nil for the zero time so the insert stores NULL and
// defaults to NOW() instead of year 1.
func nullTimePtr(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return &t
}

// AttachTaskIDByPhase stamps the task ID onto the most recent row of the
// given phase for a model. Used to backfill the generation submit trace once
// the provider returns the task ID, so generate and poll rows share it.
func (s *ServerCommunicationStore) AttachTaskIDByPhase(phase, taskID, modelName string) error {
	query := `UPDATE server_communications
		SET task_id = $1
		WHERE id = (
			SELECT id FROM server_communications
			WHERE phase = $2 AND task_id = '' AND model_name = $3
			ORDER BY created_at DESC
			LIMIT 1
		)`
	result, err := s.db.Exec(query, taskID, phase, modelName)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("no %s trace to attach task %s", phase, taskID)
	}
	return nil
}

// GetByID returns one trace, or nil.
func (s *ServerCommunicationStore) GetByID(id string) (*ServerCommunication, error) {
	log := &ServerCommunication{}
	err := s.db.QueryRow(`SELECT `+serverCommCols+` FROM server_communications WHERE id = $1`, id).Scan(
		&log.ID, &log.TaskID, &log.ModelName, &log.Endpoint, &log.Method,
		&log.Phase, &log.PollCount, &log.StartedAt, &log.FinishedAt, &log.TotalDurationMs,
		&log.RequestBody, &log.ResponseBody,
		&log.StatusCode, &log.DurationMs, &log.ErrorMessage,
		&log.UserID, &log.Username, &log.TenantSlug, &log.CredentialProvider,
		&log.APIKeyMask, &log.AccessKeyMask, &log.SecretKeyMask, &log.AuthType,
		&log.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return log, nil
}

// ServerCommFilter holds optional filters for listing server communications.
type ServerCommFilter struct {
	TaskID    string
	ModelName string
	Page      int
	Limit     int
}

// ServerCommListResponse holds paginated results.
type ServerCommListResponse struct {
	Logs       []ServerCommunication `json:"logs"`
	Total      int                   `json:"total"`
	Page       int                   `json:"page"`
	Limit      int                   `json:"limit"`
	TotalPages int                   `json:"total_pages"`
}

// List returns paginated traces.
func (s *ServerCommunicationStore) List(filter ServerCommFilter) (*ServerCommListResponse, error) {
	if filter.Page < 1 {
		filter.Page = 1
	}
	if filter.Limit < 1 || filter.Limit > 100 {
		filter.Limit = 20
	}
	offset := (filter.Page - 1) * filter.Limit

	where := ""
	args := []interface{}{}
	argIdx := 1
	if filter.TaskID != "" {
		where += fmt.Sprintf(" WHERE task_id = $%d", argIdx)
		args = append(args, filter.TaskID)
		argIdx++
	}
	if filter.ModelName != "" {
		prefix := " WHERE"
		if where != "" {
			prefix = " AND"
		}
		where += fmt.Sprintf("%s model_name = $%d", prefix, argIdx)
		args = append(args, filter.ModelName)
		argIdx++
	}

	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM server_communications"+where, args...).Scan(&total); err != nil {
		return nil, err
	}

	query := "SELECT " + serverCommCols + " FROM server_communications" + where +
		" ORDER BY created_at DESC LIMIT $" + itoa(argIdx) + " OFFSET $" + itoa(argIdx+1)
	args = append(args, filter.Limit, offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []ServerCommunication
	for rows.Next() {
		var l ServerCommunication
		if err := rows.Scan(&l.ID, &l.TaskID, &l.ModelName, &l.Endpoint, &l.Method,
			&l.Phase, &l.PollCount, &l.StartedAt, &l.FinishedAt, &l.TotalDurationMs,
			&l.RequestBody, &l.ResponseBody,
			&l.StatusCode, &l.DurationMs, &l.ErrorMessage,
			&l.UserID, &l.Username, &l.TenantSlug, &l.CredentialProvider,
			&l.APIKeyMask, &l.AccessKeyMask, &l.SecretKeyMask, &l.AuthType,
			&l.CreatedAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	totalPages := (total + filter.Limit - 1) / filter.Limit
	if totalPages < 1 {
		totalPages = 1
	}
	return &ServerCommListResponse{
		Logs: logs, Total: total, Page: filter.Page, Limit: filter.Limit, TotalPages: totalPages,
	}, nil
}

// ─── Generated assets ───────────────────────────────────────────

// GeneratedAsset is a resource produced by a model (video, image...).
type GeneratedAsset struct {
	ID               string     `json:"id"`
	TaskID           string     `json:"task_id"`
	ModelName        string     `json:"model_name"`
	UserID           *int       `json:"user_id,omitempty"`
	EventID          string     `json:"event_id,omitempty"`
	ProgramID        string     `json:"program_id,omitempty"`
	PieceID          string     `json:"piece_id,omitempty"`
	PieceCode        string     `json:"piece_code,omitempty"`
	GenerationNumber int        `json:"generation_number"`
	OriginalURL      string     `json:"original_url"`
	LocalPath        string     `json:"local_path,omitempty"`
	Filename         string     `json:"filename,omitempty"`
	MimeType         string     `json:"mime_type,omitempty"`
	FileSize         int64      `json:"file_size"`
	Status           string     `json:"status"` // pending, confirmed, failed
	ConfirmedAt      *time.Time `json:"confirmed_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty"`
}

// GeneratedAssetStore persists generated assets in the tenant schema.
type GeneratedAssetStore struct {
	db *sql.DB
}

// NewGeneratedAssetStore builds the store over a tenant pool.
func NewGeneratedAssetStore(db *sql.DB) *GeneratedAssetStore {
	return &GeneratedAssetStore{db: db}
}

const genAssetCols = `id, task_id, COALESCE(model_name,'') AS model_name,
	user_id, COALESCE(event_id::text,'') AS event_id, COALESCE(program_id::text,'') AS program_id,
	COALESCE(piece_id::text,'') AS piece_id, COALESCE(piece_code,'') AS piece_code,
	COALESCE(generation_number,0) AS generation_number,
	original_url, COALESCE(local_path,'') AS local_path,
	COALESCE(filename,'') AS filename, COALESCE(mime_type,'') AS mime_type,
	COALESCE(file_size,0) AS file_size, status,
	confirmed_at, created_at, updated_at, deleted_at`

func scanAsset(a *GeneratedAsset, sc interface {
	Scan(dest ...interface{}) error
}) error {
	return sc.Scan(
		&a.ID, &a.TaskID, &a.ModelName,
		&a.UserID, &a.EventID, &a.ProgramID, &a.PieceID, &a.PieceCode, &a.GenerationNumber,
		&a.OriginalURL, &a.LocalPath,
		&a.Filename, &a.MimeType, &a.FileSize, &a.Status,
		&a.ConfirmedAt, &a.CreatedAt, &a.UpdatedAt, &a.DeletedAt,
	)
}

// Create inserts a new generated asset (status: pending).
func (s *GeneratedAssetStore) Create(a *GeneratedAsset) error {
	query := `INSERT INTO generated_assets (task_id, model_name, user_id, event_id, program_id, piece_id, piece_code, generation_number, original_url, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, created_at, updated_at`
	return s.db.QueryRow(query,
		a.TaskID, a.ModelName, a.UserID,
		nullIfEmptyStr(a.EventID), nullIfEmptyStr(a.ProgramID), nullIfEmptyStr(a.PieceID),
		nullIfEmptyStr(a.PieceCode), a.GenerationNumber,
		a.OriginalURL, a.Status,
	).Scan(&a.ID, &a.CreatedAt, &a.UpdatedAt)
}

// ListByPiece returns the assets of a piece, ordered by created_at DESC.
func (s *GeneratedAssetStore) ListByPiece(pieceID string) ([]GeneratedAsset, error) {
	rows, err := s.db.Query(`SELECT `+genAssetCols+` FROM generated_assets
		WHERE deleted_at IS NULL AND piece_id = $1 ORDER BY created_at DESC`, pieceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var assets []GeneratedAsset
	for rows.Next() {
		var a GeneratedAsset
		if err := scanAsset(&a, rows); err != nil {
			return nil, err
		}
		assets = append(assets, a)
	}
	return assets, rows.Err()
}

// ListPendingByTask returns pending assets of a task.
func (s *GeneratedAssetStore) ListPendingByTask(taskID string) ([]GeneratedAsset, error) {
	rows, err := s.db.Query(`SELECT `+genAssetCols+` FROM generated_assets
		WHERE deleted_at IS NULL AND task_id = $1 AND status = 'pending' ORDER BY created_at DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var assets []GeneratedAsset
	for rows.Next() {
		var a GeneratedAsset
		if err := scanAsset(&a, rows); err != nil {
			return nil, err
		}
		assets = append(assets, a)
	}
	return assets, rows.Err()
}

// Confirm marks an asset confirmed with its local path.
func (s *GeneratedAssetStore) Confirm(id, localPath, filename, mimeType string, fileSize int64) error {
	query := `UPDATE generated_assets
		SET status = 'confirmed', local_path = $1, filename = $2, mime_type = $3, file_size = $4, confirmed_at = NOW(), updated_at = NOW()
		WHERE id = $5 AND deleted_at IS NULL`
	result, err := s.db.Exec(query, localPath, filename, mimeType, fileSize, id)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("generated asset not found")
	}
	return nil
}

// Fail marks an asset as failed.
func (s *GeneratedAssetStore) Fail(id string) error {
	_, err := s.db.Exec(`UPDATE generated_assets SET status = 'failed', updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL`, id)
	return err
}

// ─── Model assets (BytePlus gallery sync) ───────────────────────

// ModelAsset represents a file synced with a model's asset library.
// Gallery models reference assets as asset://<id>; others use the CDN URL.
type ModelAsset struct {
	ID           string    `json:"id"`
	ModelID      string    `json:"model_id"`
	FileID       string    `json:"file_id"`
	AssetID      string    `json:"asset_id"`
	AssetGroupID string    `json:"asset_group_id"`
	Status       string    `json:"status"` // "syncing", "active", "failed"
	ErrorMessage string    `json:"error_message,omitempty"`
	AssetURL     string    `json:"asset_url,omitempty"`
	AssetType    string    `json:"asset_type,omitempty"`
	ReferenceURI string    `json:"reference_uri,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ModelSyncSummary aggregates model_assets per model.
type ModelSyncSummary struct {
	ModelID  string     `json:"model_id"`
	Total    int        `json:"total"`
	Active   int        `json:"active"`
	Failed   int        `json:"failed"`
	Syncing  int        `json:"syncing"`
	LastSync *time.Time `json:"last_sync,omitempty"`
}

const modelAssetCols = `id, model_id, file_id, asset_id, asset_group_id, status,
	COALESCE(error_message,'') AS error_message, COALESCE(asset_url,'') AS asset_url,
	COALESCE(asset_type,'') AS asset_type, COALESCE(reference_uri,'') AS reference_uri,
	created_at, updated_at`

// AssetSyncStore persists the model↔asset sync mappings.
type AssetSyncStore struct {
	db *sql.DB
}

// NewAssetSyncStore builds the store over a tenant pool.
func NewAssetSyncStore(db *sql.DB) *AssetSyncStore {
	return &AssetSyncStore{db: db}
}

// Create inserts a sync record.
func (s *AssetSyncStore) Create(ma *ModelAsset) error {
	if ma.ID == "" {
		ma.ID = uuid.New().String()
	}
	query := `INSERT INTO model_assets (id, model_id, file_id, asset_id, asset_group_id, status, error_message, asset_type)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING created_at, updated_at`
	return s.db.QueryRow(query, ma.ID, ma.ModelID, ma.FileID, ma.AssetID,
		ma.AssetGroupID, ma.Status, nullIfEmptyStr(ma.ErrorMessage), ma.AssetType).
		Scan(&ma.CreatedAt, &ma.UpdatedAt)
}

// GetByID returns one sync record, or nil.
func (s *AssetSyncStore) GetByID(id string) (*ModelAsset, error) {
	ma := &ModelAsset{}
	err := s.db.QueryRow(`SELECT `+modelAssetCols+` FROM model_assets WHERE id = $1`, id).
		Scan(&ma.ID, &ma.ModelID, &ma.FileID, &ma.AssetID, &ma.AssetGroupID, &ma.Status,
			&ma.ErrorMessage, &ma.AssetURL, &ma.AssetType, &ma.ReferenceURI, &ma.CreatedAt, &ma.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return ma, nil
}

// GetByModelAndFile returns the latest sync record for a model+file pair.
func (s *AssetSyncStore) GetByModelAndFile(modelID, fileID string) (*ModelAsset, error) {
	ma := &ModelAsset{}
	err := s.db.QueryRow(`SELECT `+modelAssetCols+` FROM model_assets
		WHERE model_id = $1 AND file_id = $2 ORDER BY created_at DESC LIMIT 1`, modelID, fileID).
		Scan(&ma.ID, &ma.ModelID, &ma.FileID, &ma.AssetID, &ma.AssetGroupID, &ma.Status,
			&ma.ErrorMessage, &ma.AssetURL, &ma.AssetType, &ma.ReferenceURI, &ma.CreatedAt, &ma.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return ma, nil
}

// ListByModel returns all sync records of a model.
func (s *AssetSyncStore) ListByModel(modelID string) ([]ModelAsset, error) {
	rows, err := s.db.Query(`SELECT `+modelAssetCols+` FROM model_assets
		WHERE model_id = $1 ORDER BY created_at DESC`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var assets []ModelAsset
	for rows.Next() {
		var ma ModelAsset
		if err := rows.Scan(&ma.ID, &ma.ModelID, &ma.FileID, &ma.AssetID, &ma.AssetGroupID, &ma.Status,
			&ma.ErrorMessage, &ma.AssetURL, &ma.AssetType, &ma.ReferenceURI, &ma.CreatedAt, &ma.UpdatedAt); err != nil {
			return nil, err
		}
		assets = append(assets, ma)
	}
	return assets, rows.Err()
}

// UpdateStatus persists the final sync result.
func (s *AssetSyncStore) UpdateStatus(id, status, errorMessage, assetID, assetURL, assetType, referenceURI string) error {
	_, err := s.db.Exec(`UPDATE model_assets SET status = $1, error_message = $2, asset_id = $3,
		asset_url = $4, asset_type = $5, reference_uri = $6, updated_at = NOW() WHERE id = $7`,
		status, nullIfEmptyStr(errorMessage), assetID, assetURL, assetType, referenceURI, id)
	return err
}

// GetByFileIDs returns active sync records for the given file IDs, grouped by file.
func (s *AssetSyncStore) GetByFileIDs(fileIDs []string) (map[string][]ModelAsset, error) {
	result := make(map[string][]ModelAsset)
	if len(fileIDs) == 0 {
		return result, nil
	}
	placeholders := make([]string, len(fileIDs))
	args := make([]interface{}, len(fileIDs))
	for i, id := range fileIDs {
		args[i] = id
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	query := fmt.Sprintf(`SELECT %s FROM model_assets WHERE file_id IN (%s) AND status = 'active' ORDER BY created_at DESC`,
		modelAssetCols, strings.Join(placeholders, ","))

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var ma ModelAsset
		if err := rows.Scan(&ma.ID, &ma.ModelID, &ma.FileID, &ma.AssetID, &ma.AssetGroupID, &ma.Status,
			&ma.ErrorMessage, &ma.AssetURL, &ma.AssetType, &ma.ReferenceURI, &ma.CreatedAt, &ma.UpdatedAt); err != nil {
			return nil, err
		}
		result[ma.FileID] = append(result[ma.FileID], ma)
	}
	return result, rows.Err()
}

// ListModelSummaries aggregates sync records per model.
func (s *AssetSyncStore) ListModelSummaries() ([]ModelSyncSummary, error) {
	rows, err := s.db.Query(`SELECT model_id, COUNT(*) AS total,
			COUNT(*) FILTER (WHERE status = 'active') AS active,
			COUNT(*) FILTER (WHERE status = 'failed') AS failed,
			COUNT(*) FILTER (WHERE status = 'syncing') AS syncing,
			MAX(updated_at) AS last_sync
		FROM model_assets GROUP BY model_id ORDER BY last_sync DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []ModelSyncSummary
	for rows.Next() {
		var m ModelSyncSummary
		if err := rows.Scan(&m.ModelID, &m.Total, &m.Active, &m.Failed, &m.Syncing, &m.LastSync); err != nil {
			return nil, err
		}
		summaries = append(summaries, m)
	}
	return summaries, rows.Err()
}

// ListRecentErrors returns the last failed sync attempts for a file in a model.
func (s *AssetSyncStore) ListRecentErrors(modelID, fileID string, limit int) ([]ModelAsset, error) {
	rows, err := s.db.Query(`SELECT `+modelAssetCols+` FROM model_assets
		WHERE model_id = $1 AND file_id = $2 AND status = 'failed'
		ORDER BY created_at DESC LIMIT $3`, modelID, fileID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ModelAsset
	for rows.Next() {
		var ma ModelAsset
		if err := rows.Scan(&ma.ID, &ma.ModelID, &ma.FileID, &ma.AssetID, &ma.AssetGroupID, &ma.Status,
			&ma.ErrorMessage, &ma.AssetURL, &ma.AssetType, &ma.ReferenceURI, &ma.CreatedAt, &ma.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, ma)
	}
	return out, rows.Err()
}
