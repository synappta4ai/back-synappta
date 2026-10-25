package model

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"synapta/internal/utils"
)

// MasterPrefsView is GET /models/master: the whole static catalog annotated
// with platform flags (nothing is hidden here: disabled models must stay
// visible so the master can re-enable them), plus orphan pref rows for
// names that no longer exist in the catalog.
type MasterPrefsView struct {
	Models  []Model               `json:"models"`
	Prefs   map[string]MasterPref `json:"prefs"`
	Orphans []string              `json:"orphans"`
}

// MasterPrefs serves the master curation screen (platform superadmin).
func (m *Module) MasterPrefs(c *gin.Context) {
	if m.masterPrefs == nil {
		utils.Error(c, http.StatusNotImplemented, "model master prefs not configured")
		return
	}
	all, err := m.masterPrefs.All()
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	models := List("")
	known := make(map[string]bool, len(models))
	for i := range models {
		key := strings.ToLower(strings.TrimSpace(models[i].Name))
		known[key] = true
		if p, ok := all[key]; ok {
			models[i].Recommended = p.IsRecommended()
			if p.IsRecommended() {
				models[i].RecommendTags = append([]string(nil), p.RecommendTags...)
			}
			models[i].Favorite = p.FavoriteRank > 0
		}
	}
	orphans := []string{}
	for name := range all {
		if !known[strings.ToLower(strings.TrimSpace(name))] {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	utils.Success(c, MasterPrefsView{Models: models, Prefs: all, Orphans: orphans})
}

// MasterUpsertPrefs replaces platform rows (PUT /models/master).
func (m *Module) MasterUpsertPrefs(c *gin.Context) {
	if m.masterPrefs == nil {
		utils.Error(c, http.StatusNotImplemented, "model master prefs not configured")
		return
	}
	var prefs []MasterPref
	if err := c.ShouldBindJSON(&prefs); err != nil {
		utils.BadRequest(c, "invalid prefs body: "+err.Error())
		return
	}
	for i := range prefs {
		if err := NormalizeMasterPref(&prefs[i]); err != nil {
			utils.BadRequest(c, err.Error())
			return
		}
	}
	if err := m.masterPrefs.Upsert(prefs); err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	m.MasterPrefs(c)
}

// TenantPrefsView is GET /models/prefs: the tenant's effective listing
// (master veto applied, badges stamped), the names the tenant disabled
// itself (so the UI can offer re-enabling them), and the tenant's own rows
// (so the UI shows its favorites, not the master's).
type TenantPrefsView struct {
	Models   []Model               `json:"models"`
	Disabled []string              `json:"disabled"`
	Mine     map[string]TenantPref `json:"mine"`
}

// TenantPrefs serves the tenant curation screen (tenant admin).
func (m *Module) TenantPrefs(c *gin.Context) {
	access, ok := m.tenantAccess(c)
	if !ok {
		utils.Error(c, http.StatusNotImplemented, "tenant model prefs not configured")
		return
	}
	models, handled := m.usableAPIModels(c, "")
	if handled {
		return
	}
	models = append(models, m.liveUsable(c)...)
	master := map[string]MasterPref{}
	if m.masterPrefs != nil {
		master = prefsOrEmpty("master", m.masterPrefs.All)
	}
	tenant := prefsOrEmpty("tenant", access.All)
	visible, disabled := tenantPrefsView(models, master, tenant)
	utils.Success(c, TenantPrefsView{Models: visible, Disabled: disabled, Mine: tenant})
}

// TenantUpsertPrefs replaces tenant rows (PUT /models/prefs).
func (m *Module) TenantUpsertPrefs(c *gin.Context) {
	access, ok := m.tenantAccess(c)
	if !ok {
		utils.Error(c, http.StatusNotImplemented, "tenant model prefs not configured")
		return
	}
	var prefs []TenantPref
	if err := c.ShouldBindJSON(&prefs); err != nil {
		utils.BadRequest(c, "invalid prefs body: "+err.Error())
		return
	}
	for i := range prefs {
		prefs[i].ModelName = strings.TrimSpace(prefs[i].ModelName)
		if prefs[i].ModelName == "" {
			utils.BadRequest(c, "every pref needs a model_name")
			return
		}
	}
	if err := access.Upsert(prefs); err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	m.TenantPrefs(c)
}

func (m *Module) tenantAccess(c *gin.Context) (TenantPrefsAccess, bool) {
	if m.tenantPrefs == nil {
		return nil, false
	}
	return m.tenantPrefs(c)
}

// tenantPrefsView splits a merged listing into what the tenant sees
// (master veto applied, badges stamped, favorites first) and the names the
// tenant disabled itself. Master-disabled models never surface here: the
// tenant inherits them silently and cannot touch them.
func tenantPrefsView(models []Model, master map[string]MasterPref, tenant map[string]TenantPref) ([]Model, []string) {
	enabledTenant := make(map[string]TenantPref, len(tenant))
	disabled := []string{}
	for key, tp := range tenant {
		if tp.Enabled {
			enabledTenant[key] = tp
		} else {
			disabled = append(disabled, tp.ModelName)
		}
	}
	sort.Strings(disabled)
	visible := make([]Model, 0, len(models))
	for _, mdl := range models {
		key := strings.ToLower(strings.TrimSpace(mdl.Name))
		if mp, ok := master[key]; ok && !mp.Enabled {
			continue
		}
		if tp, ok := tenant[key]; ok && !tp.Enabled {
			continue
		}
		visible = append(visible, mdl)
	}
	return ApplyPrefs(visible, master, enabledTenant), disabled
}
