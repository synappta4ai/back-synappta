// Package notify posts generation events (video ready/failed) to the
// tenant's external channels (Slack, Telegram, Discord, Gotify, ...) via
// shoutrrr URLs. Channel URLs carry secrets: they are sealed at rest
// (AES-256-GCM, same key as credentials) and the API only returns masks.
package notify

import (
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/containrrr/shoutrrr"
	"github.com/containrrr/shoutrrr/pkg/types"
	"github.com/gin-gonic/gin"

	"synapta/internal/secretbox"
	"synapta/internal/utils"
)

// ─── Types ──────────────────────────────────────────────────────

// Channel is a tenant notification destination.
type Channel struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Service   string    `json:"service"`
	URL       string    `json:"url,omitempty"` // plaintext only in memory, never serialized
	URLMask   string    `json:"url_mask"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ChannelRequest is the create/update payload. Create requires name,
// service and url; update falls back to the stored values for the fields
// that arrive empty (an empty URL preserves the stored secret).
type ChannelRequest struct {
	Name    string `json:"name"`
	Service string `json:"service"`
	URL     string `json:"url"`
	Enabled *bool  `json:"enabled"`
}

// ─── Store ──────────────────────────────────────────────────────

// Store persists channels in the tenant schema.
type Store struct {
	db  *sql.DB
	key []byte
}

func NewStore(db *sql.DB, encryptionKey string) *Store {
	return &Store{db: db, key: []byte(encryptionKey)}
}

// List returns all channels with masked URLs (newest first).
func (s *Store) List() ([]Channel, error) {
	rows, err := s.db.Query(`SELECT id, name, service, url, enabled, created_at, updated_at
		FROM notification_channels ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		var c Channel
		var sealed string
		if err := rows.Scan(&c.ID, &c.Name, &c.Service, &sealed, &c.Enabled, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		if plain, err := secretbox.Open(s.key, sealed); err == nil {
			c.URLMask = maskURL(plain)
		} else {
			c.URLMask = maskURL(sealed)
		}
		out = append(out, c)
	}
	if out == nil {
		out = []Channel{}
	}
	return out, rows.Err()
}

// get loads one channel with its decrypted URL.
func (s *Store) get(id string) (*Channel, error) {
	var c Channel
	var sealed string
	err := s.db.QueryRow(`SELECT id, name, service, url, enabled, created_at, updated_at
		FROM notification_channels WHERE id = $1`, id).
		Scan(&c.ID, &c.Name, &c.Service, &sealed, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	plain, err := secretbox.Open(s.key, sealed)
	if err != nil {
		return nil, fmt.Errorf("cannot decrypt channel url: %w", err)
	}
	c.URL = plain
	c.URLMask = maskURL(plain)
	return &c, nil
}

// ListEnabled returns the enabled channels with decrypted URLs.
func (s *Store) ListEnabled() ([]Channel, error) {
	rows, err := s.db.Query(`SELECT id, name, service, url, enabled, created_at, updated_at
		FROM notification_channels WHERE enabled ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		var c Channel
		var sealed string
		if err := rows.Scan(&c.ID, &c.Name, &c.Service, &sealed, &c.Enabled, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		plain, err := secretbox.Open(s.key, sealed)
		if err != nil {
			log.Printf("[notify] channel %q: cannot decrypt url, skipping", c.Name)
			continue
		}
		c.URL = plain
		c.URLMask = maskURL(plain)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Create validates and stores a channel.
func (s *Store) Create(req *ChannelRequest) (*Channel, error) {
	req.Name = strings.TrimSpace(req.Name)
	req.Service = strings.ToLower(strings.TrimSpace(req.Service))
	req.URL = strings.TrimSpace(req.URL)
	if req.Name == "" || req.Service == "" || req.URL == "" {
		return nil, fmt.Errorf("name, service and url are required")
	}
	if err := validateURL(req.URL); err != nil {
		return nil, err
	}
	sealed, err := secretbox.Seal(s.key, req.URL)
	if err != nil {
		return nil, fmt.Errorf("cannot seal channel url: %w", err)
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	var c Channel
	err = s.db.QueryRow(`INSERT INTO notification_channels (name, service, url, enabled)
		VALUES ($1, $2, $3, $4)
		RETURNING id, name, service, enabled, created_at, updated_at`,
		req.Name, req.Service, sealed, enabled).
		Scan(&c.ID, &c.Name, &c.Service, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	c.URLMask = maskURL(req.URL)
	return &c, nil
}

// Update patches a channel; an empty URL keeps the stored secret.
func (s *Store) Update(id string, req *ChannelRequest) (*Channel, error) {
	cur, err := s.get(id)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = cur.Name
	}
	service := strings.ToLower(strings.TrimSpace(req.Service))
	if service == "" {
		service = cur.Service
	}
	secret := cur.URL
	if strings.TrimSpace(req.URL) != "" {
		secret = strings.TrimSpace(req.URL)
	}
	enabled := cur.Enabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if err := validateURL(secret); err != nil {
		return nil, err
	}
	sealed, err := secretbox.Seal(s.key, secret)
	if err != nil {
		return nil, fmt.Errorf("cannot seal channel url: %w", err)
	}
	var c Channel
	err = s.db.QueryRow(`UPDATE notification_channels
		SET name = $1, service = $2, url = $3, enabled = $4, updated_at = NOW()
		WHERE id = $5
		RETURNING id, name, service, enabled, created_at, updated_at`,
		name, service, sealed, enabled, id).
		Scan(&c.ID, &c.Name, &c.Service, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	c.URLMask = maskURL(secret)
	return &c, nil
}

// Delete removes a channel.
func (s *Store) Delete(id string) error {
	res, err := s.db.Exec(`DELETE FROM notification_channels WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// validateURL rejects URLs no shoutrrr service claims.
func validateURL(rawURL string) error {
	sender, err := shoutrrr.CreateSender(rawURL)
	if err != nil {
		return fmt.Errorf("unsupported notification url: %w", err)
	}
	_ = sender
	return nil
}

// maskURL keeps scheme://host visible and hides the secret tail.
func maskURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	if u, err := url.Parse(rawURL); err == nil && u.Scheme != "" && u.Host != "" {
		tail := strings.Trim(u.Path+u.RawQuery, "/?")
		if len(tail) > 4 {
			tail = "••••" + tail[len(tail)-2:]
		} else if tail != "" {
			tail = "••••••"
		}
		masked := u.Scheme + "://" + u.Host
		if tail != "" {
			masked += "/" + tail
		}
		return masked
	}
	if len(rawURL) <= 12 {
		return "••••••"
	}
	return rawURL[:6] + "••••" + rawURL[len(rawURL)-2:]
}

// ─── Service ────────────────────────────────────────────────────

// Service fans generation events out to the tenant's channels.
type Service struct {
	store *Store
}

func NewService(store *Store) *Service { return &Service{store: store} }

// SendToUser posts a generation event to every enabled channel.
// It satisfies the agency notifier shape (userID scopes future per-user
// routing; channels are tenant-level today). Fire-and-forget: failures are
// logged, never returned.
func (s *Service) SendToUser(userID int64, title, body string, data map[string]string) {
	channels, err := s.store.ListEnabled()
	if err != nil {
		log.Printf("[notify] list enabled channels: %v", err)
		return
	}
	if len(channels) == 0 {
		return
	}
	message := strings.TrimSpace(title)
	if strings.TrimSpace(body) != "" {
		message += "\n" + strings.TrimSpace(body)
	}
	for i := range channels {
		c := channels[i]
		sender, err := shoutrrr.CreateSender(c.URL)
		if err != nil {
			log.Printf("[notify] channel %q: %v", c.Name, err)
			continue
		}
		if errs := sender.Send(message, &types.Params{}); len(errs) > 0 {
			for _, e := range errs {
				if e != nil {
					log.Printf("[notify] channel %q: %v", c.Name, e)
				}
			}
		}
	}
}

// SendTest posts a probe message through one channel.
func (s *Service) SendTest(id string) error {
	c, err := s.store.get(id)
	if err != nil {
		return err
	}
	sender, err := shoutrrr.CreateSender(c.URL)
	if err != nil {
		return err
	}
	if errs := sender.Send("🔔 Synapta: canal \""+c.Name+"\" configurado correctamente.", &types.Params{}); len(errs) > 0 {
		for _, e := range errs {
			if e != nil {
				return e
			}
		}
	}
	return nil
}

// ─── HTTP ───────────────────────────────────────────────────────

// Handler exposes channel endpoints.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// List handles GET /notify/channels.
func (h *Handler) List(c *gin.Context) {
	items, err := h.svc.store.List()
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, items)
}

// Create handles POST /notify/channels.
func (h *Handler) Create(c *gin.Context) {
	var req ChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	ch, err := h.svc.store.Create(&req)
	if err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Success(c, ch)
}

// Update handles PUT /notify/channels/:id.
func (h *Handler) Update(c *gin.Context) {
	var req ChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	ch, err := h.svc.store.Update(c.Param("id"), &req)
	if err != nil {
		if err == sql.ErrNoRows {
			utils.NotFound(c, "channel not found")
			return
		}
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Success(c, ch)
}

// Delete handles DELETE /notify/channels/:id.
func (h *Handler) Delete(c *gin.Context) {
	if err := h.svc.store.Delete(c.Param("id")); err != nil {
		if err == sql.ErrNoRows {
			utils.NotFound(c, "channel not found")
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "channel deleted")
}

// Test handles POST /notify/channels/:id/test.
func (h *Handler) Test(c *gin.Context) {
	if err := h.svc.SendTest(c.Param("id")); err != nil {
		if err == sql.ErrNoRows {
			utils.NotFound(c, "channel not found")
			return
		}
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Message(c, "test notification sent")
}

// ─── Module ─────────────────────────────────────────────────────

// HandlerResolver supplies the tenant-scoped handler for each request.
type HandlerResolver func(c *gin.Context) (*Handler, bool)

// Module registers the notify routes (admin only for writes).
type Module struct {
	resolve HandlerResolver
}

func NewModule(resolve HandlerResolver) *Module { return &Module{resolve: resolve} }

func (m *Module) Name() string { return "notify" }

func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, adminMw gin.HandlerFunc) {
	g := rg.Group("/notify")
	g.Use(authMw, tenantMw)
	{
		g.GET("/channels", m.priv("List"))
		g.POST("/channels", adminMw, m.priv("Create"))
		g.PUT("/channels/:id", adminMw, m.priv("Update"))
		g.DELETE("/channels/:id", adminMw, m.priv("Delete"))
		g.POST("/channels/:id/test", adminMw, m.priv("Test"))
	}
}

// priv resolves the tenant handler then dispatches to the named method.
func (m *Module) priv(method string) gin.HandlerFunc {
	return func(c *gin.Context) {
		hdl, ok := m.resolve(c)
		if !ok {
			return
		}
		switch method {
		case "List":
			hdl.List(c)
		case "Create":
			hdl.Create(c)
		case "Update":
			hdl.Update(c)
		case "Delete":
			hdl.Delete(c)
		case "Test":
			hdl.Test(c)
		default:
			utils.InternalError(c, "unknown handler method: "+method)
		}
	}
}
