// Gestión de usuarios por tenant (plataforma superadmin):
// crea usuarios globales con membresía en la empresa, cambia su rol y sus
// permisos por tenant, y los quita. Los permisos efectivos de un usuario son
// la UNIÓN de los permisos base de la empresa (tenants.permissions, migración
// 00003) y los extras por membresía (tenant_memberships.permissions, 00002);
// esa unión viaja en el JWT al login. El rol 0 (SUPER_ADMIN) nunca se asigna
// por esta vía: los endpoints lo rechazan.
package tenant

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"synapta/internal/modules/auth"
	"synapta/internal/tenancy"
	"synapta/internal/utils"
)

// Permission catalog: claves estables que el front muestra agrupadas.
// '*' habilita todo (equivale a superadmin funcional dentro del tenant).
var permissionCatalog = []struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}{
	{"*", "Todos los permisos"},
	{"projects.view", "Ver proyectos"},
	{"projects.manage", "Crear y editar proyectos"},
	{"generations.create", "Generar con IA"},
	{"generations.view", "Ver generaciones"},
	{"library.view", "Ver biblioteca de recursos"},
	{"library.manage", "Subir y borrar recursos"},
	{"ingredients.manage", "Gestionar personajes, locaciones y props"},
	{"presets.manage", "Gestionar presets y skills"},
	{"logs.view", "Ver logs de generación"},
	{"members.manage", "Gestionar usuarios del tenant"},
}

// CatalogPermissions devuelve el catálogo para el front.
func CatalogPermissions() []struct {
	Key   string `json:"key"`
	Label string `json:"label"`
} {
	return permissionCatalog
}

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]{3,64}$`)
var permissionKeyRe = regexp.MustCompile(`^\*|[a-z][a-z_]*(\.[a-z_]+)*$`)

// MemberView is the API representation of a tenant member.
type MemberView struct {
	auth.UserResponse
	// RoleLevel/RoleName de la membresía (pueden diferir del platform_role del usuario).
	RoleLevel int      `json:"role_level"`
	RoleName  string   `json:"role_name"`
	Perm      []string `json:"permissions"`
}

// memberStore reads/writes memberships; reuses the auth store for users.
type memberStore struct {
	db *sql.DB
}

func (s *memberStore) listMembers(tenantID int64) ([]MemberView, error) {
	rows, err := s.db.Query(`
		SELECT u.id, u.username, u.name, u.surname, COALESCE(u.user_name,''), COALESCE(u.email,''),
		       u.active, m.role_level, to_jsonb(COALESCE(m.permissions, '{}'::text[]))::text
		FROM tenant_memberships m
		JOIN users u ON u.id = m.user_id
		WHERE m.tenant_id = $1 AND u.deleted_at IS NULL
		ORDER BY m.role_level, u.id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemberView
	for rows.Next() {
		var u MemberView
		var permsJSON string
		if err := rows.Scan(&u.ID, &u.Username, &u.Name, &u.Surname, &u.UserName, &u.Email,
			&u.Active, &u.RoleLevel, &permsJSON); err != nil {
			return nil, err
		}
		perms := parsePermsJSON(permsJSON)
		u.Perm = perms
		u.RoleName = roleNameFor(u.RoleLevel)
		out = append(out, u)
	}
	return out, rows.Err()
}

// parsePermsJSON parses a JSON array of strings (to_jsonb of a text[]) into a
// slice. database/sql no escanea text[] directo a []string, por eso las
// consultas lo serializan con to_jsonb(...)::text.
func parsePermsJSON(raw string) []string {
	var perms []string
	if raw == "" || raw == "[]" {
		return []string{}
	}
	if err := json.Unmarshal([]byte(raw), &perms); err != nil || perms == nil {
		return []string{}
	}
	return perms
}

// createUserWithMembership creates a global user and its membership atomically.
func (s *memberStore) createUserWithMembership(tenantID int64, username, password, name, surname, userName, email string, roleLevel int, perms []string) (*MemberView, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(userName) == "" {
		userName = username
	}
	var userID int64
	err = tx.QueryRow(`
		INSERT INTO users (username, password_hash, name, surname, user_name, email, platform_role, active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, true)
		RETURNING id`,
		username, string(hash), name, surname, userName, email, roleLevel).Scan(&userID)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return nil, fmt.Errorf("el usuario %q ya existe", username)
		}
		return nil, err
	}
	if err := upsertMembershipTx(tx, tenantID, userID, roleLevel, perms); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	roleName := roleNameFor(roleLevel)
	if perms == nil {
		perms = []string{}
	}
	return &MemberView{
		UserResponse: auth.UserResponse{ID: userID, Username: username, Name: name, Surname: surname, UserName: userName, Email: email, Active: true},
		RoleLevel:    roleLevel, RoleName: roleName, Perm: perms,
	}, nil
}

// addExistingUser adds a membership for an already-registered global user.
func (s *memberStore) addExistingUser(tenantID int64, userID int64, roleLevel int, perms []string) (*MemberView, error) {
	var active bool
	var username string
	err := s.db.QueryRow(`SELECT username, active FROM users WHERE id = $1 AND deleted_at IS NULL`, userID).
		Scan(&username, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("usuario %d no encontrado", userID)
	}
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, fmt.Errorf("el usuario %q está inactivo", username)
	}
	if err := upsertMembership(s.db, tenantID, userID, roleLevel, perms); err != nil {
		return nil, err
	}
	if perms == nil {
		perms = []string{}
	}
	return &MemberView{
		UserResponse: auth.UserResponse{ID: userID, Username: username},
		RoleLevel:    roleLevel, RoleName: roleNameFor(roleLevel), Perm: perms,
	}, nil
}

func upsertMembershipTx(tx *sql.Tx, tenantID, userID int64, roleLevel int, perms []string) error {
	_, err := tx.Exec(`
		INSERT INTO tenant_memberships (tenant_id, user_id, role_level, permissions)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, user_id)
		DO UPDATE SET role_level = EXCLUDED.role_level, permissions = EXCLUDED.permissions, updated_at = NOW()`,
		tenantID, userID, roleLevel, perms)
	return err
}

func upsertMembership(dbh *sql.DB, tenantID, userID int64, roleLevel int, perms []string) error {
	_, err := dbh.Exec(`
		INSERT INTO tenant_memberships (tenant_id, user_id, role_level, permissions)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, user_id)
		DO UPDATE SET role_level = EXCLUDED.role_level, permissions = EXCLUDED.permissions, updated_at = NOW()`,
		tenantID, userID, roleLevel, perms)
	return err
}

func (s *memberStore) updateRole(tenantID, userID int64, roleLevel int) error {
	result, err := s.db.Exec(
		`UPDATE tenant_memberships SET role_level = $1, updated_at = NOW() WHERE tenant_id = $2 AND user_id = $3`,
		roleLevel, tenantID, userID)
	if err != nil {
		return err
	}
	return requireRows(result)
}

func (s *memberStore) updatePermissions(tenantID, userID int64, perms []string) error {
	result, err := s.db.Exec(
		`UPDATE tenant_memberships SET permissions = $1, updated_at = NOW() WHERE tenant_id = $2 AND user_id = $3`,
		perms, tenantID, userID)
	if err != nil {
		return err
	}
	return requireRows(result)
}

func (s *memberStore) removeMember(tenantID, userID int64) error {
	result, err := s.db.Exec(`DELETE FROM tenant_memberships WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID)
	if err != nil {
		return err
	}
	return requireRows(result)
}

// membershipsForUser returns (tenantID, roleLevel, effectivePermissions) of
// every active membership. Effective = empresa (tenants.permissions) ∪ extras
// de la membresía, para que cambiar los permisos de la empresa afecte en el
// acto a todos sus usuarios.
func (s *memberStore) membershipsForUser(userID int64) ([]tenancy.Membership, error) {
	rows, err := s.db.Query(`
		SELECT m.tenant_id, m.role_level,
		       to_jsonb(COALESCE(m.permissions, '{}'::text[]))::text,
		       to_jsonb(COALESCE(t.permissions, '{}'::text[]))::text
		FROM tenant_memberships m
		JOIN tenants t ON t.id = m.tenant_id
		WHERE m.user_id = $1 AND t.active = true`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tenancy.Membership
	for rows.Next() {
		var m tenancy.Membership
		var memberJSON, tenantJSON string
		if err := rows.Scan(&m.TenantID, &m.RoleLevel, &memberJSON, &tenantJSON); err != nil {
			return nil, err
		}
		m.Permissions = unionPerms(parsePermsJSON(memberJSON), parsePermsJSON(tenantJSON))
		out = append(out, m)
	}
	return out, rows.Err()
}

func requireRows(result sql.Result) error {
	n, _ := result.RowsAffected()
	if n == 0 {
		return errors.New("membresía no encontrada")
	}
	return nil
}

// unionPerms merges two permission sets; '*' gana sobre todo lo demás.
func unionPerms(sets ...[]string) []string {
	for _, set := range sets {
		for _, p := range set {
			if p == "*" {
				return []string{"*"}
			}
		}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, set := range sets {
		for _, p := range set {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

func roleNameFor(level int) string {
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

func normalizePermissions(raw []string) ([]string, error) {
	if len(raw) > 64 {
		return nil, errors.New("demasiados permisos")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(strings.ToLower(p))
		if p == "" {
			continue
		}
		if len(p) > 64 || !permissionKeyRe.MatchString(p) {
			return nil, fmt.Errorf("permiso inválido: %q", p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// ─── HTTP handlers (montados bajo /tenants/:id/users, superadmin) ──────────

// TenantUsersHandler serves the per-tenant user management endpoints.
type TenantUsersHandler struct {
	store *memberStore
}

func NewTenantUsersHandler(db *sql.DB) *TenantUsersHandler {
	return &TenantUsersHandler{store: &memberStore{db: db}}
}

// List handles GET /tenants/:id/users
func (h *TenantUsersHandler) List(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	members, err := h.store.listMembers(id)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if members == nil {
		members = []MemberView{}
	}
	utils.Success(c, members)
}

// Create handles POST /tenants/:id/users
// Body: {username, password, name?, surname?, user_name?, email?, role_level?, permissions?}
// or: {user_id, role_level?, permissions?} to attach an existing global user.
func (h *TenantUsersHandler) Create(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req struct {
		UserID      int64    `json:"user_id"`
		Username    string   `json:"username"`
		Password    string   `json:"password"`
		Name        string   `json:"name"`
		Surname     string   `json:"surname"`
		UserName    string   `json:"user_name"`
		Email       string   `json:"email"`
		RoleLevel   *int     `json:"role_level"`
		Permissions []string `json:"permissions"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	roleLevel := 3
	if req.RoleLevel != nil {
		if *req.RoleLevel < 1 || *req.RoleLevel > 3 {
			utils.BadRequest(c, "role_level debe estar entre 1 (ADMIN) y 3 (USER); SUPER_ADMIN no se asigna por esta vía")
			return
		}
		roleLevel = *req.RoleLevel
	}
	perms, err := normalizePermissions(req.Permissions)
	if err != nil {
		utils.BadRequest(c, err.Error())
		return
	}

	if req.UserID > 0 {
		member, err := h.store.addExistingUser(id, req.UserID, roleLevel, perms)
		if err != nil {
			utils.BadRequest(c, err.Error())
			return
		}
		utils.Created(c, member)
		return
	}

	username := strings.TrimSpace(strings.ToLower(req.Username))
	if !usernameRe.MatchString(username) {
		utils.BadRequest(c, "username inválido: 3-64 caracteres [a-z0-9._-]")
		return
	}
	if len(req.Password) < 8 {
		utils.BadRequest(c, "password debe tener al menos 8 caracteres")
		return
	}
	member, err := h.store.createUserWithMembership(id, username, req.Password,
		strings.TrimSpace(req.Name), strings.TrimSpace(req.Surname),
		strings.TrimSpace(req.UserName), strings.TrimSpace(req.Email), roleLevel, perms)
	if err != nil {
		if strings.Contains(err.Error(), "ya existe") {
			utils.Conflict(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Created(c, member)
}

// UpdateRole handles PATCH /tenants/:id/users/:userId/role  {role_level}
// (rol 0/SUPER_ADMIN rechazado: solo existe el superadmin de plataforma).
func (h *TenantUsersHandler) UpdateRole(c *gin.Context) {
	tenantID, userID, ok := parseTwoIDs(c)
	if !ok {
		return
	}
	var req struct {
		RoleLevel *int `json:"role_level"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.RoleLevel == nil {
		utils.BadRequest(c, "role_level requerido (1-3)")
		return
	}
	if *req.RoleLevel < 1 || *req.RoleLevel > 3 {
		utils.BadRequest(c, "role_level debe estar entre 1 (ADMIN) y 3 (USER); SUPER_ADMIN no se asigna por esta vía")
		return
	}
	if err := h.store.updateRole(tenantID, userID, *req.RoleLevel); err != nil {
		utils.NotFound(c, err.Error())
		return
	}
	utils.Success(c, gin.H{"role_level": *req.RoleLevel, "role_name": roleNameFor(*req.RoleLevel)})
}

// UpdatePermissions handles PUT /tenants/:id/users/:userId/permissions  {permissions: [...]}
func (h *TenantUsersHandler) UpdatePermissions(c *gin.Context) {
	tenantID, userID, ok := parseTwoIDs(c)
	if !ok {
		return
	}
	var req struct {
		Permissions []string `json:"permissions"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	perms, err := normalizePermissions(req.Permissions)
	if err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	if err := h.store.updatePermissions(tenantID, userID, perms); err != nil {
		utils.NotFound(c, err.Error())
		return
	}
	utils.Success(c, gin.H{"permissions": perms})
}

// Remove handles DELETE /tenants/:id/users/:userId
func (h *TenantUsersHandler) Remove(c *gin.Context) {
	tenantID, userID, ok := parseTwoIDs(c)
	if !ok {
		return
	}
	if err := h.store.removeMember(tenantID, userID); err != nil {
		utils.NotFound(c, err.Error())
		return
	}
	utils.Success(c, nil)
}

// MembershipsForUserByUsername resolves a username to its active memberships
// (used by the tenant-aware login in main).
func (h *TenantUsersHandler) MembershipsForUserByUsername(username string) ([]tenancy.Membership, error) {
	var userID int64
	err := h.store.db.QueryRow(
		`SELECT id FROM users WHERE username = $1 AND active = true AND deleted_at IS NULL`, username,
	).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return h.store.membershipsForUser(userID)
}

// ListPlatformUsers handles GET /tenants/platform-users?q= (búsqueda para adjuntar existentes).
func (h *TenantUsersHandler) ListPlatformUsers(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	var rows *sql.Rows
	var err error
	if q != "" {
		pattern := "%" + q + "%"
		rows, err = h.store.db.Query(`
			SELECT id, username, name, surname, COALESCE(email,''), active FROM users
			WHERE deleted_at IS NULL AND (username ILIKE $1 OR name ILIKE $1 OR surname ILIKE $1 OR email ILIKE $1)
			ORDER BY username LIMIT 25`, pattern)
	} else {
		rows, err = h.store.db.Query(`
			SELECT id, username, name, surname, COALESCE(email,''), active FROM users
			WHERE deleted_at IS NULL ORDER BY username LIMIT 25`)
	}
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	defer rows.Close()
	type platformUser struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Name     string `json:"name"`
		Surname  string `json:"surname"`
		Email    string `json:"email"`
		Active   bool   `json:"active"`
	}
	out := []platformUser{}
	for rows.Next() {
		var u platformUser
		if err := rows.Scan(&u.ID, &u.Username, &u.Name, &u.Surname, &u.Email, &u.Active); err != nil {
			utils.InternalError(c, err.Error())
			return
		}
		out = append(out, u)
	}
	utils.Success(c, out)
}

func parseID(c *gin.Context) (int64, bool) {
	var id int64
	if _, err := fmt.Sscanf(c.Param("id"), "%d", &id); err != nil || id <= 0 {
		utils.BadRequest(c, "invalid tenant id")
		return 0, false
	}
	return id, true
}

func parseTwoIDs(c *gin.Context) (int64, int64, bool) {
	tenantID, ok := parseID(c)
	if !ok {
		return 0, 0, false
	}
	var userID int64
	if _, err := fmt.Sscanf(c.Param("userId"), "%d", &userID); err != nil || userID <= 0 {
		utils.BadRequest(c, "invalid user id")
		return 0, 0, false
	}
	return tenantID, userID, true
}
