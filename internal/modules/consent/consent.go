// Package consent records user acceptance of legal documents.
//
// Ley 1581 de 2012 (Colombia) requires the data controller to request and
// keep proof of each data subject's prior, express and informed
// authorization, retrievable on later consultation (arts. 9, 12, 17).
// Every acceptance stores who, when, how and which document version.
package consent

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"synapta/internal/utils"
)

// Allowed document types and acceptance methods.
var (
	allowedDocs    = map[string]bool{"terms": true, "privacy": true}
	allowedMethods = map[string]bool{"first-login": true, "admin-assisted": true, "register": true}
)

// Consent is one recorded acceptance.
type Consent struct {
	ID         int64     `json:"id"`
	UserID     int64     `json:"user_id"`
	DocType    string    `json:"doc_type"`
	DocVersion string    `json:"doc_version"`
	AcceptedAt time.Time `json:"accepted_at"`
	Method     string    `json:"method"`
}

// AcceptRequest is the body of POST /consents. The server stamps
// accepted_at, ip and user_agent; the client only declares what and how.
type AcceptRequest struct {
	DocType    string `json:"doc_type" binding:"required"`
	DocVersion string `json:"doc_version" binding:"required"`
	Method     string `json:"method" binding:"required"`
}

// Store persists consents in the system schema.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Record inserts an acceptance, idempotently: re-accepting the same
// document version returns the original row.
func (s *Store) Record(userID int64, docType, docVersion, method, ip, userAgent string) (*Consent, error) {
	c := &Consent{}
	err := s.db.QueryRow(`
		INSERT INTO user_consents (user_id, doc_type, doc_version, method, ip, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id, doc_type, doc_version) DO NOTHING
		RETURNING id, user_id, doc_type, doc_version, accepted_at, method`,
		userID, docType, docVersion, method, ip, userAgent,
	).Scan(&c.ID, &c.UserID, &c.DocType, &c.DocVersion, &c.AcceptedAt, &c.Method)
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// Already recorded: return the original acceptance (proof preserved).
	err = s.db.QueryRow(`
		SELECT id, user_id, doc_type, doc_version, accepted_at, method
		FROM user_consents WHERE user_id = $1 AND doc_type = $2 AND doc_version = $3`,
		userID, docType, docVersion,
	).Scan(&c.ID, &c.UserID, &c.DocType, &c.DocVersion, &c.AcceptedAt, &c.Method)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// List returns every acceptance of a user, newest first.
func (s *Store) List(userID int64) ([]Consent, error) {
	rows, err := s.db.Query(`
		SELECT id, user_id, doc_type, doc_version, accepted_at, method
		FROM user_consents WHERE user_id = $1 ORDER BY accepted_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Consent{}
	for rows.Next() {
		var c Consent
		if err := rows.Scan(&c.ID, &c.UserID, &c.DocType, &c.DocVersion, &c.AcceptedAt, &c.Method); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Service validates acceptance requests.
type Service struct {
	store *Store
}

func NewService(store *Store) *Service { return &Service{store: store} }

func (s *Service) Accept(userID int64, req AcceptRequest, ip, userAgent string) (*Consent, error) {
	docType := strings.ToLower(strings.TrimSpace(req.DocType))
	version := strings.TrimSpace(req.DocVersion)
	method := strings.ToLower(strings.TrimSpace(req.Method))
	if !allowedDocs[docType] {
		return nil, errors.New("unknown doc_type (terms|privacy)")
	}
	if version == "" || len(version) > 32 {
		return nil, errors.New("doc_version required (max 32)")
	}
	if !allowedMethods[method] {
		return nil, errors.New("unknown method (first-login|admin-assisted|register)")
	}
	if len(userAgent) > 512 {
		userAgent = userAgent[:512]
	}
	return s.store.Record(userID, docType, version, method, ip, userAgent)
}

func (s *Service) ListMine(userID int64) ([]Consent, error) {
	return s.store.List(userID)
}

// Handler serves consent routes.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Accept handles POST /consents
func (h *Handler) Accept(c *gin.Context) {
	id := utils.UserIDFromContext(c)
	if id <= 0 {
		utils.Unauthorized(c, "invalid token")
		return
	}
	var req AcceptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "doc_type, doc_version and method are required")
		return
	}
	consent, err := h.svc.Accept(id, req, c.ClientIP(), c.GetHeader("User-Agent"))
	if err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Created(c, consent)
}

// ListMine handles GET /consents/me
func (h *Handler) ListMine(c *gin.Context) {
	id := utils.UserIDFromContext(c)
	if id <= 0 {
		utils.Unauthorized(c, "invalid token")
		return
	}
	consents, err := h.svc.ListMine(id)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, consents)
}

// Module registers consent routes (system-scoped: user identity, no tenant).
type Module struct {
	hdl *Handler
}

func NewModule(hdl *Handler) *Module { return &Module{hdl: hdl} }

func (m *Module) Name() string { return "consents" }

func (m *Module) Register(rg *gin.RouterGroup, authMw, _, _ gin.HandlerFunc) {
	g := rg.Group("/consents")
	g.Use(authMw)
	{
		g.POST("", m.hdl.Accept)
		g.GET("/me", m.hdl.ListMine)
	}
}
