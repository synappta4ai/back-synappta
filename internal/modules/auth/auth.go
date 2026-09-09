// Package auth handles platform users and JWT issuance.
//
// Users are global (system schema). A token carries a tenant_id chosen at
// login (the user's first active membership unless overridden), which the
// tenancy middleware uses to scope every request.
package auth

import (
	"database/sql"
	"errors"
	"fmt"
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
	ID           int64      `json:"id"`
	Username     string     `json:"username"`
	Name         string     `json:"name"`
	Surname      string     `json:"surname"`
	UserName     string     `json:"user_name"`
	Email        string     `json:"email"`
	PlatformRole int        `json:"platform_role"`
	Active       bool       `json:"active"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
	PasswordHash string     `json:"-"`
}

const userCols = `id, username, name, surname, COALESCE(user_name,'') AS user_name,
	COALESCE(email,'') AS email, platform_role, active, created_at, updated_at, deleted_at`

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
		Scan(&u.ID, &u.Username, &u.Name, &u.Surname, &u.UserName, &u.Email, &u.PlatformRole, &u.Active, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Store) GetUserByUsername(username string) (*User, error) {
	query := `SELECT ` + userCols + `, password_hash FROM users WHERE username = $1 AND active = true AND deleted_at IS NULL`
	u := &User{}
	err := s.db.QueryRow(query, username).
		Scan(&u.ID, &u.Username, &u.Name, &u.Surname, &u.UserName, &u.Email, &u.PlatformRole, &u.Active, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt, &u.PasswordHash)
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
		Scan(&u.ID, &u.Username, &u.Name, &u.Surname, &u.UserName, &u.Email, &u.PlatformRole, &u.Active, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u, nil
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
		if err := rows.Scan(&u.ID, &u.Username, &u.Name, &u.Surname, &u.UserName, &u.Email, &u.PlatformRole, &u.Active, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt); err != nil {
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

	// Default tenant = first active membership.
	tenantID, tenantSlug, err := s.store.FirstTenantForUser(user.ID)
	if err != nil {
		return nil, err
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
	}

	tokenStr, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(s.jwtSecret))
	if err != nil {
		return nil, err
	}

	return &TokenResponse{
		Token:      tokenStr,
		User:       toResponse(user),
		TenantID:   tenantID,
		TenantSlug: tenantSlug,
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
	return &UserResponse{
		ID: u.ID, Username: u.Username, Name: u.Name, Surname: u.Surname,
		UserName: u.UserName, Email: u.Email, RoleLevel: u.PlatformRole,
		RoleName: roleName(u.PlatformRole), Active: u.Active,
	}
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
}

type TokenResponse struct {
	Token      string        `json:"token"`
	User       *UserResponse `json:"user"`
	TenantID   int64         `json:"tenant_id"`
	TenantSlug string        `json:"tenant_slug,omitempty"`
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

// ListUsers handles GET /admin/users
func (h *Handler) ListUsers(c *gin.Context) {
	users, err := h.svc.ListUsers()
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

	adm := rg.Group("/admin")
	adm.Use(authMw, adminMw)
	{
		adm.GET("/users", m.hdl.ListUsers)
	}
}
