// Package push delivers Web Push notifications (VAPID) with per-tenant
// subscriptions stored in the tenant schema. When VAPID is not configured
// the service degrades to a no-op so the app keeps working.
package push

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/gin-gonic/gin"

	"synapta/internal/utils"
)

// ─── Types ──────────────────────────────────────────────────────

// Subscription is a Web Push subscription registered for a user's device.
type Subscription struct {
	ID        string    `json:"id"`
	UserID    int64     `json:"user_id"`
	Endpoint  string    `json:"endpoint"`
	P256DH    string    `json:"p256dh"`
	Auth      string    `json:"auth"`
	UserAgent string    `json:"user_agent,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SubscriptionRequest is the browser's PushSubscriptionJSON body.
type SubscriptionRequest struct {
	Endpoint       string           `json:"endpoint" binding:"required"`
	ExpirationTime *time.Time       `json:"expirationTime"`
	Keys           SubscriptionKeys `json:"keys"`
}

// SubscriptionKeys holds the ECDH public key and auth secret.
type SubscriptionKeys struct {
	P256DH string `json:"p256dh" binding:"required"`
	Auth   string `json:"auth" binding:"required"`
}

// ─── Store ──────────────────────────────────────────────────────

// Store persists subscriptions in the tenant schema.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Create inserts a subscription, or refreshes keys on re-registration.
func (s *Store) Create(sub *Subscription) error {
	_, err := s.db.Exec(`
		INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth, user_agent)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, endpoint) DO UPDATE
		SET p256dh = EXCLUDED.p256dh,
		    auth = EXCLUDED.auth,
		    user_agent = EXCLUDED.user_agent,
		    updated_at = NOW()`,
		sub.UserID, sub.Endpoint, sub.P256DH, sub.Auth, sub.UserAgent)
	return err
}

// ListByUser returns every device subscribed by the user.
func (s *Store) ListByUser(userID int64) ([]Subscription, error) {
	rows, err := s.db.Query(`
		SELECT id, user_id, endpoint, p256dh, auth, COALESCE(user_agent,''), created_at, updated_at
		FROM push_subscriptions WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []Subscription
	for rows.Next() {
		var sub Subscription
		if err := rows.Scan(&sub.ID, &sub.UserID, &sub.Endpoint, &sub.P256DH, &sub.Auth,
			&sub.UserAgent, &sub.CreatedAt, &sub.UpdatedAt); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// DeleteByEndpoint removes one device subscription for a user.
func (s *Store) DeleteByEndpoint(userID int64, endpoint string) error {
	_, err := s.db.Exec(`DELETE FROM push_subscriptions WHERE user_id = $1 AND endpoint = $2`,
		userID, endpoint)
	return err
}

// ─── Service ────────────────────────────────────────────────────

// Service owns subscription persistence and Web Push delivery.
type Service struct {
	store           *Store
	vapidPublicKey  string
	vapidPrivateKey string
	vapidSubject    string
	client          *http.Client
}

func NewService(store *Store, vapidPublicKey, vapidPrivateKey, vapidSubject string) *Service {
	return &Service{
		store:           store,
		vapidPublicKey:  vapidPublicKey,
		vapidPrivateKey: vapidPrivateKey,
		vapidSubject:    vapidSubject,
		client:          &http.Client{Timeout: 15 * time.Second},
	}
}

// Enabled reports whether VAPID is configured.
func (s *Service) Enabled() bool {
	return s.vapidPublicKey != "" && s.vapidPrivateKey != "" && s.vapidSubject != ""
}

// Register stores a new device subscription for the user.
func (s *Service) Register(userID int64, req *SubscriptionRequest, userAgent string) error {
	return s.store.Create(&Subscription{
		UserID: userID, Endpoint: req.Endpoint,
		P256DH: req.Keys.P256DH, Auth: req.Keys.Auth, UserAgent: userAgent,
	})
}

// Unregister removes one device subscription for the user.
func (s *Service) Unregister(userID int64, endpoint string) error {
	return s.store.DeleteByEndpoint(userID, endpoint)
}

// SendTest delivers a test notification to all the user's devices.
func (s *Service) SendTest(userID int64, message string) (int, error) {
	if !s.Enabled() {
		return 0, fmt.Errorf("push not configured: set PUSH_VAPID_PUBLIC_KEY, PUSH_VAPID_PRIVATE_KEY and PUSH_VAPID_SUBJECT")
	}
	subs, err := s.store.ListByUser(userID)
	if err != nil {
		return 0, err
	}
	if len(subs) == 0 {
		return 0, nil
	}
	if message == "" {
		message = "If you see this, the whole chain works."
	}
	payload, err := buildPayload("🔔 Synapta push test", message, map[string]string{"type": "push-test"})
	if err != nil {
		return 0, err
	}
	for i := range subs {
		s.sendOne(&subs[i], payload)
	}
	return len(subs), nil
}

// SendToUser pushes a notification to all the user's devices. Fire-and-forget:
// delivery problems are logged and gone subscriptions are cleaned up.
func (s *Service) SendToUser(userID int64, title, body string, data map[string]string) {
	if !s.Enabled() {
		return
	}
	subs, err := s.store.ListByUser(userID)
	if err != nil {
		log.Printf("[push] list subscriptions user=%d: %v", userID, err)
		return
	}
	payload, err := buildPayload(title, body, data)
	if err != nil {
		log.Printf("[push] build payload: %v", err)
		return
	}
	for i := range subs {
		s.sendOne(&subs[i], payload)
	}
}

func (s *Service) sendOne(sub *Subscription, payload []byte) {
	resp, err := webpush.SendNotificationWithContext(context.Background(), payload, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{P256dh: sub.P256DH, Auth: sub.Auth},
	}, &webpush.Options{
		HTTPClient:      s.client,
		Subscriber:      s.vapidSubject,
		VAPIDPublicKey:  s.vapidPublicKey,
		VAPIDPrivateKey: s.vapidPrivateKey,
		TTL:             60,
	})
	if err != nil {
		log.Printf("[push] send to %s: %v", sub.Endpoint, err)
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
	case http.StatusNotFound, http.StatusGone:
		log.Printf("[push] subscription gone (%d), removing %s", resp.StatusCode, sub.Endpoint)
		if delErr := s.store.DeleteByEndpoint(sub.UserID, sub.Endpoint); delErr != nil {
			log.Printf("[push] remove gone subscription: %v", delErr)
		}
	default:
		log.Printf("[push] send to %s returned %d", sub.Endpoint, resp.StatusCode)
	}
}

// buildPayload encodes the Web Push body for service-worker display.
func buildPayload(title, body string, data map[string]string) ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"notification": map[string]interface{}{
			"title": title,
			"body":  body,
			"icon":  "/assets/icons/192x192.png",
			"badge": "/assets/icons/192x192.png",
			"data":  data,
		},
	})
}

// ─── HTTP ───────────────────────────────────────────────────────

// Handler exposes push endpoints.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Register handles POST /push/subscriptions.
func (h *Handler) Register(c *gin.Context) {
	var req SubscriptionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	userID := utils.UserIDFromContext(c)
	if userID <= 0 {
		utils.Unauthorized(c, "missing user id")
		return
	}
	if err := h.svc.Register(userID, &req, c.Request.UserAgent()); err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, gin.H{"subscribed": true})
}

// Unregister handles DELETE /push/subscriptions?endpoint=...
func (h *Handler) Unregister(c *gin.Context) {
	endpoint := c.Query("endpoint")
	if endpoint == "" {
		utils.BadRequest(c, "endpoint is required")
		return
	}
	userID := utils.UserIDFromContext(c)
	if userID <= 0 {
		utils.Unauthorized(c, "missing user id")
		return
	}
	if err := h.svc.Unregister(userID, endpoint); err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "unsubscribed")
}

// Test handles POST /push/test — diagnostic helper.
func (h *Handler) Test(c *gin.Context) {
	userID := utils.UserIDFromContext(c)
	if userID <= 0 {
		utils.Unauthorized(c, "missing user id")
		return
	}
	var req struct {
		Message string `json:"message"`
	}
	_ = c.ShouldBindJSON(&req)
	n, err := h.svc.SendTest(userID, req.Message)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if n == 0 {
		utils.Message(c, "no push subscriptions registered for this user")
		return
	}
	utils.Success(c, gin.H{"sent_to": n})
}

// ─── Module ─────────────────────────────────────────────────────

// HandlerResolver supplies the tenant-scoped handler for each request.
type HandlerResolver func(c *gin.Context) (*Handler, bool)

// Module registers push routes.
type Module struct {
	resolve HandlerResolver
}

func NewModule(resolve HandlerResolver) *Module { return &Module{resolve: resolve} }

func (m *Module) Name() string { return "push" }

func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, _ gin.HandlerFunc) {
	g := rg.Group("/push")
	g.Use(authMw, tenantMw)
	{
		g.POST("/subscriptions", m.priv("Register"))
		g.DELETE("/subscriptions", m.priv("Unregister"))
		g.POST("/test", m.priv("Test"))
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
		case "Register":
			hdl.Register(c)
		case "Unregister":
			hdl.Unregister(c)
		case "Test":
			hdl.Test(c)
		default:
			utils.InternalError(c, "unknown handler method: "+method)
		}
	}
}
