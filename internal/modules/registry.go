// Package modules provides the Module interface and Registry for organizing
// feature modules.
//
// Each module is a self-contained unit that registers its own routes.
// All module routes are protected by default — auth middleware is applied
// to the entire module group. Modules can expose public routes where needed.
package modules

import "github.com/gin-gonic/gin"

// Module defines a self-contained feature module that registers its own routes.
type Module interface {
	// Name returns the module name (for debugging/logging).
	Name() string
	// Register sets up routes on the API v1 router group.
	// authMw is the JWT authentication middleware.
	// tenantMw resolves the per-request tenant (ignored by system modules).
	// adminMw checks for admin role (RequireRole(1)).
	Register(rg *gin.RouterGroup, authMw, tenantMw, adminMw gin.HandlerFunc)
}

// Registry holds all registered modules and sets them up on the router.
type Registry struct {
	modules []Module
}

// NewRegistry creates an empty module registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds a module to the registry.
func (r *Registry) Register(m Module) {
	r.modules = append(r.modules, m)
}

// Setup iterates all modules and calls Register on each.
func (r *Registry) Setup(v1 *gin.RouterGroup, authMw, tenantMw, adminMw gin.HandlerFunc) {
	for _, m := range r.modules {
		m.Register(v1, authMw, tenantMw, adminMw)
	}
}
