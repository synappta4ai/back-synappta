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
	// Fuente del costo: "api_response", "calculator", "pending".
	CostSource string `json:"cost_source"`
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
		gl.created_at, gl.updated_at, gl.deleted_at`

const genLogJoinCols = `COALESCE(ev.name, '') AS event_name,
		COALESCE(u.name || ' ' || u.surname, '') AS user_display_name,
		COALESCE(pc.name, '') AS piece_name`

const genLogFromJoins = `FROM generation_logs gl
		LEFT JOIN events ev ON ev.id::text = gl.event_id
		LEFT JOIN pieces pc ON pc.id::text = gl.piece_id
		LEFT JOIN tenant_system.users u ON u.id = gl.user_id`

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
	query := `INSERT INTO generation_logs (task_id, model_name, request_payload, outputs, status, error_message, user_id, event_id, program_id, piece_id, piece_code, generation_number, resource_type, content_types, estimated_cost, cost_source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
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
type ServerCommunication struct {
	ID           string    `json:"id"`
	TaskID       string    `json:"task_id"`
	ModelName    string    `json:"model_name"`
	Endpoint     string    `json:"endpoint"`
	Method       string    `json:"method"`
	RequestBody  string    `json:"request_body,omitempty"`
	ResponseBody string    `json:"response_body,omitempty"`
	StatusCode   int       `json:"status_code"`
	DurationMs   int64     `json:"duration_ms"`
	ErrorMessage string    `json:"error_message,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
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
	COALESCE(request_body, '') AS request_body,
	COALESCE(response_body, '') AS response_body,
	status_code, duration_ms,
	COALESCE(error_message, '') AS error_message,
	created_at`

// Create inserts a server communication trace.
func (s *ServerCommunicationStore) Create(log *ServerCommunication) error {
	if log.ID == "" {
		log.ID = uuid.New().String()
	}
	query := `INSERT INTO server_communications
		(id, task_id, model_name, endpoint, method, request_body, response_body, status_code, duration_ms, error_message, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
		RETURNING created_at`
	return s.db.QueryRow(query, log.ID, log.TaskID, log.ModelName, log.Endpoint, log.Method,
		nullIfEmptyStr(log.RequestBody), nullIfEmptyStr(log.ResponseBody),
		log.StatusCode, log.DurationMs, nullIfEmptyStr(log.ErrorMessage)).
		Scan(&log.CreatedAt)
}

// GetByID returns one trace, or nil.
func (s *ServerCommunicationStore) GetByID(id string) (*ServerCommunication, error) {
	log := &ServerCommunication{}
	err := s.db.QueryRow(`SELECT `+serverCommCols+` FROM server_communications WHERE id = $1`, id).Scan(
		&log.ID, &log.TaskID, &log.ModelName, &log.Endpoint, &log.Method,
		&log.RequestBody, &log.ResponseBody,
		&log.StatusCode, &log.DurationMs, &log.ErrorMessage, &log.CreatedAt)
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
			&l.RequestBody, &l.ResponseBody,
			&l.StatusCode, &l.DurationMs, &l.ErrorMessage, &l.CreatedAt); err != nil {
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
