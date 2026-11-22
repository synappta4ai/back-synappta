// Package looks loads the look templates (light and colour schemes) and
// assembles the final prompt block used by image and video generation.
//
// The canonical document is internal/Prompt/looks_templates.md; this package
// embeds a byte-identical copy (go:embed cannot cross package directories) and
// looks_test.go keeps both in sync.
package looks

import (
	_ "embed"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"unicode"
)

//go:embed data/looks_templates.md
var rawTemplates string

// Mode selects the base template (video or image).
type Mode string

const (
	ModeVideo Mode = "video"
	ModeImage Mode = "image"
)

// Level is the look intensity.
type Level string

const (
	LevelSubtle Level = "subtle"
	LevelMedium Level = "medium"
	LevelStrong Level = "strong"
)

// Look is a single scheme parsed from the document.
type Look struct {
	ID    string
	Name  string
	Block string
	Draft bool
	// Set is 1 (long format, rich transferable/conditional blocks) or 2
	// (short format, description + GUARD).
	Set int
}

// Catalog is the parsed looks document.
type Catalog struct {
	// Set 1 has its own video/image base templates; Set 2 shares one.
	BaseVideo string
	BaseImage string
	BaseShort string
	Intensity map[Level]string
	Looks     map[string]Look
	Order     []string
}

var placeholder = regexp.MustCompile(`\{[A-Z][A-Z0-9_]*\}`)

var defaultCatalog = sync.OnceValues(func() (*Catalog, error) {
	return Parse(rawTemplates)
})

// Default returns the embedded catalog.
func Default() (*Catalog, error) {
	return defaultCatalog()
}

// NormalizeLevel maps accepted spellings to a Level. Unknown or empty values
// fall back to medium, the documented default.
func NormalizeLevel(value string) Level {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "subtle", "sutil", "low", "bajo":
		return LevelSubtle
	case "strong", "fuerte", "high", "alto":
		return LevelStrong
	case "medium", "media", "", "default":
		return LevelMedium
	}
	return LevelMedium
}

// NormalizeMode maps accepted spellings to a Mode; empty defaults to image.
func NormalizeMode(value string) Mode {
	if strings.EqualFold(strings.TrimSpace(value), "video") {
		return ModeVideo
	}
	return ModeImage
}

// Parse builds a Catalog from the markdown document.
func Parse(raw string) (*Catalog, error) {
	body := normalize(raw)
	lines := strings.Split(body, "\n")

	catalog := &Catalog{
		Intensity: make(map[Level]string),
		Looks:     make(map[string]Look),
	}

	// Intensity blocks are shared by both sets.
	if istart, iend := sectionPrefix(lines, 0, len(lines), "## Intensidad"); istart >= 0 {
		for i := istart; i < iend; i++ {
			level, ok := intensityLevelOf(lines[i])
			if !ok {
				continue
			}
			block, _, found := fenceInRange(lines, i+1, iend)
			if !found {
				continue
			}
			catalog.Intensity[level] = block
		}
	}
	if len(catalog.Intensity) != 3 {
		return nil, fmt.Errorf("looks: se esperaban 3 bloques de intensidad, se encontraron %d", len(catalog.Intensity))
	}

	s1start, s1end := topSection(lines, "# SET 1")
	if s1start < 0 {
		return nil, errors.New("looks: no se encontró la sección # SET 1")
	}
	if vstart, vend := sectionPrefix(lines, s1start, s1end, "## Plantilla base - Video"); vstart >= 0 {
		if block, _, ok := fenceInRange(lines, vstart, vend); ok {
			catalog.BaseVideo = block
		}
	}
	if istart, iend := sectionPrefix(lines, s1start, s1end, "## Plantilla base - Imagen"); istart >= 0 {
		if block, _, ok := fenceInRange(lines, istart, iend); ok {
			catalog.BaseImage = block
		}
	}

	s2start, s2end := topSection(lines, "# SET 2")
	if s2start < 0 {
		return nil, errors.New("looks: no se encontró la sección # SET 2")
	}
	if bstart, bend := exactSection(lines, s2start, s2end, "## Plantilla base"); bstart >= 0 {
		if block, _, ok := fenceInRange(lines, bstart, bend); ok {
			catalog.BaseShort = block
		}
	}

	if catalog.BaseVideo == "" || catalog.BaseImage == "" {
		return nil, errors.New("looks: no se encontraron las plantillas base del Set 1")
	}
	if catalog.BaseShort == "" {
		return nil, errors.New("looks: no se encontró la plantilla base del Set 2")
	}

	if err := catalog.parseLooks(lines, 1, "## Looks del Set 1", s1start, s1end); err != nil {
		return nil, err
	}
	if err := catalog.parseLooks(lines, 2, "## Looks del Set 2", s2start, s2end); err != nil {
		return nil, err
	}
	if len(catalog.Looks) == 0 {
		return nil, errors.New("looks: no se encontró ningún look")
	}

	return catalog, nil
}

func (c *Catalog) parseLooks(lines []string, set int, heading string, from, to int) error {
	start, end := exactSection(lines, from, to, heading)
	if start < 0 {
		return fmt.Errorf("looks: no se encontró la sección %q", heading)
	}
	for i := start; i < end; i++ {
		if !strings.HasPrefix(lines[i], "### ") {
			continue
		}
		name, draft := parseLookTitle(strings.TrimSpace(strings.TrimPrefix(lines[i], "### ")))
		if name == "" {
			continue
		}
		block, _, found := fenceInRange(lines, i+1, end)
		if !found {
			return fmt.Errorf("looks: el look %q no tiene bloque", name)
		}
		look := Look{ID: Slug(name), Name: name, Block: block, Draft: draft, Set: set}
		if _, exists := c.Looks[look.ID]; exists {
			return fmt.Errorf("looks: id duplicado %q", look.ID)
		}
		c.Looks[look.ID] = look
		c.Order = append(c.Order, look.ID)
	}
	return nil
}

// Assemble renders the base template for mode, substituting the look, the
// intensity block, the source tag and the optional reference tag. When ref is
// empty the {REF} clause is removed completely (whole line in Set 1, inline
// sentence in Set 2).
func (c *Catalog) Assemble(mode Mode, lookRef string, level Level, source, ref string) (string, error) {
	look, ok := c.Lookup(lookRef)
	if !ok {
		return "", fmt.Errorf("looks: look desconocido %q", lookRef)
	}
	intensity, ok := c.Intensity[NormalizeLevel(string(level))]
	if !ok {
		return "", fmt.Errorf("looks: intensidad desconocida %q", level)
	}

	base := c.BaseShort
	defaultSource := "@imagen1"
	if mode == ModeVideo {
		defaultSource = "@video1"
	}
	if look.Set == 1 {
		if mode == ModeVideo {
			base = c.BaseVideo
			defaultSource = "@video1"
		} else {
			base = c.BaseImage
			defaultSource = "@imagen1"
		}
	}
	if strings.TrimSpace(source) == "" {
		source = defaultSource
	}

	out := base
	out = strings.ReplaceAll(out, "{LOOK_NAME}", look.Name)
	out = strings.ReplaceAll(out, "{LOOK_BLOCK}", strings.TrimSpace(look.Block))
	out = strings.ReplaceAll(out, "{INTENSITY_BLOCK}", strings.TrimSpace(intensity))
	out = strings.ReplaceAll(out, "{SOURCE}", source)
	out = strings.ReplaceAll(out, "{SUBJECT}", "the subject")

	if strings.TrimSpace(ref) == "" {
		out = removeRef(out)
	} else {
		out = strings.ReplaceAll(out, "{REF}", ref)
	}

	out = collapseSpaces(out)
	out = collapseBlankRuns(out)
	if missing := placeholder.FindAllString(out, -1); len(missing) > 0 {
		msg := fmt.Sprintf("looks: quedaron variables sin reemplazar en el prompt final: %s", strings.Join(missing, ", "))
		log.Printf("[looks] ERROR %s", msg)
		return "", errors.New(msg)
	}
	return strings.TrimSpace(out), nil
}

// Assemble is a convenience wrapper around the embedded catalog.
func Assemble(mode Mode, lookRef string, level Level, source, ref string) (string, error) {
	catalog, err := Default()
	if err != nil {
		return "", err
	}
	return catalog.Assemble(mode, lookRef, level, source, ref)
}

// Lookup resolves a look by slug or by name (case-insensitive).
func (c *Catalog) Lookup(value string) (Look, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Look{}, false
	}
	if look, ok := c.Looks[Slug(value)]; ok {
		return look, true
	}
	for _, look := range c.Looks {
		if strings.EqualFold(look.Name, value) || strings.EqualFold(look.ID, value) {
			return look, true
		}
	}
	return Look{}, false
}

// Names returns the look names in document order.
func (c *Catalog) Names() []string {
	names := make([]string, 0, len(c.Order))
	for _, id := range c.Order {
		names = append(names, c.Looks[id].Name)
	}
	return names
}

// Slug converts a look name into the id used by the app.
func Slug(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "&", " and "))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		r = foldAccent(unicode.ToLower(r))
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func foldAccent(r rune) rune {
	switch r {
	case 'á', 'à', 'ä', 'â', 'ã':
		return 'a'
	case 'é', 'è', 'ë', 'ê':
		return 'e'
	case 'í', 'ì', 'ï', 'î':
		return 'i'
	case 'ó', 'ò', 'ö', 'ô', 'õ':
		return 'o'
	case 'ú', 'ù', 'ü', 'û':
		return 'u'
	case 'ñ':
		return 'n'
	case 'ç':
		return 'c'
	}
	return r
}

func normalize(raw string) string {
	raw = strings.TrimPrefix(raw, "\ufeff")
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	return raw
}

func intensityLevelOf(line string) (Level, bool) {
	upper := strings.ToUpper(line)
	switch {
	case strings.HasPrefix(strings.TrimSpace(upper), "**SUBTLE"), strings.HasPrefix(strings.TrimSpace(upper), "**SUTIL"):
		return LevelSubtle, true
	case strings.HasPrefix(strings.TrimSpace(upper), "**MEDIUM"), strings.HasPrefix(strings.TrimSpace(upper), "**MEDIA"):
		return LevelMedium, true
	case strings.HasPrefix(strings.TrimSpace(upper), "**STRONG"), strings.HasPrefix(strings.TrimSpace(upper), "**FUERTE"):
		return LevelStrong, true
	}
	return "", false
}

func parseLookTitle(title string) (string, bool) {
	draft := false
	if strings.Contains(title, "[BORRADOR]") {
		draft = true
		title = strings.ReplaceAll(title, "[BORRADOR]", "")
	}
	title = strings.TrimSpace(title)
	if dot := strings.Index(title, "."); dot >= 0 {
		prefix := title[:dot]
		if _, ok := parseNumber(prefix); ok {
			title = strings.TrimSpace(title[dot+1:])
		}
	}
	return strings.TrimSpace(title), draft
}

func parseNumber(value string) (int, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	n := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

// topSection returns the lines after a top-level "# ..." heading, up to the
// next top-level heading.
func topSection(lines []string, prefix string) (int, int) {
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return -1, -1
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "# ") {
			end = i
			break
		}
	}
	return start, end
}

// sectionPrefix finds a heading whose trimmed line starts with prefix and
// returns its body, ending at the next heading of any level.
func sectionPrefix(lines []string, from, to int, prefix string) (int, int) {
	start := -1
	for i := from; i < to; i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), prefix) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return -1, -1
	}
	end := to
	for i := start; i < to; i++ {
		if strings.HasPrefix(lines[i], "## ") || strings.HasPrefix(lines[i], "# ") {
			end = i
			break
		}
	}
	return start, end
}

// exactSection is sectionPrefix for an exact heading match.
func exactSection(lines []string, from, to int, heading string) (int, int) {
	start := -1
	for i := from; i < to; i++ {
		if strings.TrimSpace(lines[i]) == heading {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return -1, -1
	}
	end := to
	for i := start; i < to; i++ {
		if strings.HasPrefix(lines[i], "## ") || strings.HasPrefix(lines[i], "# ") {
			end = i
			break
		}
	}
	return start, end
}

func fenceInRange(lines []string, start, end int) (string, int, bool) {
	open := -1
	for i := start; i < end; i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
			open = i
			break
		}
	}
	if open < 0 {
		return "", -1, false
	}
	var body []string
	for i := open + 1; i < end; i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
			return strings.TrimSpace(strings.Join(body, "\n")), i, true
		}
		body = append(body, lines[i])
	}
	return "", -1, false
}

// removeRef drops the optional reference clause. Set 1 keeps {REF} on its own
// line; Set 2 embeds it in the REFERENCES sentence, so only that sentence is
// removed.
func removeRef(text string) string {
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if !strings.Contains(line, "{REF}") {
			kept = append(kept, line)
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "{REF}") {
			continue
		}
		idx := strings.Index(line, "{REF}")
		rest := line[idx:]
		if dot := strings.Index(rest, "."); dot >= 0 {
			line = line[:idx] + rest[dot+1:]
		} else {
			line = line[:idx]
		}
		kept = append(kept, strings.TrimRight(line, " "))
	}
	return strings.Join(kept, "\n")
}

var blankRun = regexp.MustCompile(`\n{3,}`)

func collapseBlankRuns(text string) string {
	return blankRun.ReplaceAllString(text, "\n\n")
}

var spaceRun = regexp.MustCompile(`[ \t]{2,}`)

func collapseSpaces(text string) string {
	return spaceRun.ReplaceAllString(text, " ")
}
