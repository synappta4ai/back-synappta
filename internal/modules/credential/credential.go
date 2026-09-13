// Package credential stores per-tenant provider credentials (API keys, AK/SK)
// encrypted at rest. Models are defined in code; each tenant brings its own
// credentials for the providers its models need.
package credential

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

// IsValidProvider reports whether the provider name is one of the supported ones.
func IsValidProvider(p string) bool { return validProviders[p] }

// MaskPublic returns an obfuscated representation of a secret safe for logs
// and API responses: first 4 + last 4 chars, e.g. "AIza••••cQ9x". Short or
// empty secrets produce a fixed placeholder without leaking content.
func MaskPublic(secret string) string {
	s := strings.TrimSpace(secret)
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "••••••"
	}
	return s[:4] + "••••" + s[len(s)-4:]
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

// validateKeyFormat rejects keys that clearly belong to another provider
// (paste-the-wrong-key protection). Empty keys pass — validation of presence
// is done at generation time. Only confident signatures are rejected; anything
// ambiguous is allowed (Ark keys have no documented fixed prefix).
func validateKeyFormat(provider, apiKey string) error {
	k := strings.TrimSpace(apiKey)
	if k == "" {
		return nil
	}
	isAnthropicKey := strings.HasPrefix(k, "sk-ant-")
	isGeminiKey := strings.HasPrefix(k, "AIza")
	isGenericOpenAIStyle := strings.HasPrefix(k, "sk-") && !isAnthropicKey

	// Which provider's signature does the key carry?
	var keyLooksLike string
	switch {
	case isAnthropicKey:
		keyLooksLike = "an Anthropic key (sk-ant-...)"
	case isGeminiKey:
		keyLooksLike = "a Google Gemini key (AIza...)"
	case isGenericOpenAIStyle:
		keyLooksLike = "an OpenAI-style key (sk-...)"
	default:
		keyLooksLike = ""
	}

	conflict := func(where string) error {
		return fmt.Errorf(
			"the pasted key is %s and cannot be saved for %s — get the correct key in: %s",
			keyLooksLike, provider, where)
	}

	switch provider {
	case ProviderGemini:
		if isAnthropicKey || isGenericOpenAIStyle {
			return conflict("Google AI Studio (aistudio.google.com → Get API key)")
		}
	case ProviderAnthropic:
		if isGeminiKey || isGenericOpenAIStyle {
			return conflict("the Anthropic Console (console.anthropic.com → API Keys)")
		}
	case ProviderBytePlus:
		if isAnthropicKey || isGeminiKey {
			return conflict("BytePlus Console → ModelArk → API Key Management (console.byteplus.com/ark)")
		}
	}
	return nil
}

// Upsert creates or updates the tenant's credential for a provider.
func (s *Store) Upsert(req *UpsertRequest) (*Credential, error) {
	if !validProviders[req.Provider] {
		return nil, fmt.Errorf("invalid provider %q (valid: byteplus, gemini, anthropic)", req.Provider)
	}

	// Trim pasted values — stray whitespace/newlines break Authorization headers.
	req.AccessKeyID = strings.TrimSpace(req.AccessKeyID)
	req.SecretAccessKey = strings.TrimSpace(req.SecretAccessKey)
	req.APIKey = strings.TrimSpace(req.APIKey)
	req.Endpoint = strings.TrimSpace(req.Endpoint)
	req.BaseURL = strings.TrimSpace(req.BaseURL)
	req.DisplayName = strings.TrimSpace(req.DisplayName)

	if err := validateKeyFormat(req.Provider, req.APIKey); err != nil {
		return nil, err
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

// ExportedCredential is one credential with DECRYPTED secrets, used only by
// the tenant export/import flow (platform superadmin backup/migration).
type ExportedCredential struct {
	Provider        string `json:"provider"`
	DisplayName     string `json:"display_name,omitempty"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	APIKey          string `json:"api_key,omitempty"`
	Endpoint        string `json:"endpoint,omitempty"`
	BaseURL         string `json:"base_url,omitempty"`
	Extra           string `json:"extra,omitempty"`
}

// ExportAll returns every credential with decrypted secrets. NEVER expose
// this through an unauthenticated or non-superadmin route.
func (s *Store) ExportAll() ([]ExportedCredential, error) {
	rows, err := s.db.Query(
		`SELECT provider, COALESCE(display_name,''), COALESCE(access_key_id,''), COALESCE(secret_access_key,''),
		        COALESCE(api_key,''), COALESCE(endpoint,''), COALESCE(base_url,''), COALESCE(extra,'')
		 FROM credentials ORDER BY provider`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ExportedCredential
	for rows.Next() {
		var e ExportedCredential
		var akSealed, skSealed, apiSealed string
		if err := rows.Scan(&e.Provider, &e.DisplayName, &akSealed, &skSealed, &apiSealed,
			&e.Endpoint, &e.BaseURL, &e.Extra); err != nil {
			return nil, err
		}
		e.AccessKeyID = s.open(akSealed)
		e.SecretAccessKey = s.open(skSealed)
		e.APIKey = s.open(apiSealed)
		out = append(out, e)
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
