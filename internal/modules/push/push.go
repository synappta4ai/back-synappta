// Package push delivers Web Push notifications (VAPID) with per-tenant
// subscriptions stored in the tenant schema. When VAPID is not configured
// the service degrades to a no-op so the app keeps working.
package push

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/gin-gonic/gin"

	"synapta/internal/tenancy"
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
// Delivery is signed with the user's own VAPID pair (auto-generated on
// first use); the global PUSH_VAPID_* pair only signs sends to legacy
// subscriptions created before per-user keys existed.
type Service struct {
	store           *Store
	vapidPublicKey  string
	vapidPrivateKey string
	vapidSubject    string
	userKeys        *UserKeysStore
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

// WithUserKeys attaches the per-user VAPID store (auto-generation).
func (s *Service) WithUserKeys(st *UserKeysStore) *Service { s.userKeys = st; return s }

// Enabled reports whether push delivery can work at all: per-user keys
// auto-generate, so a configured store is enough; otherwise the legacy
// global pair must be complete.
func (s *Service) Enabled() bool {
	if s.userKeys != nil {
		return true
	}
	return s.vapidPublicKey != "" && s.vapidPrivateKey != "" && s.vapidSubject != ""
}

// keysFor resolves the signing identity for a user: their own pair when
// present, else the legacy global pair.
func (s *Service) keysFor(userID int64) (pub, priv, subject string) {
	if s.userKeys != nil {
		if k, err := s.userKeys.Get(userID); err == nil && k.PublicKey != "" && k.PrivateKey != "" {
			sub := k.Subject
			if sub == "" {
				sub = s.vapidSubject
			}
			return k.PublicKey, k.PrivateKey, sub
		}
	}
	return s.vapidPublicKey, s.vapidPrivateKey, s.vapidSubject
}

// EnsureUserKeys returns the user's own VAPID pair, generating it on first
// use (same generation as cmd/generate-keys).
func (s *Service) EnsureUserKeys(userID int64) (*UserKeys, error) {
	if s.userKeys == nil {
		return &UserKeys{UserID: userID, PublicKey: s.vapidPublicKey, PrivateKey: s.vapidPrivateKey, Subject: s.vapidSubject}, nil
	}
	return s.userKeys.Ensure(userID, s.vapidSubject, false)
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
	pub, priv, subject := s.keysFor(userID)
	if pub == "" || priv == "" || subject == "" {
		return 0, fmt.Errorf("push not configured: no VAPID keys for user")
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
		s.sendOne(&subs[i], payload, pub, priv, subject)
	}
	return len(subs), nil
}

// SendToUser pushes a notification to all the user's devices. Fire-and-forget:
// delivery problems are logged and gone subscriptions are cleaned up.
func (s *Service) SendToUser(userID int64, title, body string, data map[string]string) {
	pub, priv, subject := s.keysFor(userID)
	if pub == "" || priv == "" || subject == "" {
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
		s.sendOne(&subs[i], payload, pub, priv, subject)
	}
}

func (s *Service) sendOne(sub *Subscription, payload []byte, pub, priv, subject string) {
	resp, err := webpush.SendNotificationWithContext(context.Background(), payload, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{P256dh: sub.P256DH, Auth: sub.Auth},
	}, &webpush.Options{
		HTTPClient:      s.client,
		Subscriber:      subject,
		VAPIDPublicKey:  pub,
		VAPIDPrivateKey: priv,
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
	// pairing mints QR codes (system-wide); nil disables pairing routes.
	pairing *PairingService
	// center serves the native in-app bell; nil disables center routes.
	center *CenterService
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// WithPairing attaches the QR pairing service (shared, not per-tenant).
func (h *Handler) WithPairing(p *PairingService) *Handler { h.pairing = p; return h }

// WithCenter attaches the native notification center (per-tenant pool).
func (h *Handler) WithCenter(c *CenterService) *Handler { h.center = c; return h }

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

// PublicKey handles GET /push/public-key: the user's own VAPID key
// (auto-generated on first call), plus whether push delivery works.
func (h *Handler) PublicKey(c *gin.Context) {
	userID := utils.UserIDFromContext(c)
	if userID <= 0 {
		utils.Unauthorized(c, "missing user id")
		return
	}
	keys, err := h.svc.EnsureUserKeys(userID)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, gin.H{
		"public_key": keys.PublicKey,
		"enabled":    keys.PublicKey != "" && keys.PrivateKey != "",
		"managed":    h.svc.userKeys != nil,
	})
}

// RotateVapid handles POST /push/vapid/rotate: forces a fresh per-user pair.
// Existing subscriptions were bound to the old applicationServerKey and must
// resubscribe (the front re-subscribes automatically after rotating).
func (h *Handler) RotateVapid(c *gin.Context) {
	if h.svc.userKeys == nil {
		utils.BadRequest(c, "per-user vapid not configured")
		return
	}
	userID := utils.UserIDFromContext(c)
	if userID <= 0 {
		utils.Unauthorized(c, "missing user id")
		return
	}
	keys, err := h.svc.userKeys.Ensure(userID, h.svc.vapidSubject, true)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, gin.H{"public_key": keys.PublicKey})
}

// TestCenter handles POST /push/notifications/test: writes a probe alert
// into the native bell so admins can verify the center chain.
func (h *Handler) TestCenter(c *gin.Context) {
	if h.center == nil {
		utils.InternalError(c, "notification center not configured")
		return
	}
	userID := utils.UserIDFromContext(c)
	if userID <= 0 {
		utils.Unauthorized(c, "missing user id")
		return
	}
	var req struct {
		Message string `json:"message"`
	}
	_ = c.ShouldBindJSON(&req)
	body := req.Message
	if body == "" {
		body = "If you see this in your bell, the native center works."
	}
	if err := h.center.Record(userID, "push-test", "🔔 Synapta center test", body, ""); err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "center test notification recorded")
}

// CreatePairing handles POST /push/pairing: mints a QR code binding the
// calling user+tenant for 10 minutes.
func (h *Handler) CreatePairing(c *gin.Context) {
	if h.pairing == nil {
		utils.InternalError(c, "pairing not configured")
		return
	}
	userID := utils.UserIDFromContext(c)
	if userID <= 0 {
		utils.Unauthorized(c, "missing user id")
		return
	}
	t := tenancy.FromContext(c.Request.Context())
	if t == nil {
		utils.BadRequest(c, "missing tenant context")
		return
	}
	pc, err := h.pairing.Mint(t.Slug, userID)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, pc)
}// ListNotifications handles GET /push/notifications: the native bell feed.
func (h *Handler) ListNotifications(c *gin.Context) {
	if h.center == nil {
		utils.InternalError(c, "notification center not configured")
		return
	}
	userID := utils.UserIDFromContext(c)
	if userID <= 0 {
		utils.Unauthorized(c, "missing user id")
		return
	}
	items, err := h.center.List(userID, 30)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, items)
}

// UnreadNotifications handles GET /push/notifications/unread: just the count.
func (h *Handler) UnreadNotifications(c *gin.Context) {
	if h.center == nil {
		utils.InternalError(c, "notification center not configured")
		return
	}
	userID := utils.UserIDFromContext(c)
	if userID <= 0 {
		utils.Unauthorized(c, "missing user id")
		return
	}
	n, err := h.center.UnreadCount(userID)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, gin.H{"unread": n})
}

// ReadNotifications handles POST /push/notifications/read: marks all read.
func (h *Handler) ReadNotifications(c *gin.Context) {
	if h.center == nil {
		utils.InternalError(c, "notification center not configured")
		return
	}
	userID := utils.UserIDFromContext(c)
	if userID <= 0 {
		utils.Unauthorized(c, "missing user id")
		return
	}
	n, err := h.center.MarkAllRead(userID)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, gin.H{"read": n})
}

// ─── Module ─────────────────────────────────────────────────────

// HandlerResolver supplies the tenant-scoped handler for each request.
type HandlerResolver func(c *gin.Context) (*Handler, bool)

// Module registers push routes.
type Module struct {
	resolve HandlerResolver
	// pairing serves the public QR redeem (no tenant context on that call).
	pairing *PairingService
}

func NewModule(resolve HandlerResolver) *Module { return &Module{resolve: resolve} }

// WithPairing attaches the shared pairing service for the public redeem.
func (m *Module) WithPairing(p *PairingService) *Module { m.pairing = p; return m }

func (m *Module) Name() string { return "push" }

func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, _ gin.HandlerFunc) {
	g := rg.Group("/push")
	g.Use(authMw, tenantMw)
	{
		g.POST("/subscriptions", m.priv("Register"))
		g.DELETE("/subscriptions", m.priv("Unregister"))
		g.POST("/test", m.priv("Test"))
		g.GET("/public-key", m.priv("PublicKey"))
		g.POST("/pairing", m.priv("CreatePairing"))
		g.GET("/notifications", m.priv("ListNotifications"))
		g.GET("/notifications/unread", m.priv("UnreadNotifications"))
		g.POST("/notifications/read", m.priv("ReadNotifications"))
		g.POST("/notifications/test", m.priv("TestCenter"))
		g.POST("/vapid/rotate", m.priv("RotateVapid"))
	}
	// QR redeem is public: the scanning phone has no session yet.
	pub := rg.Group("/push")
	{
		pub.GET("/pairing/:code/public-key", m.redeemKey)
		pub.POST("/pairing/:code/redeem", m.redeem)
	}
}

// redeemKey serves the owner's VAPID public key for a pairing code so the
// scanning phone can subscribe before redeeming (still unused at this point).
func (m *Module) redeemKey(c *gin.Context) {
	if m.pairing == nil {
		utils.InternalError(c, "pairing not configured")
		return
	}
	pub, err := m.pairing.PublicKeyFor(c.Param("code"), "")
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			utils.BadRequest(c, "invalid or expired code")
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, gin.H{"public_key": pub, "enabled": pub != ""})
}

// redeem serves the public route with the shared pairing service.
func (m *Module) redeem(c *gin.Context) {
	if m.pairing == nil {
		utils.InternalError(c, "pairing not configured")
		return
	}
	var req SubscriptionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	if err := m.pairing.Redeem(c.Param("code"), &req, c.Request.UserAgent()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
		utils.BadRequest(c, "invalid or expired code")
		return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, gin.H{"subscribed": true})
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
		case "PublicKey":
			hdl.PublicKey(c)
		case "CreatePairing":
			hdl.CreatePairing(c)
		case "ListNotifications":
			hdl.ListNotifications(c)
		case "UnreadNotifications":
			hdl.UnreadNotifications(c)
		case "ReadNotifications":
			hdl.ReadNotifications(c)
		case "TestCenter":
			hdl.TestCenter(c)
		case "RotateVapid":
			hdl.RotateVapid(c)
		default:
			utils.InternalError(c, "unknown handler method: "+method)
		}
	}
}
