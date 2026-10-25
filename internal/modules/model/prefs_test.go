package model

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func prefModel(name, display string) Model {
	return Model{Name: name, DisplayName: display, Modality: ModalityImage, Generator: "x"}
}

func TestCheckAllowedMasterVetoWins(t *testing.T) {
	master := map[string]MasterPref{"soul": {ModelName: "soul", Enabled: false}}
	tenant := map[string]TenantPref{"soul": {ModelName: "soul", Enabled: true}}
	if err := CheckAllowed("soul", master, tenant); err == nil {
		t.Fatal("master-disabled model must stay off even when the tenant re-enables it")
	}
	// Case-insensitive match.
	if err := CheckAllowed("SOUL", master, nil); err == nil {
		t.Fatal("name matching must be case-insensitive")
	}
	if err := CheckAllowed("other", master, tenant); err != nil {
		t.Fatalf("unlisted model must be allowed: %v", err)
	}
	tenantOnly := map[string]TenantPref{"soul": {ModelName: "soul", Enabled: false}}
	if err := CheckAllowed("soul", nil, tenantOnly); err == nil {
		t.Fatal("tenant-disabled model must be blocked")
	}
}

func TestApplyPrefsFiltersAndFlags(t *testing.T) {
	models := []Model{prefModel("soul", "Soul"), prefModel("gem", "Gemini"), prefModel("flux", "Flux")}
	master := map[string]MasterPref{
		"soul": {ModelName: "soul", Enabled: false},
		"gem":  {ModelName: "gem", Enabled: true, RecommendTags: StringList{"eficiente"}},
	}
	tenant := map[string]TenantPref{
		"flux": {ModelName: "flux", Enabled: false},
	}
	got := ApplyPrefs(models, master, tenant)
	if len(got) != 1 || got[0].Name != "gem" {
		t.Fatalf("got %+v, want only gem", got)
	}
	if !got[0].Recommended {
		t.Fatal("recommended flag must be stamped")
	}
	if len(got[0].RecommendTags) != 1 || got[0].RecommendTags[0] != "eficiente" {
		t.Fatalf("recommend tags = %v, want [eficiente]", got[0].RecommendTags)
	}
	if got[0].Favorite {
		t.Fatal("no favorites here")
	}
}

func TestApplyPrefsFavoritesFirst(t *testing.T) {
	models := []Model{prefModel("a", "A"), prefModel("b", "B"), prefModel("c", "C"), prefModel("d", "D")}
	master := map[string]MasterPref{
		"b": {ModelName: "b", Enabled: true, FavoriteRank: 2},
		"c": {ModelName: "c", Enabled: true, FavoriteRank: 1},
	}
	tenant := map[string]TenantPref{
		"d": {ModelName: "d", Enabled: true, FavoriteRank: 1},
	}
	got := ApplyPrefs(models, master, tenant)
	want := []string{"d", "c", "b", "a"} // tenant fav, then master favs by rank, then rest in order
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("order = %v, want %v", names(got), want)
		}
		if i < 3 && !got[i].Favorite {
			t.Fatalf("%s must be flagged favorite", name)
		}
	}
	if got[3].Favorite {
		t.Fatal("plain model must not be flagged favorite")
	}
}

func TestApplyPrefsTenantFavoriteOutranksMaster(t *testing.T) {
	models := []Model{prefModel("a", "A"), prefModel("b", "B")}
	master := map[string]MasterPref{"a": {ModelName: "a", Enabled: true, FavoriteRank: 1}}
	tenant := map[string]TenantPref{"b": {ModelName: "b", Enabled: true, FavoriteRank: 5}}
	got := ApplyPrefs(models, master, tenant)
	// Tenant favorite wins the front even with a worse rank number.
	if got[0].Name != "b" || got[1].Name != "a" {
		t.Fatalf("order = %v, want [b a]", names(got))
	}
}

func TestTenantPrefsViewSplitsDisabled(t *testing.T) {
	models := []Model{prefModel("soul", "Soul"), prefModel("gem", "Gemini")}
	master := map[string]MasterPref{"gone": {ModelName: "gone", Enabled: false}}
	tenant := map[string]TenantPref{"gem": {ModelName: "gem", Enabled: false, FavoriteRank: 0}}
	visible, disabled := tenantPrefsView(models, master, tenant)
	if len(visible) != 1 || visible[0].Name != "soul" {
		t.Fatalf("visible = %v", names(visible))
	}
	if len(disabled) != 1 || disabled[0] != "gem" {
		t.Fatalf("disabled = %v", disabled)
	}
}

func names(models []Model) []string {
	out := make([]string, len(models))
	for i, m := range models {
		out[i] = m.Name
	}
	return out
}

// fakePrefs wires in-memory curation for handler tests (no database).
type fakeMasterPrefs struct {
	rows map[string]MasterPref
}

func (f *fakeMasterPrefs) All() (map[string]MasterPref, error) { return f.rows, nil }
func (f *fakeMasterPrefs) Upsert(prefs []MasterPref) error {
	for _, p := range prefs {
		f.rows[strings.ToLower(p.ModelName)] = p
	}
	return nil
}

type fakeTenantPrefs struct {
	rows map[string]TenantPref
}

func (f *fakeTenantPrefs) All() (map[string]TenantPref, error) { return f.rows, nil }
func (f *fakeTenantPrefs) Upsert(prefs []TenantPref) error {
	for _, p := range prefs {
		f.rows[strings.ToLower(p.ModelName)] = p
	}
	return nil
}

func testPrefsRouter(m *Module) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	v1 := r.Group("/api/v1")
	pass := func(c *gin.Context) { c.Next() }
	// The inline gate reads role_level from the context (real authMw sets
	// it); tests drive it through a header.
	roleMw := func(c *gin.Context) {
		if c.GetHeader("X-Test-Role") == "0" {
			c.Set("role_level", float64(0))
		} else {
			c.Set("role_level", float64(1))
		}
		c.Next()
	}
	m.Register(v1, pass, roleMw, pass)
	return r
}

func TestMasterEndpointsNeedSuperadmin(t *testing.T) {
	m := NewModule(func(*gin.Context) (CredentialLister, bool) { return nil, false }, "")
	m.SetMasterPrefsStore(&fakeMasterPrefs{rows: map[string]MasterPref{}})
	r := testPrefsRouter(m)

	// Tenant admin (level 1) is rejected…
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/models/master", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("tenant admin got %d, want 403", w.Code)
	}
	// …superadmin (level 0) passes.
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/models/master", nil)
	req.Header.Set("X-Test-Role", "0")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("superadmin got %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestMasterUpsertRoundTrip(t *testing.T) {
	m := NewModule(func(*gin.Context) (CredentialLister, bool) { return nil, false }, "")
	fake := &fakeMasterPrefs{rows: map[string]MasterPref{}}
	m.SetMasterPrefsStore(fake)

	r := testPrefsRouter(m)

	body := `[{"model_name":"soul","enabled":false,"favorite_rank":0},
		{"model_name":"gem","enabled":true,"recommend_tags":"eficiente","metadata":{"costo":"bajo"},"favorite_rank":2}]`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/models/master", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Role", "0")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT got %d: %s", w.Code, w.Body.String())
	}
	if fake.rows["soul"].Enabled {
		t.Fatal("soul must be stored disabled")
	}
	if !fake.rows["gem"].IsRecommended() || fake.rows["gem"].FavoriteRank != 2 {
		t.Fatalf("gem row = %+v", fake.rows["gem"])
	}
	if len(fake.rows["gem"].RecommendTags) != 1 || fake.rows["gem"].RecommendTags[0] != "eficiente" {
		t.Fatalf("gem tags = %v, want single string accepted as one tag", fake.rows["gem"].RecommendTags)
	}
	if fake.rows["gem"].Metadata["costo"] != "bajo" {
		t.Fatalf("gem metadata = %+v", fake.rows["gem"].Metadata)
	}

	// The view reflects badges on the static catalog.
	w = httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/models/master", nil)
	getReq.Header.Set("X-Test-Role", "0")
	r.ServeHTTP(w, getReq)
	var view struct {
		Models []Model `json:"models"`
	}
	// Response envelope is {data, success, message}.
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(env.Data, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Models) == 0 {
		t.Fatal("master view must list the static catalog")
	}
}

func TestStringListAcceptsOneOrMany(t *testing.T) {
	var single StringList
	if err := json.Unmarshal([]byte(`"eficiente"`), &single); err != nil {
		t.Fatal(err)
	}
	if len(single) != 1 || single[0] != "eficiente" {
		t.Fatalf("single = %v", single)
	}
	var multi StringList
	if err := json.Unmarshal([]byte(`["a", " a ", "", "A"]`), &multi); err != nil {
		t.Fatal(err)
	}
	if len(multi) != 1 || multi[0] != "a" {
		t.Fatalf("multi = %v, want trimmed deduped [a]", multi)
	}
	var nul StringList = StringList{"x"}
	if err := json.Unmarshal([]byte(`null`), &nul); err != nil || len(nul) != 0 {
		t.Fatalf("null = %v, err = %v", nul, err)
	}
	var bad StringList
	if err := json.Unmarshal([]byte(`42`), &bad); err == nil {
		t.Fatal("number must be rejected")
	}
}

func TestStringListScanArrayLiteral(t *testing.T) {
	var s StringList
	if err := s.Scan(`{eficiente,"bajo, estable",eficiente}`); err != nil {
		t.Fatal(err)
	}
	if len(s) != 2 || s[0] != "eficiente" || s[1] != "bajo, estable" {
		t.Fatalf("scanned = %v", []string(s))
	}
	var empty StringList = StringList{"x"}
	if err := empty.Scan(`{}`); err != nil || len(empty) != 0 {
		t.Fatalf("empty = %v, err = %v", []string(empty), err)
	}
	var native StringList
	if err := native.Scan([]string{"a", " a "}); err != nil || len(native) != 1 {
		t.Fatalf("native = %v, err = %v", []string(native), err)
	}
	var bad StringList
	if err := bad.Scan(`nope`); err == nil {
		t.Fatal("bare literal must be rejected")
	}
}

func TestNormalizeMasterPrefLimits(t *testing.T) {
	good := MasterPref{ModelName: " soul ", Enabled: true, RecommendTags: StringList{"a"}, Metadata: map[string]string{" k ": " v "}}
	if err := NormalizeMasterPref(&good); err != nil {
		t.Fatal(err)
	}
	if good.ModelName != "soul" || good.Metadata["k"] != "v" {
		t.Fatalf("not normalized: %+v", good)
	}
	empty := MasterPref{ModelName: "   "}
	if err := NormalizeMasterPref(&empty); err == nil {
		t.Fatal("blank model_name must be rejected")
	}
	many := MasterPref{ModelName: "m"}
	for i := 0; i <= maxRecommendTags; i++ {
		// Distinct tags: normalization dedupes identical ones.
		many.RecommendTags = append(many.RecommendTags, "tag"+strconv.Itoa(i))
	}
	if err := NormalizeMasterPref(&many); err == nil {
		t.Fatal("too many tags must be rejected")
	}
	long := MasterPref{ModelName: "m", Metadata: map[string]string{"k": strings.Repeat("v", maxMetadataValue+1)}}
	if err := NormalizeMasterPref(&long); err == nil {
		t.Fatal("oversized metadata value must be rejected")
	}
}

type fakeLister struct{}

func (fakeLister) UsableProviders() (map[string]bool, error) { return map[string]bool{}, nil }

func TestTenantUpsertRoundTrip(t *testing.T) {
	m := NewModule(func(*gin.Context) (CredentialLister, bool) { return fakeLister{}, true }, "")
	fake := &fakeTenantPrefs{rows: map[string]TenantPref{}}
	m.SetTenantPrefsSource(func(*gin.Context) (TenantPrefsAccess, bool) { return fake, true })

	r := testPrefsRouter(m)

	body := `[{"model_name":"gem","enabled":true,"favorite_rank":1},
		{"model_name":"flux","enabled":false,"favorite_rank":0}]`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/models/prefs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT got %d: %s", w.Code, w.Body.String())
	}
	if fake.rows["gem"].FavoriteRank != 1 || fake.rows["flux"].Enabled {
		t.Fatalf("rows = %+v", fake.rows)
	}
}
