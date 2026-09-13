// Package event implements the event-oriented domain that replaces dcs-back's
// project hierarchy:
//
//	Event → Program (run-of-show block) → Piece (deliverable) → PieceGeneration (attempt)
//
// Resources attach to events/programs/pieces through the agnostic `assignments`
// table (see the assignment module).
package event

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"synapta/config"
	"synapta/internal/utils"
)

// ErrNotFound helpers per entity.
var (
	ErrEventNotFound      = errors.New("event not found")
	ErrProgramNotFound    = errors.New("program not found")
	ErrPieceNotFound      = errors.New("piece not found")
	ErrGenerationNotFound = errors.New("generation not found")
)

// ─── Types ──────────────────────────────────────────────────────

// Event is a real-world production (concert, gala, festival...).
type Event struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	Metadata     string     `json:"metadata"`
	Venue        string     `json:"venue"`
	StartsAt     *time.Time `json:"starts_at,omitempty"`
	EndsAt       *time.Time `json:"ends_at,omitempty"`
	Status       string     `json:"status"`
	Active       bool       `json:"active"`
	ProgramCount int        `json:"program_count"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
}

type CreateEventRequest struct {
	Name        string     `json:"name" binding:"required"`
	Description string     `json:"description"`
	Metadata    string     `json:"metadata"`
	Venue       string     `json:"venue"`
	StartsAt    *time.Time `json:"starts_at"`
	EndsAt      *time.Time `json:"ends_at"`
}

type UpdateEventRequest struct {
	Name        *string    `json:"name"`
	Description *string    `json:"description"`
	Metadata    *string    `json:"metadata"`
	Venue       *string    `json:"venue"`
	StartsAt    *time.Time `json:"starts_at"`
	EndsAt      *time.Time `json:"ends_at"`
	Status      *string    `json:"status"`
	Active      *bool      `json:"active"`
}

// Program is a run-of-show block inside an event.
type Program struct {
	ID          string     `json:"id"`
	EventID     string     `json:"event_id"`
	Number      int        `json:"number"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	SortOrder   int        `json:"sort_order"`
	Active      bool       `json:"active"`
	PieceCount  int        `json:"piece_count"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
}

type CreateProgramRequest struct {
	Number      int        `json:"number" binding:"required"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	ScheduledAt *time.Time `json:"scheduled_at"`
	SortOrder   *int       `json:"sort_order"`
}

type UpdateProgramRequest struct {
	Number      *int       `json:"number"`
	Name        *string    `json:"name"`
	Description *string    `json:"description"`
	ScheduledAt *time.Time `json:"scheduled_at"`
	SortOrder   *int       `json:"sort_order"`
	Active      *bool      `json:"active"`
}

// Piece is one deliverable unit (opening video, flyer, lower third...).
type Piece struct {
	ID           string     `json:"id"`
	EventID      string     `json:"event_id"`
	ProgramID    string     `json:"program_id,omitempty"`
	Number       int        `json:"number"`
	PieceCode    string     `json:"piece_code"`
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	Type         string     `json:"type"`
	OutputFormat string     `json:"output_format"`
	Duration     int        `json:"duration"`
	AspectRatio  string     `json:"aspect_ratio"`
	Active       bool       `json:"active"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
}

type CreatePieceRequest struct {
	ProgramID    string `json:"program_id"`
	Number       int    `json:"number" binding:"required"`
	PieceCode    string `json:"piece_code"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Type         string `json:"type"`
	OutputFormat string `json:"output_format"`
	Duration     int    `json:"duration"`
	AspectRatio  string `json:"aspect_ratio"`
}

type UpdatePieceRequest struct {
	ProgramID    *string `json:"program_id"`
	Number       *int    `json:"number"`
	PieceCode    *string `json:"piece_code"`
	Name         *string `json:"name"`
	Description  *string `json:"description"`
	Type         *string `json:"type"`
	OutputFormat *string `json:"output_format"`
	Duration     *int    `json:"duration"`
	AspectRatio  *string `json:"aspect_ratio"`
	Active       *bool   `json:"active"`
}

// PieceGeneration is one attempt at rendering a piece.
type PieceGeneration struct {
	ID             string     `json:"id"`
	PieceID        string     `json:"piece_id"`
	Number         int        `json:"number"`
	VideoURL       string     `json:"video_url"`
	VideoLocalURL  string     `json:"video_local_url"`
	Status         string     `json:"status"`
	Active         bool       `json:"active"`
	Final          bool       `json:"final"`
	FinalizedAt    *time.Time `json:"finalized_at,omitempty"`
	TaskID         string     `json:"task_id,omitempty"`
	Rating         int        `json:"rating"`
	RequestPayload string     `json:"request_payload,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
}

type CreateGenerationRequest struct {
	Number int    `json:"number" binding:"required"`
	Status string `json:"status"`
}

type UpdateGenerationRequest struct {
	VideoURL      *string `json:"video_url"`
	VideoLocalURL *string `json:"video_local_url"`
	Status        *string `json:"status"`
	Active        *bool   `json:"active"`
	Final         *bool   `json:"final"`
	TaskID        *string `json:"task_id"`
	Rating        *int    `json:"rating"`
}

// Combined tree response.
type EventWithPrograms struct {
	Event    Event               `json:"event"`
	Programs []ProgramWithPieces `json:"programs"`
}

type ProgramWithPieces struct {
	Program Program `json:"program"`
	Pieces  []Piece `json:"pieces"`
}

// ─── Store ──────────────────────────────────────────────────────

// Store persists the event domain in the tenant schema.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const eventCols = `e.id, e.name, COALESCE(e.description,'') AS description,
	COALESCE(e.metadata,'') AS metadata, COALESCE(e.venue,'') AS venue,
	e.starts_at, e.ends_at, e.status, e.active,
	(SELECT COUNT(*) FROM programs p WHERE p.event_id = e.id AND p.deleted_at IS NULL) AS program_count,
	e.created_at, e.updated_at, e.deleted_at`

func scanEvent(ev *Event, sc interface {
	Scan(dest ...interface{}) error
}) error {
	return sc.Scan(&ev.ID, &ev.Name, &ev.Description, &ev.Metadata, &ev.Venue,
		&ev.StartsAt, &ev.EndsAt, &ev.Status, &ev.Active, &ev.ProgramCount,
		&ev.CreatedAt, &ev.UpdatedAt, &ev.DeletedAt)
}

func (s *Store) CreateEvent(ev *Event) error {
	query := `INSERT INTO events (id, name, description, metadata, venue, starts_at, ends_at, status, active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING created_at, updated_at`
	return s.db.QueryRow(query, ev.ID, ev.Name, ev.Description,
		nullIfEmpty(ev.Metadata), ev.Venue, ev.StartsAt, ev.EndsAt,
		ev.Status, ev.Active).Scan(&ev.CreatedAt, &ev.UpdatedAt)
}

func (s *Store) GetEventByID(id string) (*Event, error) {
	ev := &Event{}
	err := scanEvent(ev, s.db.QueryRow(`SELECT `+eventCols+` FROM events e WHERE e.id = $1 AND e.deleted_at IS NULL`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return ev, err
}

func (s *Store) ListEvents(activeOnly bool) ([]Event, error) {
	where := "WHERE e.deleted_at IS NULL"
	if activeOnly {
		where += " AND e.active = true"
	}
	rows, err := s.db.Query(`SELECT ` + eventCols + ` FROM events e ` + where + ` ORDER BY e.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var ev Event
		if err := scanEvent(&ev, rows); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *Store) UpdateEvent(id string, updates map[string]interface{}) error {
	return applyUpdates(s.db, "events", id, updates, "event not found")
}

func (s *Store) SoftDeleteEvent(id string) error {
	result, err := s.db.Exec(`UPDATE events SET deleted_at = NOW(), updated_at = NOW(), active = false
		WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrEventNotFound
	}
	return nil
}

const programCols = `p.id, p.event_id, p.number, p.name, COALESCE(p.description,'') AS description,
	p.scheduled_at, p.sort_order, p.active,
	(SELECT COUNT(*) FROM pieces pc WHERE pc.program_id = p.id AND pc.deleted_at IS NULL) AS piece_count,
	p.created_at, p.updated_at, p.deleted_at`

func scanProgram(pr *Program, sc interface {
	Scan(dest ...interface{}) error
}) error {
	return sc.Scan(&pr.ID, &pr.EventID, &pr.Number, &pr.Name, &pr.Description,
		&pr.ScheduledAt, &pr.SortOrder, &pr.Active, &pr.PieceCount,
		&pr.CreatedAt, &pr.UpdatedAt, &pr.DeletedAt)
}

func (s *Store) CreateProgram(pr *Program) error {
	query := `INSERT INTO programs (id, event_id, number, name, description, scheduled_at, sort_order, active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING created_at, updated_at`
	return s.db.QueryRow(query, pr.ID, pr.EventID, pr.Number, pr.Name,
		pr.Description, pr.ScheduledAt, pr.SortOrder, pr.Active).
		Scan(&pr.CreatedAt, &pr.UpdatedAt)
}

func (s *Store) GetProgramByID(id string) (*Program, error) {
	pr := &Program{}
	err := scanProgram(pr, s.db.QueryRow(`SELECT `+programCols+` FROM programs p WHERE p.id = $1 AND p.deleted_at IS NULL`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return pr, err
}

func (s *Store) ListPrograms(eventID string) ([]Program, error) {
	rows, err := s.db.Query(`SELECT `+programCols+` FROM programs p
		WHERE p.event_id = $1 AND p.deleted_at IS NULL ORDER BY p.sort_order, p.number ASC`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Program
	for rows.Next() {
		var pr Program
		if err := scanProgram(&pr, rows); err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	return out, rows.Err()
}

func (s *Store) UpdateProgram(id string, updates map[string]interface{}) error {
	return applyUpdates(s.db, "programs", id, updates, "program not found")
}

func (s *Store) SoftDeleteProgram(id string) error {
	result, err := s.db.Exec(`UPDATE programs SET deleted_at = NOW(), updated_at = NOW(), active = false
		WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrProgramNotFound
	}
	return nil
}

const pieceCols = `pc.id, pc.event_id, COALESCE(pc.program_id::text,'') AS program_id, pc.number,
	pc.piece_code, pc.name, COALESCE(pc.description,'') AS description, pc.type,
	pc.output_format, pc.duration, pc.aspect_ratio, pc.active,
	pc.created_at, pc.updated_at, pc.deleted_at`

func scanPiece(pc *Piece, sc interface {
	Scan(dest ...interface{}) error
}) error {
	return sc.Scan(&pc.ID, &pc.EventID, &pc.ProgramID, &pc.Number, &pc.PieceCode, &pc.Name,
		&pc.Description, &pc.Type, &pc.OutputFormat, &pc.Duration, &pc.AspectRatio, &pc.Active,
		&pc.CreatedAt, &pc.UpdatedAt, &pc.DeletedAt)
}

func (s *Store) CreatePiece(pc *Piece) error {
	query := `INSERT INTO pieces (id, event_id, program_id, number, piece_code, name, description, type, output_format, duration, aspect_ratio, active)
		VALUES ($1, $2, NULLIF($3,'')::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING created_at, updated_at`
	return s.db.QueryRow(query, pc.ID, pc.EventID, pc.ProgramID, pc.Number, pc.PieceCode,
		pc.Name, pc.Description, pc.Type,
		pc.OutputFormat, pc.Duration, pc.AspectRatio, pc.Active).
		Scan(&pc.CreatedAt, &pc.UpdatedAt)
}

func (s *Store) GetPieceByID(id string) (*Piece, error) {
	pc := &Piece{}
	err := scanPiece(pc, s.db.QueryRow(`SELECT `+pieceCols+` FROM pieces pc WHERE pc.id = $1 AND pc.deleted_at IS NULL`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return pc, err
}

func (s *Store) ListPieces(eventID, programID string) ([]Piece, error) {
	where := "WHERE pc.deleted_at IS NULL"
	args := []interface{}{}
	idx := 1
	if eventID != "" {
		where += fmt.Sprintf(" AND pc.event_id = $%d", idx)
		args = append(args, eventID)
		idx++
	}
	if programID != "" {
		where += fmt.Sprintf(" AND pc.program_id = $%d", idx)
		args = append(args, programID)
		idx++
	}
	rows, err := s.db.Query(`SELECT `+pieceCols+` FROM pieces pc `+where+` ORDER BY pc.number ASC, pc.created_at ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Piece
	for rows.Next() {
		var pc Piece
		if err := scanPiece(&pc, rows); err != nil {
			return nil, err
		}
		out = append(out, pc)
	}
	return out, rows.Err()
}

func (s *Store) UpdatePiece(id string, updates map[string]interface{}) error {
	if val, ok := updates["program_id"]; ok {
		if str, ok := val.(string); ok && str == "" {
			updates["program_id"] = nil // empty → detach
		}
	}
	return applyUpdates(s.db, "pieces", id, updates, "piece not found")
}

func (s *Store) SoftDeletePiece(id string) error {
	result, err := s.db.Exec(`UPDATE pieces SET deleted_at = NOW(), updated_at = NOW(), active = false
		WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrPieceNotFound
	}
	return nil
}

const genCols = `g.id, g.piece_id, g.number, g.video_url, g.video_local_url,
	COALESCE(g.status,'pending') AS status, g.active, g.final, g.finalized_at,
	COALESCE(g.task_id,'') AS task_id, COALESCE(g.rating,0) AS rating,
	COALESCE(gl.request_payload,'') AS request_payload,
	g.created_at, g.updated_at, g.deleted_at`

const genFrom = `FROM piece_generations g
	LEFT JOIN generation_logs gl ON gl.task_id = g.task_id AND gl.deleted_at IS NULL`

func scanGeneration(g *PieceGeneration, sc interface {
	Scan(dest ...interface{}) error
}) error {
	return sc.Scan(&g.ID, &g.PieceID, &g.Number, &g.VideoURL, &g.VideoLocalURL, &g.Status,
		&g.Active, &g.Final, &g.FinalizedAt, &g.TaskID, &g.Rating, &g.RequestPayload,
		&g.CreatedAt, &g.UpdatedAt, &g.DeletedAt)
}

func (s *Store) CreateGeneration(g *PieceGeneration) error {
	query := `INSERT INTO piece_generations (id, piece_id, number, video_url, video_local_url, status, active, task_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8,''))
		RETURNING created_at, updated_at`
	return s.db.QueryRow(query, g.ID, g.PieceID, g.Number, nullIfEmpty(g.VideoURL),
		nullIfEmpty(g.VideoLocalURL), g.Status, g.Active, g.TaskID).
		Scan(&g.CreatedAt, &g.UpdatedAt)
}

func (s *Store) GetGenerationByID(id string) (*PieceGeneration, error) {
	g := &PieceGeneration{}
	err := scanGeneration(g, s.db.QueryRow(`SELECT `+genCols+` `+genFrom+` WHERE g.id = $1 AND g.deleted_at IS NULL`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return g, err
}

func (s *Store) ListGenerations(pieceID string) ([]PieceGeneration, error) {
	rows, err := s.db.Query(`SELECT `+genCols+` `+genFrom+`
		WHERE g.piece_id = $1 AND g.deleted_at IS NULL ORDER BY g.number ASC, g.created_at DESC`, pieceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PieceGeneration
	for rows.Next() {
		var g PieceGeneration
		if err := scanGeneration(&g, rows); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ListActiveGenerations returns only active generations (one per number).
func (s *Store) ListActiveGenerations(pieceID string) ([]PieceGeneration, error) {
	rows, err := s.db.Query(`SELECT `+genCols+` `+genFrom+`
		WHERE g.piece_id = $1 AND g.deleted_at IS NULL AND g.active = true ORDER BY g.number ASC`, pieceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PieceGeneration
	for rows.Next() {
		var g PieceGeneration
		if err := scanGeneration(&g, rows); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetActiveGenerationByNumber returns the active generation for a slot, or nil.
func (s *Store) GetActiveGenerationByNumber(pieceID string, number int) (*PieceGeneration, error) {
	g := &PieceGeneration{}
	err := scanGeneration(g, s.db.QueryRow(`SELECT `+genCols+` `+genFrom+`
		WHERE g.piece_id = $1 AND g.number = $2 AND g.deleted_at IS NULL AND g.active = true`, pieceID, number))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return g, err
}

// DeactivateGenerationsByNumber frees a generation slot (before a new attempt).
func (s *Store) DeactivateGenerationsByNumber(pieceID string, number int) error {
	_, err := s.db.Exec(`UPDATE piece_generations SET active = false, updated_at = NOW()
		WHERE piece_id = $1 AND number = $2 AND deleted_at IS NULL AND active = true`, pieceID, number)
	return err
}

// DeactivateFinalsByNumber clears the final flag in a slot (before re-finalizing).
func (s *Store) DeactivateFinalsByNumber(pieceID string, number int) error {
	_, err := s.db.Exec(`UPDATE piece_generations SET final = false, finalized_at = NULL, updated_at = NOW()
		WHERE piece_id = $1 AND number = $2 AND deleted_at IS NULL AND final = true`, pieceID, number)
	return err
}

// GetPendingGenerationByNumber finds a pending generation in a slot regardless
// of the active flag (used by the generation flow).
func (s *Store) GetPendingGenerationByNumber(pieceID string, number int) (*PieceGeneration, error) {
	g := &PieceGeneration{}
	err := scanGeneration(g, s.db.QueryRow(`SELECT `+genCols+` `+genFrom+`
		WHERE g.piece_id = $1 AND g.number = $2 AND g.deleted_at IS NULL AND g.status = 'pending'
		ORDER BY g.created_at DESC LIMIT 1`, pieceID, number))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return g, err
}

func (s *Store) UpdateGeneration(id string, updates map[string]interface{}) error {
	if val, ok := updates["task_id"]; ok {
		if str, ok := val.(string); ok && str == "" {
			return fmt.Errorf("task_id cannot be empty")
		}
	}
	return applyUpdates(s.db, "piece_generations", id, updates, "generation not found")
}

func (s *Store) SoftDeleteGeneration(id string) error {
	result, err := s.db.Exec(`UPDATE piece_generations SET deleted_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrGenerationNotFound
	}
	return nil
}

// ─── Shared helpers ─────────────────────────────────────────────

func applyUpdates(db *sql.DB, table, id string, updates map[string]interface{}, notFound string) error {
	if len(updates) == 0 {
		return nil
	}
	query := "UPDATE " + table + " SET updated_at = NOW()"
	args := []interface{}{}
	argIdx := 1
	for col, val := range updates {
		query += fmt.Sprintf(", %s = $%d", col, argIdx)
		args = append(args, val)
		argIdx++
	}
	query += fmt.Sprintf(" WHERE id = $%d AND deleted_at IS NULL", argIdx)
	args = append(args, id)

	result, err := db.Exec(query, args...)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return errors.New(notFound)
	}
	return nil
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// ─── Service ────────────────────────────────────────────────────

// Service implements the event domain logic.
type Service struct {
	store *Store
}

func NewService(store *Store) *Service { return &Service{store: store} }

func (s *Service) CreateEvent(req *CreateEventRequest) (*Event, error) {
	ev := &Event{
		ID: uuid.New().String(), Name: req.Name, Description: req.Description,
		Metadata: req.Metadata, Venue: req.Venue, StartsAt: req.StartsAt, EndsAt: req.EndsAt,
		Status: "draft", Active: true,
	}
	if err := s.store.CreateEvent(ev); err != nil {
		return nil, err
	}
	return ev, nil
}

func (s *Service) GetEvent(id string) (*Event, error) { return s.store.GetEventByID(id) }

func (s *Service) ListEvents(activeOnly bool) ([]Event, error) {
	events, err := s.store.ListEvents(activeOnly)
	if err != nil {
		return nil, err
	}
	if events == nil {
		events = []Event{}
	}
	return events, nil
}

func (s *Service) UpdateEvent(id string, req *UpdateEventRequest) (*Event, error) {
	updates := map[string]interface{}{}
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.Description != nil {
		updates["description"] = *req.Description
	}
	if req.Metadata != nil {
		updates["metadata"] = *req.Metadata
	}
	if req.Venue != nil {
		updates["venue"] = *req.Venue
	}
	if req.StartsAt != nil {
		updates["starts_at"] = *req.StartsAt
	}
	if req.EndsAt != nil {
		updates["ends_at"] = *req.EndsAt
	}
	if req.Status != nil {
		updates["status"] = *req.Status
	}
	if req.Active != nil {
		updates["active"] = *req.Active
	}
	if err := s.store.UpdateEvent(id, updates); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, ErrEventNotFound
		}
		return nil, err
	}
	return s.store.GetEventByID(id)
}

func (s *Service) SoftDeleteEvent(id string) error { return s.store.SoftDeleteEvent(id) }

// GetEventWithPrograms returns the full run-of-show tree.
func (s *Service) GetEventWithPrograms(id string) (*EventWithPrograms, error) {
	ev, err := s.store.GetEventByID(id)
	if err != nil {
		return nil, err
	}
	if ev == nil {
		return nil, ErrEventNotFound
	}
	programs, err := s.store.ListPrograms(id)
	if err != nil {
		return nil, err
	}
	result := &EventWithPrograms{Event: *ev, Programs: []ProgramWithPieces{}}
	for _, pr := range programs {
		pieces, err := s.store.ListPieces("", pr.ID)
		if err != nil {
			return nil, err
		}
		if pieces == nil {
			pieces = []Piece{}
		}
		result.Programs = append(result.Programs, ProgramWithPieces{Program: pr, Pieces: pieces})
	}
	return result, nil
}

func (s *Service) CreateProgram(eventID string, req *CreateProgramRequest) (*Program, error) {
	ev, err := s.store.GetEventByID(eventID)
	if err != nil {
		return nil, err
	}
	if ev == nil {
		return nil, ErrEventNotFound
	}
	sortOrder := req.Number
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}
	pr := &Program{
		ID: uuid.New().String(), EventID: eventID, Number: req.Number,
		Name: req.Name, Description: req.Description, ScheduledAt: req.ScheduledAt,
		SortOrder: sortOrder, Active: true,
	}
	if err := s.store.CreateProgram(pr); err != nil {
		return nil, err
	}
	return pr, nil
}

func (s *Service) GetProgram(id string) (*Program, error) { return s.store.GetProgramByID(id) }

func (s *Service) ListPrograms(eventID string) ([]Program, error) {
	prs, err := s.store.ListPrograms(eventID)
	if err != nil {
		return nil, err
	}
	if prs == nil {
		prs = []Program{}
	}
	return prs, nil
}

func (s *Service) UpdateProgram(id string, req *UpdateProgramRequest) (*Program, error) {
	updates := map[string]interface{}{}
	if req.Number != nil {
		updates["number"] = *req.Number
	}
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.Description != nil {
		updates["description"] = *req.Description
	}
	if req.ScheduledAt != nil {
		updates["scheduled_at"] = *req.ScheduledAt
	}
	if req.SortOrder != nil {
		updates["sort_order"] = *req.SortOrder
	}
	if req.Active != nil {
		updates["active"] = *req.Active
	}
	if err := s.store.UpdateProgram(id, updates); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, ErrProgramNotFound
		}
		return nil, err
	}
	return s.store.GetProgramByID(id)
}

func (s *Service) SoftDeleteProgram(id string) error { return s.store.SoftDeleteProgram(id) }

func (s *Service) CreatePiece(eventID string, req *CreatePieceRequest) (*Piece, error) {
	// Validate the program belongs to the event when provided.
	if req.ProgramID != "" {
		pr, err := s.store.GetProgramByID(req.ProgramID)
		if err != nil {
			return nil, err
		}
		if pr == nil || pr.EventID != eventID {
			return nil, ErrProgramNotFound
		}
	}
	pieceType := req.Type
	if pieceType == "" {
		pieceType = "custom"
	}
	pc := &Piece{
		ID: uuid.New().String(), EventID: eventID, ProgramID: req.ProgramID,
		Number: req.Number, PieceCode: req.PieceCode, Name: req.Name,
		Description: req.Description, Type: pieceType, OutputFormat: req.OutputFormat,
		Duration: req.Duration, AspectRatio: req.AspectRatio, Active: true,
	}
	if err := s.store.CreatePiece(pc); err != nil {
		return nil, err
	}
	return pc, nil
}

func (s *Service) GetPiece(id string) (*Piece, error) { return s.store.GetPieceByID(id) }

func (s *Service) ListPieces(eventID, programID string) ([]Piece, error) {
	pieces, err := s.store.ListPieces(eventID, programID)
	if err != nil {
		return nil, err
	}
	if pieces == nil {
		pieces = []Piece{}
	}
	return pieces, nil
}

func (s *Service) UpdatePiece(id string, req *UpdatePieceRequest) (*Piece, error) {
	updates := map[string]interface{}{}
	if req.ProgramID != nil {
		updates["program_id"] = *req.ProgramID
	}
	if req.Number != nil {
		updates["number"] = *req.Number
	}
	if req.PieceCode != nil {
		updates["piece_code"] = *req.PieceCode
	}
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.Description != nil {
		updates["description"] = *req.Description
	}
	if req.Type != nil {
		updates["type"] = *req.Type
	}
	if req.OutputFormat != nil {
		updates["output_format"] = *req.OutputFormat
	}
	if req.Duration != nil {
		updates["duration"] = *req.Duration
	}
	if req.AspectRatio != nil {
		updates["aspect_ratio"] = *req.AspectRatio
	}
	if req.Active != nil {
		updates["active"] = *req.Active
	}
	if err := s.store.UpdatePiece(id, updates); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, ErrPieceNotFound
		}
		return nil, err
	}
	return s.store.GetPieceByID(id)
}

func (s *Service) SoftDeletePiece(id string) error { return s.store.SoftDeletePiece(id) }

// CreateGeneration reserves a generation slot on a piece.
func (s *Service) CreateGeneration(pieceID string, req *CreateGenerationRequest) (*PieceGeneration, error) {
	pc, err := s.store.GetPieceByID(pieceID)
	if err != nil {
		return nil, err
	}
	if pc == nil {
		return nil, ErrPieceNotFound
	}
	status := req.Status
	if status == "" {
		status = "pending"
	}
	g := &PieceGeneration{
		ID: uuid.New().String(), PieceID: pieceID, Number: req.Number,
		Status: status, Active: true,
	}
	if err := s.store.CreateGeneration(g); err != nil {
		return nil, err
	}
	return g, nil
}

func (s *Service) GetGeneration(id string) (*PieceGeneration, error) {
	return s.store.GetGenerationByID(id)
}

func (s *Service) ListGenerations(pieceID string) ([]PieceGeneration, error) {
	pc, err := s.store.GetPieceByID(pieceID)
	if err != nil {
		return nil, err
	}
	if pc == nil {
		return nil, ErrPieceNotFound
	}
	gens, err := s.store.ListGenerations(pieceID)
	if err != nil {
		return nil, err
	}
	if gens == nil {
		gens = []PieceGeneration{}
	}
	return gens, nil
}

func (s *Service) UpdateGeneration(id string, req *UpdateGenerationRequest) (*PieceGeneration, error) {
	updates := map[string]interface{}{}
	if req.VideoURL != nil {
		updates["video_url"] = *req.VideoURL
	}
	if req.VideoLocalURL != nil {
		updates["video_local_url"] = *req.VideoLocalURL
	}
	if req.Status != nil {
		updates["status"] = *req.Status
	}
	if req.Active != nil {
		updates["active"] = *req.Active
	}
	if req.Final != nil {
		updates["final"] = *req.Final
		if *req.Final {
			now := time.Now()
			updates["finalized_at"] = now
		}
	}
	if req.TaskID != nil {
		updates["task_id"] = *req.TaskID
	}
	if req.Rating != nil {
		updates["rating"] = *req.Rating
	}
	if err := s.store.UpdateGeneration(id, updates); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, ErrGenerationNotFound
		}
		return nil, err
	}
	return s.store.GetGenerationByID(id)
}

func (s *Service) SoftDeleteGeneration(id string) error { return s.store.SoftDeleteGeneration(id) }

// ─── HTTP ───────────────────────────────────────────────────────

// Handler exposes event endpoints.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// ─── Events ─────────────────────────────────────────────────────

// Create handles POST /events
func (h *Handler) Create(c *gin.Context) {
	var req CreateEventRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	ev, err := h.svc.CreateEvent(&req)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Created(c, ev)
}

// List handles GET /events
func (h *Handler) List(c *gin.Context) {
	events, err := h.svc.ListEvents(c.Query("all") != "true")
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, events)
}

// GetByID handles GET /events/:id (returns the full tree)
func (h *Handler) GetByID(c *gin.Context) {
	result, err := h.svc.GetEventWithPrograms(c.Param("id"))
	if err != nil {
		if errors.Is(err, ErrEventNotFound) {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, result)
}

// Update handles PATCH /events/:id
func (h *Handler) Update(c *gin.Context) {
	var req UpdateEventRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	ev, err := h.svc.UpdateEvent(c.Param("id"), &req)
	if err != nil {
		if errors.Is(err, ErrEventNotFound) {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, ev)
}

// SoftDelete handles DELETE /events/:id
func (h *Handler) SoftDelete(c *gin.Context) {
	if err := h.svc.SoftDeleteEvent(c.Param("id")); err != nil {
		if errors.Is(err, ErrEventNotFound) {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "event deleted")
}

// ─── Programs ───────────────────────────────────────────────────

// CreateProgram handles POST /events/:id/programs
func (h *Handler) CreateProgram(c *gin.Context) {
	var req CreateProgramRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	pr, err := h.svc.CreateProgram(c.Param("id"), &req)
	if err != nil {
		if errors.Is(err, ErrEventNotFound) {
			utils.NotFound(c, err.Error())
			return
		}
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			utils.BadRequest(c, "program number already exists for this event")
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Created(c, pr)
}

// ListPrograms handles GET /events/:id/programs
func (h *Handler) ListPrograms(c *gin.Context) {
	prs, err := h.svc.ListPrograms(c.Param("id"))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, prs)
}

// GetProgram handles GET /programs/:id
func (h *Handler) GetProgram(c *gin.Context) {
	pr, err := h.svc.GetProgram(c.Param("id"))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if pr == nil {
		utils.NotFound(c, "program not found")
		return
	}
	utils.Success(c, pr)
}

// UpdateProgram handles PATCH /programs/:id
func (h *Handler) UpdateProgram(c *gin.Context) {
	var req UpdateProgramRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	pr, err := h.svc.UpdateProgram(c.Param("id"), &req)
	if err != nil {
		if errors.Is(err, ErrProgramNotFound) {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, pr)
}

// SoftDeleteProgram handles DELETE /programs/:id
func (h *Handler) SoftDeleteProgram(c *gin.Context) {
	if err := h.svc.SoftDeleteProgram(c.Param("id")); err != nil {
		if errors.Is(err, ErrProgramNotFound) {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "program deleted")
}

// ─── Pieces ─────────────────────────────────────────────────────

// CreatePiece handles POST /events/:id/pieces
func (h *Handler) CreatePiece(c *gin.Context) {
	var req CreatePieceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	pc, err := h.svc.CreatePiece(c.Param("id"), &req)
	if err != nil {
		if errors.Is(err, ErrEventNotFound) || errors.Is(err, ErrProgramNotFound) {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Created(c, pc)
}

// ListPieces handles GET /pieces?event_id=&program_id=
func (h *Handler) ListPieces(c *gin.Context) {
	pieces, err := h.svc.ListPieces(c.Query("event_id"), c.Query("program_id"))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, pieces)
}

// GetPiece handles GET /pieces/:id
func (h *Handler) GetPiece(c *gin.Context) {
	pc, err := h.svc.GetPiece(c.Param("id"))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if pc == nil {
		utils.NotFound(c, "piece not found")
		return
	}
	utils.Success(c, pc)
}

// UpdatePiece handles PATCH /pieces/:id
func (h *Handler) UpdatePiece(c *gin.Context) {
	var req UpdatePieceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	pc, err := h.svc.UpdatePiece(c.Param("id"), &req)
	if err != nil {
		if errors.Is(err, ErrPieceNotFound) {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, pc)
}

// SoftDeletePiece handles DELETE /pieces/:id
func (h *Handler) SoftDeletePiece(c *gin.Context) {
	if err := h.svc.SoftDeletePiece(c.Param("id")); err != nil {
		if errors.Is(err, ErrPieceNotFound) {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "piece deleted")
}

// ─── Generations ────────────────────────────────────────────────

// CreateGeneration handles POST /pieces/:id/generations
func (h *Handler) CreateGeneration(c *gin.Context) {
	var req CreateGenerationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	g, err := h.svc.CreateGeneration(c.Param("id"), &req)
	if err != nil {
		if errors.Is(err, ErrPieceNotFound) {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Created(c, g)
}

// ListGenerations handles GET /pieces/:id/generations
func (h *Handler) ListGenerations(c *gin.Context) {
	gens, err := h.svc.ListGenerations(c.Param("id"))
	if err != nil {
		if errors.Is(err, ErrPieceNotFound) {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, gens)
}

// GetGeneration handles GET /generations/:id
func (h *Handler) GetGeneration(c *gin.Context) {
	g, err := h.svc.GetGeneration(c.Param("id"))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if g == nil {
		utils.NotFound(c, "generation not found")
		return
	}
	utils.Success(c, g)
}

// UpdateGeneration handles PATCH /generations/:id
func (h *Handler) UpdateGeneration(c *gin.Context) {
	var req UpdateGenerationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	g, err := h.svc.UpdateGeneration(c.Param("id"), &req)
	if err != nil {
		if errors.Is(err, ErrGenerationNotFound) {
			utils.NotFound(c, err.Error())
			return
		}
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Success(c, g)
}

// SoftDeleteGeneration handles DELETE /generations/:id
func (h *Handler) SoftDeleteGeneration(c *gin.Context) {
	if err := h.svc.SoftDeleteGeneration(c.Param("id")); err != nil {
		if errors.Is(err, ErrGenerationNotFound) {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "generation deleted")
}

// ─── Module ─────────────────────────────────────────────────────

// HandlerResolver supplies the tenant-scoped handler for each request.
type HandlerResolver func(c *gin.Context) (*Handler, bool)

// Module registers event routes.
type Module struct {
	resolve HandlerResolver
}

func NewModule(resolve HandlerResolver) *Module { return &Module{resolve: resolve} }

func (m *Module) Name() string { return "events" }

func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, _ gin.HandlerFunc) {
	ev := rg.Group("/events")
	ev.Use(authMw, tenantMw)
	{
		ev.POST("", h(m.resolve)("Create"))
		ev.GET("", h(m.resolve)("List"))
		ev.GET("/:id", h(m.resolve)("GetByID"))
		ev.PATCH("/:id", h(m.resolve)("Update"))
		ev.DELETE("/:id", h(m.resolve)("SoftDelete"))

		ev.POST("/:id/programs", h(m.resolve)("CreateProgram"))
		ev.GET("/:id/programs", h(m.resolve)("ListPrograms"))
		ev.POST("/:id/pieces", h(m.resolve)("CreatePiece"))
	}

	pr := rg.Group("/programs")
	pr.Use(authMw, tenantMw)
	{
		pr.GET("/:id", h(m.resolve)("GetProgram"))
		pr.PATCH("/:id", h(m.resolve)("UpdateProgram"))
		pr.DELETE("/:id", h(m.resolve)("SoftDeleteProgram"))
	}

	pc := rg.Group("/pieces")
	pc.Use(authMw, tenantMw)
	{
		pc.GET("", h(m.resolve)("ListPieces"))
		pc.GET("/:id", h(m.resolve)("GetPiece"))
		pc.PATCH("/:id", h(m.resolve)("UpdatePiece"))
		pc.DELETE("/:id", h(m.resolve)("SoftDeletePiece"))
		pc.POST("/:id/generations", h(m.resolve)("CreateGeneration"))
		pc.GET("/:id/generations", h(m.resolve)("ListGenerations"))
	}

	gen := rg.Group("/generations")
	gen.Use(authMw, tenantMw)
	{
		gen.GET("/:id", h(m.resolve)("GetGeneration"))
		gen.PATCH("/:id", h(m.resolve)("UpdateGeneration"))
		gen.DELETE("/:id", h(m.resolve)("SoftDeleteGeneration"))
	}
}

// dispatch adapts a resolver into per-method gin handlers without a manual
// wrapper per endpoint.
func h(resolve HandlerResolver) func(method string) gin.HandlerFunc {
	return func(method string) gin.HandlerFunc {
		return func(c *gin.Context) {
			hdl, ok := resolve(c)
			if !ok {
				return
			}
			switch method {
			case "Create":
				hdl.Create(c)
			case "List":
				hdl.List(c)
			case "GetByID":
				hdl.GetByID(c)
			case "Update":
				hdl.Update(c)
			case "SoftDelete":
				hdl.SoftDelete(c)
			case "CreateProgram":
				hdl.CreateProgram(c)
			case "ListPrograms":
				hdl.ListPrograms(c)
			case "GetProgram":
				hdl.GetProgram(c)
			case "UpdateProgram":
				hdl.UpdateProgram(c)
			case "SoftDeleteProgram":
				hdl.SoftDeleteProgram(c)
			case "CreatePiece":
				hdl.CreatePiece(c)
			case "ListPieces":
				hdl.ListPieces(c)
			case "GetPiece":
				hdl.GetPiece(c)
			case "UpdatePiece":
				hdl.UpdatePiece(c)
			case "SoftDeletePiece":
				hdl.SoftDeletePiece(c)
			case "CreateGeneration":
				hdl.CreateGeneration(c)
			case "ListGenerations":
				hdl.ListGenerations(c)
			case "GetGeneration":
				hdl.GetGeneration(c)
			case "UpdateGeneration":
				hdl.UpdateGeneration(c)
			case "SoftDeleteGeneration":
				hdl.SoftDeleteGeneration(c)
			default:
				utils.InternalError(c, "unknown handler method: "+method)
			}
		}
	}
}

// SaveGenerationOutput persists a completed generation into its piece slot
// (used by the agency core as its GenerationSaver).
func (s *Service) SaveGenerationOutput(pieceID string, number int, videoURL, localURL, taskID string) error {
	if err := s.store.DeactivateGenerationsByNumber(pieceID, number); err != nil {
		return err
	}
	g := &PieceGeneration{
		ID: uuid.New().String(), PieceID: pieceID, Number: number,
		VideoURL: videoURL, VideoLocalURL: localURL, Status: config.STATUS_SUCCESS,
		Active: true, TaskID: taskID,
	}
	return s.store.CreateGeneration(g)
}
