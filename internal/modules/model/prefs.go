package model

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

// Model curation ("maestro" + tenant overrides).
//
// A platform superadmin curates the shared catalog in the system schema
// (model_master_prefs): disabling a model hides it from every tenant
// listing and blocks generation (tenants cannot re-enable it), recommended
// marks the efficient ones with a badge, and favorite_rank > 0 pins models
// to the front of their modality listing (several favorites allowed,
// ordered by rank). The master only covers known catalog names (curated +
// clouds); worker-downloaded models are dynamic per tenant and regulated
// at tenant level.
//
// Each tenant inherits what the master leaves active and adjusts its own
// rows (tenant schema, model_tenant_prefs): it can disable more models and
// pin its own favorites. The master's veto always wins.

// Caps for master curation payloads (defense against junk rows).
const (
	maxRecommendTags = 16
	maxRecommendTag  = 48
	maxMetadataPairs = 32
	maxMetadataKey   = 64
	maxMetadataValue = 2000
)

// StringList is one string or many: it unmarshals from `"tag"` as well
// as `["a", "b"]` (and null), always normalizing to a clean list.
type StringList []string

// UnmarshalJSON implements json.Unmarshaler.
func (s *StringList) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*s = nil
		return nil
	}
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*s = normalizeTags([]string{single})
		return nil
	}
	var multi []string
	if err := json.Unmarshal(data, &multi); err != nil {
		return fmt.Errorf("expected a string or an array of strings")
	}
	*s = normalizeTags(multi)
	return nil
}

// Scan implements sql.Scanner for Postgres TEXT[] (array literal, bytes or
// a native string slice, depending on the driver).
func (s *StringList) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*s = nil
		return nil
	case []string:
		*s = normalizeTags(v)
		return nil
	case string:
		return s.scanArrayLiteral(v)
	case []byte:
		return s.scanArrayLiteral(string(v))
	default:
		return fmt.Errorf("cannot scan %T into StringList", src)
	}
}

// scanArrayLiteral parses a Postgres `{a,"b,c"}` literal.
func (s *StringList) scanArrayLiteral(literal string) error {
	trimmed := strings.TrimSpace(literal)
	if trimmed == "" || trimmed == "{}" {
		*s = nil
		return nil
	}
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		return fmt.Errorf("invalid array literal %q", literal)
	}
	body := trimmed[1 : len(trimmed)-1]
	var tags []string
	var current strings.Builder
	inQuotes := false
	escaped := false
	for _, r := range body {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case r == '\\':
			if inQuotes {
				escaped = true
			} else {
				current.WriteRune(r)
			}
		case r == '"':
			inQuotes = !inQuotes
		case r == ',' && !inQuotes:
			tags = append(tags, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	tags = append(tags, current.String())
	*s = normalizeTags(tags)
	return nil
}

// normalizeTags trims, drops empties and dedupes (case-insensitive).
func normalizeTags(tags []string) StringList {
	seen := make(map[string]bool, len(tags))
	out := make(StringList, 0, len(tags))
	for _, tag := range tags {
		clean := strings.TrimSpace(tag)
		if clean == "" {
			continue
		}
		key := strings.ToLower(clean)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, clean)
	}
	return out
}

// normalizeMetadata trims keys/values, drops empty keys and keeps the
// last value on duplicates.
func normalizeMetadata(meta map[string]string) map[string]string {
	out := make(map[string]string, len(meta))
	for key, value := range meta {
		clean := strings.TrimSpace(key)
		if clean == "" {
			continue
		}
		out[clean] = strings.TrimSpace(value)
	}
	return out
}

// NormalizeMasterPref cleans a row in place and rejects oversized
// payloads. It never fails on defaults (empty tags/metadata always fit).
func NormalizeMasterPref(p *MasterPref) error {
	p.ModelName = strings.TrimSpace(p.ModelName)
	if p.ModelName == "" {
		return fmt.Errorf("every pref needs a model_name")
	}
	p.RecommendTags = normalizeTags(p.RecommendTags)
	if len(p.RecommendTags) > maxRecommendTags {
		return fmt.Errorf("at most %d recommend_tags allowed", maxRecommendTags)
	}
	for _, tag := range p.RecommendTags {
		if len([]rune(tag)) > maxRecommendTag {
			return fmt.Errorf("recommend_tag %q exceeds %d characters", tag, maxRecommendTag)
		}
	}
	p.Metadata = normalizeMetadata(p.Metadata)
	if len(p.Metadata) > maxMetadataPairs {
		return fmt.Errorf("at most %d metadata pairs allowed", maxMetadataPairs)
	}
	for key, value := range p.Metadata {
		if len([]rune(key)) > maxMetadataKey {
			return fmt.Errorf("metadata key %q exceeds %d characters", key, maxMetadataKey)
		}
		if len([]rune(value)) > maxMetadataValue {
			return fmt.Errorf("metadata value for %q exceeds %d characters", key, maxMetadataValue)
		}
	}
	if p.FavoriteRank < 0 {
		p.FavoriteRank = 0
	}
	return nil
}

// MasterPref is one platform-level curation row.
type MasterPref struct {
	ModelName string `json:"model_name"`
	Enabled   bool   `json:"enabled"`
	// RecommendTags holds one or several recommendation labels; empty
	// means not recommended. Accepts a single string too.
	RecommendTags StringList `json:"recommend_tags"`
	// Metadata holds free key-value pairs (costs, notes, …).
	Metadata     map[string]string `json:"metadata"`
	FavoriteRank int               `json:"favorite_rank"`
}

// IsRecommended reports whether the row carries any recommendation tag.
func (p MasterPref) IsRecommended() bool { return len(p.RecommendTags) > 0 }

// TenantPref is one tenant-level curation row.
type TenantPref struct {
	ModelName    string `json:"model_name"`
	Enabled      bool   `json:"enabled"`
	FavoriteRank int    `json:"favorite_rank"`
}

// MasterPrefsAccess is the platform curation source. The SQL store and
// tests both satisfy it, so handlers stay testable without a database.
type MasterPrefsAccess interface {
	All() (map[string]MasterPref, error)
	Upsert([]MasterPref) error
}

// TenantPrefsAccess is the tenant curation source (same testability deal).
type TenantPrefsAccess interface {
	All() (map[string]TenantPref, error)
	Upsert([]TenantPref) error
}

// TenantPrefsSource resolves the tenant curation source for a request
// (production: a store over the tenant-schema pool; tests: an in-memory
// fake). False means no tenant context, so overrides are skipped.
type TenantPrefsSource func(c *gin.Context) (TenantPrefsAccess, bool)

// MasterPrefsStore persists platform curation in the system schema.
type MasterPrefsStore struct {
	db *sql.DB
}

// NewMasterPrefsStore builds the platform curation store over the system DB.
func NewMasterPrefsStore(db *sql.DB) *MasterPrefsStore { return &MasterPrefsStore{db: db} }

// All returns every platform curation row keyed by lowercase model name.
func (s *MasterPrefsStore) All() (map[string]MasterPref, error) {
	rows, err := s.db.Query(`SELECT model_name, enabled, recommend_tags, favorite_rank, metadata FROM model_master_prefs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]MasterPref{}
	for rows.Next() {
		var p MasterPref
		var rawMeta []byte
		if err := rows.Scan(&p.ModelName, &p.Enabled, &p.RecommendTags, &p.FavoriteRank, &rawMeta); err != nil {
			return nil, err
		}
		p.Metadata = map[string]string{}
		if len(rawMeta) > 0 {
			if err := json.Unmarshal(rawMeta, &p.Metadata); err != nil {
				return nil, fmt.Errorf("decoding metadata for %q: %w", p.ModelName, err)
			}
		}
		out[strings.ToLower(strings.TrimSpace(p.ModelName))] = p
	}
	return out, rows.Err()
}

// Upsert replaces the given rows. Entries back at defaults (enabled, no
// tags, no metadata, rank 0) are deleted so the table only holds real
// decisions.
func (s *MasterPrefsStore) Upsert(prefs []MasterPref) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, p := range prefs {
		if err := NormalizeMasterPref(&p); err != nil {
			return err
		}
		name := strings.TrimSpace(p.ModelName)
		if name == "" {
			continue
		}
		if p.Enabled && len(p.RecommendTags) == 0 && len(p.Metadata) == 0 && p.FavoriteRank <= 0 {
			if _, err := tx.Exec(`DELETE FROM model_master_prefs WHERE model_name = $1`, name); err != nil {
				return err
			}
			continue
		}
		rawMeta, err := json.Marshal(p.Metadata)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
			INSERT INTO model_master_prefs (model_name, enabled, recommend_tags, favorite_rank, metadata, updated_at)
			VALUES ($1, $2, $3, $4, $5::jsonb, NOW())
			ON CONFLICT (model_name) DO UPDATE SET
				enabled = EXCLUDED.enabled,
				recommend_tags = EXCLUDED.recommend_tags,
				favorite_rank = EXCLUDED.favorite_rank,
				metadata = EXCLUDED.metadata,
				updated_at = NOW()`,
			name, p.Enabled, []string(p.RecommendTags), p.FavoriteRank, string(rawMeta)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TenantPrefsStore persists tenant curation in the tenant schema.
type TenantPrefsStore struct {
	db *sql.DB
}

// NewTenantPrefsStore builds the tenant curation store over the tenant pool.
func NewTenantPrefsStore(db *sql.DB) *TenantPrefsStore { return &TenantPrefsStore{db: db} }

// All returns every tenant curation row keyed by lowercase model name.
func (s *TenantPrefsStore) All() (map[string]TenantPref, error) {
	rows, err := s.db.Query(`SELECT model_name, enabled, favorite_rank FROM model_tenant_prefs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]TenantPref{}
	for rows.Next() {
		var p TenantPref
		if err := rows.Scan(&p.ModelName, &p.Enabled, &p.FavoriteRank); err != nil {
			return nil, err
		}
		out[strings.ToLower(strings.TrimSpace(p.ModelName))] = p
	}
	return out, rows.Err()
}

// Upsert replaces the given rows; default rows are deleted (see master).
func (s *TenantPrefsStore) Upsert(prefs []TenantPref) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, p := range prefs {
		name := strings.TrimSpace(p.ModelName)
		if name == "" {
			continue
		}
		if p.Enabled && p.FavoriteRank <= 0 {
			if _, err := tx.Exec(`DELETE FROM model_tenant_prefs WHERE model_name = $1`, name); err != nil {
				return err
			}
			continue
		}
		if p.FavoriteRank < 0 {
			p.FavoriteRank = 0
		}
		if _, err := tx.Exec(`
			INSERT INTO model_tenant_prefs (model_name, enabled, favorite_rank, updated_at)
			VALUES ($1, $2, $3, NOW())
			ON CONFLICT (model_name) DO UPDATE SET
				enabled = EXCLUDED.enabled,
				favorite_rank = EXCLUDED.favorite_rank,
				updated_at = NOW()`,
			name, p.Enabled, p.FavoriteRank); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CheckAllowed reports whether a model name may be listed/generated under
// both curation levels. Master veto wins: a platform-disabled model stays
// off even when the tenant re-enables it.
func CheckAllowed(name string, master map[string]MasterPref, tenant map[string]TenantPref) error {
	key := strings.ToLower(strings.TrimSpace(name))
	if mp, ok := master[key]; ok && !mp.Enabled {
		return fmt.Errorf("model %q is disabled by the platform catalog", name)
	}
	if tp, ok := tenant[key]; ok && !tp.Enabled {
		return fmt.Errorf("model %q is disabled for this tenant", name)
	}
	return nil
}

// ApplyPrefs filters a listing through both curation levels, stamps the
// Recommended/Favorite badges (plus the recommendation tags), and pins
// favorites to the front of the listing: the tenant's favorites first
// (by rank), then the master's (by rank), then everything else keeping
// its incoming relative order (each source already sorts its own way).
func ApplyPrefs(models []Model, master map[string]MasterPref, tenant map[string]TenantPref) []Model {
	out := make([]Model, 0, len(models))
	tenantRanks := make(map[string]int, len(models))
	masterRanks := make(map[string]int, len(models))
	for _, m := range models {
		key := strings.ToLower(strings.TrimSpace(m.Name))
		mp, hasMaster := master[key]
		tp, hasTenant := tenant[key]
		if hasMaster && !mp.Enabled {
			continue // master veto: hidden everywhere
		}
		if hasTenant && !tp.Enabled {
			continue
		}
		if hasMaster && len(mp.RecommendTags) > 0 {
			m.Recommended = true
			m.RecommendTags = append([]string(nil), mp.RecommendTags...)
		}
		if hasMaster && mp.FavoriteRank > 0 {
			masterRanks[key] = mp.FavoriteRank
		}
		if hasTenant && tp.FavoriteRank > 0 {
			tenantRanks[key] = tp.FavoriteRank
		}
		if tenantRanks[key] > 0 || masterRanks[key] > 0 {
			m.Favorite = true
		}
		out = append(out, m)
	}
	rankOf := func(name string) (tenantFav bool, rank int) {
		key := strings.ToLower(strings.TrimSpace(name))
		if r := tenantRanks[key]; r > 0 {
			return true, r
		}
		return false, masterRanks[key]
	}
	sort.SliceStable(out, func(i, j int) bool {
		ti, ri := rankOf(out[i].Name)
		tj, rj := rankOf(out[j].Name)
		if (ri > 0) != (rj > 0) {
			return ri > 0
		}
		if ri == 0 {
			return false // keep incoming order otherwise
		}
		if ti != tj {
			return ti // tenant favorites before master favorites
		}
		return ri < rj
	})
	return out
}

// prefsOrEmpty loads a pref map, failing open with a loud log: curation is
// not the security boundary (credentials are), so a prefs outage degrades
// to the unfiltered catalog instead of breaking every listing.
func prefsOrEmpty[T any](label string, load func() (map[string]T, error)) map[string]T {
	got, err := load()
	if err != nil {
		log.Printf("[model] %s prefs unavailable, serving unfiltered: %v", label, err)
		return map[string]T{}
	}
	if got == nil {
		return map[string]T{}
	}
	return got
}
