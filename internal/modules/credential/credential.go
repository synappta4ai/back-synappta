// Package credential stores per-tenant provider credentials (API keys, AK/SK)
// encrypted at rest. Models are defined in code; each tenant brings its own
// credentials for the providers its models need.
package credential

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"synapta/internal/utils"
)

// Provider names mirror the model catalog's CredentialProvider values.
const (
	ProviderBytePlus  = "byteplus"
	ProviderGemini    = "gemini"
	ProviderAnthropic = "anthropic"
)

var validProviders = map[string]bool{
	ProviderBytePlus: true, ProviderGemini: true, ProviderAnthropic: true,
}

// Credential is one tenant's credentials for one provider.
type Credential struct {
	ID              string    `json:"id"`
	Provider        string    `json:"provider"`
	DisplayName     string    `json:"display_name,omitempty"`
	AccessKeyID     string    `json:"access_key_id"` // stored encrypted, returned masked
	SecretAccessKey string    `json:"-"`             // never returned
	APIKey          string    `json:"-"`             // never returned
	Endpoint        string    `json:"endpoint"`
	BaseURL         string    `json:"base_url"`
	Extra           string    `json:"extra,omitempty"` // JSON blob for provider-specific fields
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Masked is the API representation of a credential (secrets masked).
type Masked struct {
	ID          string    `json:"id"`
	Provider    string    `json:"provider"`
	DisplayName string    `json:"display_name,omitempty"`
	AccessKeyID string    `json:"access_key_id"`
	APIKeyMask  string    `json:"api_key_mask"`
	Endpoint    string    `json:"endpoint"`
	BaseURL     string    `json:"base_url"`
	Extra       string    `json:"extra,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Resolve is what the agency pipeline consumes.
type Resolve struct {
	AccessKeyID     string
	SecretAccessKey string
	APIKey          string
	Endpoint        string
	BaseURL         string
	Extra           string
}

// UpsertRequest is the payload for creating/updating a credential.
type UpsertRequest struct {
	Provider        string `json:"provider" binding:"required"`
	DisplayName     string `json:"display_name"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	APIKey          string `json:"api_key"`
	Endpoint        string `json:"endpoint"`
	BaseURL         string `json:"base_url"`
	Extra           string `json:"extra"`
}

// Store persists credentials in the tenant schema.
type Store struct {
	db  *sql.DB
	key []byte
}

func NewStore(db *sql.DB, encryptionKey string) *Store {
	return &Store{db: db, key: []byte(encryptionKey)}
}

func (s *Store) seal(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	return encrypt(s.key, []byte(v))
}

func (s *Store) open(sealed string) string {
	if sealed == "" {
		return ""
	}
	plain, err := decrypt(s.key, sealed)
	if err != nil {
		return ""
	}
	return string(plain)
}

// Upsert creates or updates the tenant's credential for a provider.
func (s *Store) Upsert(req *UpsertRequest) (*Credential, error) {
	if !validProviders[req.Provider] {
		return nil, fmt.Errorf("invalid provider %q (valid: byteplus, gemini, anthropic)", req.Provider)
	}

	akSealed, err := s.seal(req.AccessKeyID)
	if err != nil {
		return nil, err
	}
	skSealed, err := s.seal(req.SecretAccessKey)
	if err != nil {
		return nil, err
	}
	apiSealed, err := s.seal(req.APIKey)
	if err != nil {
		return nil, err
	}

	id := uuid.New().String()
	c := &Credential{
		ID:          id,
		Provider:    req.Provider,
		DisplayName: req.DisplayName,
		Endpoint:    req.Endpoint,
		BaseURL:     req.BaseURL,
		Extra:       req.Extra,
	}

	query := `
		INSERT INTO credentials (id, provider, display_name, access_key_id, secret_access_key, api_key, endpoint, base_url, extra)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (provider) DO UPDATE SET
			display_name = EXCLUDED.display_name,
			access_key_id = EXCLUDED.access_key_id,
			secret_access_key = EXCLUDED.secret_access_key,
			api_key = EXCLUDED.api_key,
			endpoint = EXCLUDED.endpoint,
			base_url = EXCLUDED.base_url,
			extra = EXCLUDED.extra,
			updated_at = NOW()
		RETURNING id, created_at, updated_at`

	err = s.db.QueryRow(query, id, req.Provider, req.DisplayName, akSealed, skSealed, apiSealed, req.Endpoint, req.BaseURL, req.Extra).
		Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to upsert credential: %w", err)
	}
	return c, nil
}

// Get resolves a provider's credentials (decrypted) for pipeline use.
func (s *Store) Get(provider string) (*Resolve, error) {
	var akSealed, skSealed, apiSealed string
	var r Resolve
	var displayName string
	var createdAt, updatedAt time.Time
	err := s.db.QueryRow(
		`SELECT COALESCE(access_key_id,''), COALESCE(secret_access_key,''), COALESCE(api_key,''),
		        COALESCE(endpoint,''), COALESCE(base_url,''), COALESCE(extra,''), COALESCE(display_name,''),
		        created_at, updated_at
		 FROM credentials WHERE provider = $1`, provider,
	).Scan(&akSealed, &skSealed, &apiSealed, &r.Endpoint, &r.BaseURL, &r.Extra, &displayName, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.AccessKeyID = s.open(akSealed)
	r.SecretAccessKey = s.open(skSealed)
	r.APIKey = s.open(apiSealed)
	return &r, nil
}

// List returns all credentials masked (never returns secrets).
func (s *Store) List() ([]Masked, error) {
	rows, err := s.db.Query(
		`SELECT id, provider, COALESCE(display_name,''), COALESCE(access_key_id,''), COALESCE(api_key,''),
		        COALESCE(endpoint,''), COALESCE(base_url,''), COALESCE(extra,''), created_at, updated_at
		 FROM credentials ORDER BY provider`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Masked
	for rows.Next() {
		var m Masked
		var akSealed, apiSealed string
		if err := rows.Scan(&m.ID, &m.Provider, &m.DisplayName, &akSealed, &apiSealed,
			&m.Endpoint, &m.BaseURL, &m.Extra, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		m.AccessKeyID = mask(s.open(akSealed))
		m.APIKeyMask = mask(s.open(apiSealed))
		out = append(out, m)
	}
	return out, rows.Err()
}

// Delete removes a provider's credentials.
func (s *Store) Delete(provider string) error {
	result, err := s.db.Exec(`DELETE FROM credentials WHERE provider = $1`, provider)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("credential not found for provider %q", provider)
	}
	return nil
}

// Handler exposes credential CRUD over HTTP.
type Handler struct {
	store *Store
}

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// List handles GET /credentials
func (h *Handler) List(c *gin.Context) {
	items, err := h.store.List()
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if items == nil {
		items = []Masked{}
	}
	utils.Success(c, items)
}

// Upsert handles PUT /credentials
func (h *Handler) Upsert(c *gin.Context) {
	var req UpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	cred, err := h.store.Upsert(&req)
	if err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Success(c, Masked{
		ID: cred.ID, Provider: cred.Provider, DisplayName: cred.DisplayName,
		Endpoint: cred.Endpoint, BaseURL: cred.BaseURL, Extra: cred.Extra,
		CreatedAt: cred.CreatedAt, UpdatedAt: cred.UpdatedAt,
	})
}

// Delete handles DELETE /credentials/:provider
func (h *Handler) Delete(c *gin.Context) {
	provider := c.Param("provider")
	if err := h.store.Delete(provider); err != nil {
		utils.NotFound(c, err.Error())
		return
	}
	utils.Message(c, "credential deleted")
}

// Module registers the credential routes (admin only for writes).
type Module struct {
	resolve HandlerResolver
}

// HandlerResolver supplies the tenant-scoped handler for each request.
type HandlerResolver func(c *gin.Context) (*Handler, bool)

func NewModule(resolve HandlerResolver) *Module { return &Module{resolve: resolve} }

func (m *Module) Name() string { return "credentials" }

func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, adminMw gin.HandlerFunc) {
	g := rg.Group("/credentials")
	g.Use(authMw, tenantMw)
	{
		g.GET("", m.priv("List"))
		g.PUT("", adminMw, m.priv("Upsert"))
		g.DELETE("/:provider", adminMw, m.priv("Delete"))
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
		case "Upsert":
			hdl.Upsert(c)
		case "Delete":
			hdl.Delete(c)
		default:
			utils.InternalError(c, "unknown handler method: "+method)
		}
	}
}
