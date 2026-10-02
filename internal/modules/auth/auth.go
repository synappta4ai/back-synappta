// Package auth handles platform users and JWT issuance.
//
// Users are global (system schema). A token carries a tenant_id chosen at
// login (the user's first active membership unless overridden), which the
// tenancy middleware uses to scope every request.
package auth

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"synapta/internal/utils"
)

var (
	ErrUserExists   = errors.New("username already exists")
	ErrInvalidCreds = errors.New("invalid username or password")
)

// User is a platform user row.
type User struct {
	ID           int64       `json:"id"`
	Username     string      `json:"username"`
	Name         string      `json:"name"`
	Surname      string      `json:"surname"`
	UserName     string      `json:"user_name"`
	Email        string      `json:"email"`
	PlatformRole int         `json:"platform_role"`
	Active       bool        `json:"active"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
	DeletedAt    *time.Time  `json:"deleted_at,omitempty"`
	PasswordHash string      `json:"-"`
	Preferences  Preferences `json:"preferences,omitempty"`
}

// Preferences is a JSONB value type for user preferences.
type Preferences map[string]any

func (p Preferences) Value() (driver.Value, error) {
	return json.Marshal(p)
}

func (p *Preferences) Scan(src any) error {
	if src == nil {
		*p = make(Preferences)
		return nil
	}
	switch v := src.(type) {
	case []byte:
		if len(v) == 0 {
			*p = make(Preferences)
			return nil
		}
		return json.Unmarshal(v, p)
	case string:
		if len(v) == 0 {
			*p = make(Preferences)
			return nil
		}
		return json.Unmarshal([]byte(v), p)
	default:
		return fmt.Errorf("preferences: expected []byte or string, got %T", src)
	}
}

// ThemePreferences represents the theme sub-object.
type ThemePreferences struct {
	Palette string `json:"palette"`
	Mode    string `json:"mode"`
}

const userCols = `id, username, name, surname, COALESCE(user_name,'') AS user_name,
	COALESCE(email,'') AS email, platform_role, active, created_at, updated_at, deleted_at, COALESCE(preferences, '{}') AS preferences`

// Store persists users and memberships in the system schema.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) CreateUser(username, passwordHash, name, surname, userName, email string, platformRole int) (*User, error) {
	query := `INSERT INTO users (username, password_hash, name, surname, user_name, email, platform_role, active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, true)
		RETURNING ` + userCols
	u := &User{Username: username, PasswordHash: passwordHash, Name: name, Surname: surname, UserName: userName, Email: email, PlatformRole: platformRole}
	err := s.db.QueryRow(query, username, passwordHash, name, surname, userName, email, platformRole).
		Scan(&u.ID, &u.Username, &u.Name, &u.Surname, &u.UserName, &u.Email, &u.PlatformRole, &u.Active, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt, &u.Preferences)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Store) GetUserByUsername(username string) (*User, error) {
	query := `SELECT ` + userCols + `, password_hash FROM users WHERE username = $1 AND active = true AND deleted_at IS NULL`
	u := &User{}
	err := s.db.QueryRow(query, username).
		Scan(&u.ID, &u.Username, &u.Name, &u.Surname, &u.UserName, &u.Email, &u.PlatformRole, &u.Active, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt, &u.Preferences, &u.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Store) GetUserByID(id int64) (*User, error) {
	query := `SELECT ` + userCols + ` FROM users WHERE id = $1 AND deleted_at IS NULL`
	u := &User{}
	err := s.db.QueryRow(query, id).
		Scan(&u.ID, &u.Username, &u.Name, &u.Surname, &u.UserName, &u.Email, &u.PlatformRole, &u.Active, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt, &u.Preferences)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// ListTenantUsers returns the users who are members of a tenant (JOIN memberships).
func (s *Store) ListTenantUsers(tenantID int64) ([]User, error) {
	rows, err := s.db.Query(`SELECT u.id, u.username, u.name, u.surname, COALESCE(u.user_name,'') AS user_name,
		COALESCE(u.email,'') AS email, u.platform_role, u.active, u.created_at, u.updated_at, u.deleted_at, COALESCE(u.preferences, '{}') AS preferences
		FROM users u
		JOIN tenant_memberships m ON m.user_id = u.id
		WHERE m.tenant_id = $1 AND u.deleted_at IS NULL ORDER BY u.id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Name, &u.Surname, &u.UserName, &u.Email, &u.PlatformRole, &u.Active, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt, &u.Preferences); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListUsers returns every active platform user.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users WHERE deleted_at IS NULL ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Name, &u.Surname, &u.UserName, &u.Email, &u.PlatformRole, &u.Active, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt, &u.Preferences); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// FirstTenantForUser returns the user's first active tenant id, or 0.
func (s *Store) FirstTenantForUser(userID int64) (int64, string, error) {
	var id int64
	var slug string
	err := s.db.QueryRow(`
		SELECT t.id, t.slug FROM tenants t
		JOIN tenant_memberships m ON m.tenant_id = t.id
		WHERE m.user_id = $1 AND t.active = true
		ORDER BY t.id LIMIT 1`, userID).Scan(&id, &slug)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return id, slug, err
}

// TenantForUser validates that the user is a member of the given active tenant.
// Returns the tenant id + slug, or 0 when there is no membership.
func (s *Store) TenantForUser(userID, tenantID int64) (int64, string, error) {
	var id int64
	var slug string
	err := s.db.QueryRow(`
		SELECT t.id, t.slug FROM tenants t
		JOIN tenant_memberships m ON m.tenant_id = t.id
		WHERE m.user_id = $1 AND m.tenant_id = $2 AND t.active = true`, userID, tenantID).Scan(&id, &slug)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return id, slug, err
}

// TenantSlugByID returns the slug of an active tenant, or "" when missing.
func (s *Store) TenantSlugByID(tenantID int64) (string, error) {
	var slug string
	err := s.db.QueryRow(`SELECT slug FROM tenants WHERE id = $1 AND active = true`, tenantID).Scan(&slug)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return slug, err
}

// UpdateUserActive toggles a user's active flag.
func (s *Store) UpdateUserActive(id int64, active bool) error {
	result, err := s.db.Exec(`UPDATE users SET active = $1, updated_at = NOW() WHERE id = $2 AND deleted_at IS NULL`, active, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return errors.New("user not found")
	}
	return nil
}

// GetUserPreferences returns the user's preferences JSON.
func (s *Store) GetUserPreferences(userID int64) (Preferences, error) {
	var prefs Preferences
	err := s.db.QueryRow(`SELECT COALESCE(preferences, '{}') FROM users WHERE id = $1 AND deleted_at IS NULL`, userID).Scan(&prefs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("user not found")
	}
	if err != nil {
		return nil, err
	}
	if prefs == nil {
		prefs = make(Preferences)
	}
	return prefs, nil
}

// UpdateUserPreferences overwrites the user's preferences JSON.
func (s *Store) UpdateUserPreferences(userID int64, prefs Preferences) error {
	result, err := s.db.Exec(`UPDATE users SET preferences = $1, updated_at = NOW() WHERE id = $2 AND deleted_at IS NULL`, prefs, userID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return errors.New("user not found")
	}
	return nil
}

// UpdateThemePreferences updates only the theme sub-key in preferences.
func (s *Store) UpdateThemePreferences(userID int64, theme ThemePreferences) error {
	result, err := s.db.Exec(
		`UPDATE users SET preferences = jsonb_set(COALESCE(preferences, '{}'), '{theme}', $1::jsonb, true),
		updated_at = NOW() WHERE id = $2 AND deleted_at IS NULL`,
		theme, userID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return errors.New("user not found")
	}
	return nil
}

// UpdateAvatarPreference stores (or, when both are empty, clears) the user's
// profile avatar: the owning file id plus its cached public serve URL.
func (s *Store) UpdateAvatarPreference(userID int64, fileID, url string) error {
	if fileID == "" && url == "" {
		result, err := s.db.Exec(
			`UPDATE users SET preferences = COALESCE(preferences, '{}') - ARRAY['avatar_file_id','avatar_url']::text[],
			updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL`, userID)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return errors.New("user not found")
		}
		return nil
	}
	result, err := s.db.Exec(
		`UPDATE users SET preferences = jsonb_set(jsonb_set(COALESCE(preferences, '{}'), '{avatar_file_id}', to_jsonb($1::text), true), '{avatar_url}', to_jsonb($2::text), true),
		updated_at = NOW() WHERE id = $3 AND deleted_at IS NULL`,
		fileID, url, userID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return errors.New("user not found")
	}
	return nil
}

// Service issues tokens and manages users.
type Service struct {
	store     *Store
	jwtSecret string

	superAdminUsername string
	superAdminPassword string
	superAdminName     string
	superAdminSurname  string
	superAdminUserName string
	superAdminEmail    string
}

func NewService(store *Store, jwtSecret string) *Service {
	return &Service{store: store, jwtSecret: jwtSecret}
}

// SetSuperAdminConfig configures the platform superadmin seed credentials.
func (s *Service) SetSuperAdminConfig(username, password, name, surname, userName, email string) {
	s.superAdminUsername = username
	s.superAdminPassword = password
	s.superAdminName = name
	s.superAdminSurname = surname
	s.superAdminUserName = userName
	s.superAdminEmail = email
}

// SeedSuperAdmin guarantees the platform superadmin exists with env credentials.
func (s *Service) SeedSuperAdmin() error {
	if s.superAdminUsername == "" || s.superAdminPassword == "" {
		return errors.New("SUPER_ADMIN_USERNAME and SUPER_ADMIN_PASSWORD must be set")
	}

	existing, err := s.store.GetUserByUsername(s.superAdminUsername)
	if err != nil {
		return fmt.Errorf("checking super admin: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(s.superAdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hashing super admin password: %w", err)
	}

	if existing != nil {
		// Refresh credentials from env on every boot.
		_, err := s.store.db.Exec(
			`UPDATE users SET password_hash = $1, platform_role = 0, active = true, deleted_at = NULL, updated_at = NOW() WHERE id = $2`,
			string(hash), existing.ID)
		return err
	}

	name := orDefault(s.superAdminName, "Super")
	surname := orDefault(s.superAdminSurname, "Admin")
	userName := orDefault(s.superAdminUserName, "superadmin")
	email := orDefault(s.superAdminEmail, "superadmin@synapta.local")
	_, err = s.store.CreateUser(s.superAdminUsername, string(hash), name, surname, userName, email, 0)
	return err
}

// Login verifies credentials and issues a tenant-scoped JWT.
func (s *Service) Login(username, password string) (*TokenResponse, error) {
	return s.loginAs(username, password, nil)
}

// LoginForTenant authenticates the user and issues a token scoped to the
// given tenant (validating the membership). tenantPerms supplies the
// membership permissions to embed in the token.
func (s *Service) LoginForTenant(username, password string, tenantID int64, tenantPerms []string) (*TokenResponse, error) {
	return s.loginAs(username, password, &loginTenant{ID: tenantID, Permissions: tenantPerms})
}

type loginTenant struct {
	ID          int64
	Permissions []string
}

func (s *Service) loginAs(username, password string, tenantSel *loginTenant) (*TokenResponse, error) {
	user, err := s.store.GetUserByUsername(username)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, ErrInvalidCreds
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCreds
	}

	// Default tenant = first active membership, or the explicitly selected one.
	var tenantID int64
	var tenantSlug string
	var memberPerms []string
	if tenantSel != nil && tenantSel.ID > 0 {
		tenantID, tenantSlug, err = s.store.TenantForUser(user.ID, tenantSel.ID)
		if err != nil {
			return nil, err
		}
		memberPerms = tenantSel.Permissions
		if tenantID == 0 && user.PlatformRole == 0 {
			// El superadmin de plataforma opera en cualquier tenant activo
			// (equivale al bypass X-Tenant-Slug del middleware).
			slug, slugErr := s.store.TenantSlugByID(tenantSel.ID)
			if slugErr != nil {
				return nil, slugErr
			}
			if slug != "" {
				tenantID, tenantSlug = tenantSel.ID, slug
				if len(memberPerms) == 0 {
					memberPerms = []string{"*"}
				}
			}
		}
		if tenantID == 0 {
			return nil, ErrInvalidCreds
		}
	} else {
		tenantID, tenantSlug, err = s.store.FirstTenantForUser(user.ID)
		if err != nil {
			return nil, err
		}
	}

	now := time.Now()
	claims := jwt.MapClaims{
		"sub":        user.ID,
		"username":   user.Username,
		"name":       user.Name,
		"surname":    user.Surname,
		"user_name":  user.UserName,
		"email":      user.Email,
		"role_level": user.PlatformRole,
		"role_name":  roleName(user.PlatformRole),
		"iat":        now.Unix(),
		"exp":        now.Add(24 * time.Hour).Unix(),
	}
	if tenantID > 0 {
		claims["tenant_id"] = tenantID
		claims["tenant_slug"] = tenantSlug
		if memberPerms == nil {
			memberPerms = []string{}
		}
		claims["permissions"] = memberPerms
	}

	tokenStr, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(s.jwtSecret))
	if err != nil {
		return nil, err
	}

	return &TokenResponse{
		Token:       tokenStr,
		User:        toResponse(user),
		TenantID:    tenantID,
		TenantSlug:  tenantSlug,
		Permissions: memberPerms,
	}, nil
}

// Register creates a plain platform user (role USER).
func (s *Service) Register(req *RegisterRequest) (*UserResponse, error) {
	existing, err := s.store.GetUserByUsername(req.Username)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, ErrUserExists
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	userName := orDefault(req.UserName, req.Username)
	user, err := s.store.CreateUser(req.Username, string(hash), req.Name, req.Surname, userName, req.Email, 3)
	if err != nil {
		return nil, err
	}
	return toResponse(user), nil
}

// ListUsers returns all platform users.
func (s *Service) ListUsers() ([]UserResponse, error) {
	users, err := s.store.ListUsers()
	if err != nil {
		return nil, err
	}
	out := make([]UserResponse, 0, len(users))
	for i := range users {
		out = append(out, *toResponse(&users[i]))
	}
	return out, nil
}

// ListTenantUsers returns the users who are members of a tenant.
func (s *Service) ListTenantUsers(tenantID int64) ([]UserResponse, error) {
	users, err := s.store.ListTenantUsers(tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]UserResponse, 0, len(users))
	for i := range users {
		out = append(out, *toResponse(&users[i]))
	}
	return out, nil
}

// GetUserProfile returns a user by id.
func (s *Service) GetUserProfile(id int64) (*UserResponse, error) {
	u, err := s.store.GetUserByID(id)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, errors.New("user not found")
	}
	return toResponse(u), nil
}

// GetThemePreferences returns the user's theme preferences.
func (s *Service) GetThemePreferences(userID int64) (ThemePreferences, error) {
	prefs, err := s.store.GetUserPreferences(userID)
	if err != nil {
		return ThemePreferences{}, err
	}
	themeRaw, ok := prefs["theme"]
	if !ok {
		return ThemePreferences{Palette: "violet", Mode: "light"}, nil
	}
	themeBytes, err := json.Marshal(themeRaw)
	if err != nil {
		return ThemePreferences{}, err
	}
	var theme ThemePreferences
	if err := json.Unmarshal(themeBytes, &theme); err != nil {
		return ThemePreferences{}, err
	}
	return theme, nil
}

// UpdateThemePreferences saves the user's theme preferences.
func (s *Service) UpdateThemePreferences(userID int64, theme ThemePreferences) error {
	return s.store.UpdateThemePreferences(userID, theme)
}

// UpdateUserAvatar stores the user's profile avatar (file id + public serve
// URL) or clears it when both values are empty. Returns the fresh profile.
func (s *Service) UpdateUserAvatar(userID int64, avatarFileID, avatarURL string) (*UserResponse, error) {
	if len(avatarFileID) > 128 {
		return nil, errors.New("avatar_file_id too long")
	}
	if len(avatarURL) > 2048 {
		return nil, errors.New("avatar_url too long")
	}
	if err := s.store.UpdateAvatarPreference(userID, avatarFileID, avatarURL); err != nil {
		return nil, err
	}
	return s.GetUserProfile(userID)
}

func roleName(level int) string {
	switch level {
	case 0:
		return "SUPER_ADMIN"
	case 1:
		return "ADMIN"
	case 2:
		return "DIRECTOR"
	default:
		return "USER"
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func toResponse(u *User) *UserResponse {
	resp := &UserResponse{
		ID: u.ID, Username: u.Username, Name: u.Name, Surname: u.Surname,
		UserName: u.UserName, Email: u.Email, RoleLevel: u.PlatformRole,
		RoleName: roleName(u.PlatformRole), Active: u.Active,
	}
	if v, ok := u.Preferences["avatar_file_id"].(string); ok {
		resp.AvatarFileID = v
	}
	if v, ok := u.Preferences["avatar_url"].(string); ok {
		resp.AvatarURL = v
	}
	return resp
}

// ─── DTOs ─────────────────────────────────────────────────────

type RegisterRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
	Name     string `json:"name"`
	Surname  string `json:"surname"`
	UserName string `json:"user_name"`
	Email    string `json:"email"`
}

type LoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type UserResponse struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Name      string `json:"name"`
	Surname   string `json:"surname"`
	UserName  string `json:"user_name"`
	Email     string `json:"email"`
	RoleLevel int    `json:"role_level"`
	RoleName  string `json:"role_name"`
	Active    bool   `json:"active"`
	// Profile avatar: owning file id plus the cached public serve URL,
	// both persisted under users.preferences.
	AvatarFileID string `json:"avatar_file_id,omitempty"`
	AvatarURL    string `json:"avatar_url,omitempty"`
}

type TokenResponse struct {
	Token       string        `json:"token"`
	User        *UserResponse `json:"user"`
	TenantID    int64         `json:"tenant_id"`
	TenantSlug  string        `json:"tenant_slug,omitempty"`
	Permissions []string      `json:"permissions,omitempty"`
}

// ─── HTTP layer ───────────────────────────────────────────────

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Login handles POST /auth/login
func (h *Handler) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	token, err := h.svc.Login(req.Username, req.Password)
	if err != nil {
		if errors.Is(err, ErrInvalidCreds) {
			utils.Unauthorized(c, err.Error())
			return
		}
		utils.InternalError(c, "internal server error")
		return
	}
	utils.Success(c, token)
}

// Register handles POST /auth/register
func (h *Handler) Register(c *gin.Context) {
	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	user, err := h.svc.Register(&req)
	if err != nil {
		if errors.Is(err, ErrUserExists) {
			utils.Conflict(c, err.Error())
			return
		}
		utils.InternalError(c, "internal server error")
		return
	}
	utils.Created(c, user)
}

// GetProfile handles GET /auth/profile
func (h *Handler) GetProfile(c *gin.Context) {
	id := utils.UserIDFromContext(c)
	if id <= 0 {
		utils.Unauthorized(c, "invalid token")
		return
	}
	user, err := h.svc.GetUserProfile(id)
	if err != nil {
		utils.NotFound(c, err.Error())
		return
	}
	utils.Success(c, user)
}

// GetTheme handles GET /user/preferences/theme
func (h *Handler) GetTheme(c *gin.Context) {
	id := utils.UserIDFromContext(c)
	if id <= 0 {
		utils.Unauthorized(c, "invalid token")
		return
	}
	theme, err := h.svc.GetThemePreferences(id)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, theme)
}

// UpdateTheme handles POST /user/preferences/theme
func (h *Handler) UpdateTheme(c *gin.Context) {
	id := utils.UserIDFromContext(c)
	if id <= 0 {
		utils.Unauthorized(c, "invalid token")
		return
	}
	var req struct {
		Theme ThemePreferences `json:"theme" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	if err := h.svc.UpdateThemePreferences(id, req.Theme); err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, req.Theme)
}

// UpdateAvatar handles PUT /user/profile/avatar
// Accepts {avatar_file_id, avatar_url}; both empty clears the avatar.
func (h *Handler) UpdateAvatar(c *gin.Context) {
	id := utils.UserIDFromContext(c)
	if id <= 0 {
		utils.Unauthorized(c, "invalid token")
		return
	}
	var req struct {
		AvatarFileID string `json:"avatar_file_id"`
		AvatarURL    string `json:"avatar_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	fileID := strings.TrimSpace(req.AvatarFileID)
	url := strings.TrimSpace(req.AvatarURL)
	user, err := h.svc.UpdateUserAvatar(id, fileID, url)
	if err != nil {
		if strings.Contains(err.Error(), "user not found") {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, user)
}

// ListUsers handles GET /admin/users
func (h *Handler) ListUsers(c *gin.Context) {
	users, err := h.svc.ListUsers()
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, users)
}

// ListTenantUsers handles GET /admin/tenants/:id/users (platform admins).
// Returns the users who belong to a tenant, for admin consoles.
func (h *Handler) ListTenantUsers(c *gin.Context) {
	var id int64
	if _, err := fmt.Sscanf(c.Param("id"), "%d", &id); err != nil || id <= 0 {
		utils.BadRequest(c, "invalid tenant id")
		return
	}
	users, err := h.svc.ListTenantUsers(id)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, users)
}

// Module registers auth routes.
type Module struct {
	hdl *Handler
}

func NewModule(hdl *Handler) *Module { return &Module{hdl: hdl} }

func (m *Module) Name() string { return "auth" }

func (m *Module) Register(rg *gin.RouterGroup, authMw, _, adminMw gin.HandlerFunc) {
	pub := rg.Group("/auth")
	{
		pub.POST("/login", m.hdl.Login)
		pub.POST("/register", m.hdl.Register)
	}

	priv := rg.Group("/auth")
	priv.Use(authMw)
	{
		priv.GET("/profile", m.hdl.GetProfile)
	}

	user := rg.Group("/user")
	user.Use(authMw)
	{
		user.GET("/preferences/theme", m.hdl.GetTheme)
		user.POST("/preferences/theme", m.hdl.UpdateTheme)
		user.PUT("/profile/avatar", m.hdl.UpdateAvatar)
	}

	adm := rg.Group("/admin")
	adm.Use(authMw, adminMw)
	{
		adm.GET("/users", m.hdl.ListUsers)
		adm.GET("/tenants/:id/users", m.hdl.ListTenantUsers)
	}
}
