package agency

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	pdfreader "github.com/ledongthuc/pdf"

	"synapta/internal/utils"
)

// ─── Brief extraction (folletos de proyecto) ─────────────────────
//
// El paso 1 del flujo de agencia pide la "Descripción y puntos fuertes" del
// proyecto. Este endpoint permite subir el folleto (PDF/DOCX/TXT/MD) y
// devolver su texto plano para que el front lo appendee en ese campo: la
// información general del proyecto vive en el folleto.

// maxBriefFileBytes limita el folleto subido (20 MiB).
const maxBriefFileBytes = 20 << 20

// maxBriefTextChars acota el texto extraído que alimenta la descripción
// (el campo alimenta prompts LLM; texto más largo no aporta).
const maxBriefTextChars = 8000

// BriefExtraction es la respuesta de POST /agency/brief/extract.
type BriefExtraction struct {
	Filename  string `json:"filename"`
	Text      string `json:"text"`
	Chars     int    `json:"chars"`
	Truncated bool   `json:"truncated"`
}

// ExtractBrief handles POST /agency/brief/extract (multipart: file)
//
// Extrae el texto plano de un folleto. Formatos soportados: .pdf, .docx,
// .txt y .md. Cualquier otro formato o un PDF escaneado (sin capa de texto)
// responde 400 con el motivo.
func (h *Handler) ExtractBrief(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		utils.BadRequest(c, "file field is required")
		return
	}
	if file.Size > maxBriefFileBytes {
		utils.BadRequest(c, "el archivo supera el máximo de 20 MB")
		return
	}
	f, err := file.Open()
	if err != nil {
		utils.InternalError(c, "failed to read file")
		return
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxBriefFileBytes+1))
	if err != nil {
		utils.InternalError(c, "failed to read file data")
		return
	}

	text, err := extractBriefText(file.Filename, data)
	if err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	if text == "" {
		utils.BadRequest(c, "no se pudo extraer texto del archivo (¿está escaneado como imagen?)")
		return
	}
	text, truncated := truncateRunes(text, maxBriefTextChars)

	utils.Success(c, BriefExtraction{
		Filename:  file.Filename,
		Text:      text,
		Chars:     len([]rune(text)),
		Truncated: truncated,
	})
}

// extractBriefText despacha por extensión al extractor correspondiente y
// normaliza el resultado (saltos de línea, líneas vacías repetidas).
func extractBriefText(filename string, data []byte) (string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return "", errors.New("archivo vacío")
	}
	var (
		text string
		err  error
	)
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".pdf":
		text, err = pdfText(data)
	case ".docx":
		text, err = docxText(data)
	case ".txt", ".md":
		text = string(data)
	default:
		return "", fmt.Errorf("formato no soportado %q: usá PDF, DOCX, TXT o MD", ext)
	}
	if err != nil {
		return "", err
	}
	return normalizeBriefText(text), nil
}

// pdfText extrae el texto de un PDF con capa de texto selectable.
//
// Muchos generadores escriben cada palabra (o cada glifo) como un bloque de
// texto independiente, así que leer el content stream en crudo deja una
// palabra por línea. Acá se reconstruyen las líneas y los párrafos a partir de
// las coordenadas de cada glifo para que el texto fluya como se ve en el PDF.
func pdfText(data []byte) (string, error) {
	r, err := pdfreader.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("PDF ilegible: %w", err)
	}
	if r.NumPage() == 0 {
		return "", errors.New("PDF sin páginas")
	}
	pages := make([]string, 0, r.NumPage())
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		text, err := pdfPageText(p)
		if err != nil {
			plain, plainErr := p.GetPlainText(nil)
			if plainErr != nil {
				continue
			}
			text = plain
		}
		if text = strings.TrimSpace(text); text != "" {
			pages = append(pages, text)
		}
	}
	if len(pages) == 0 {
		return "", errors.New("no se pudo extraer el texto del PDF")
	}
	return strings.Join(pages, "\n\n"), nil
}

// pdfGlyph es un fragmento de texto dibujado en una posición del PDF.
type pdfGlyph struct {
	x, y, w, size float64
	text          string
}

// pdfLine es una línea reconstruida (glifos con la misma altura).
type pdfLine struct {
	y    float64
	size float64
	text string
}

func pdfPageText(p pdfreader.Page) (string, error) {
	glyphs, err := pdfPageGlyphs(p)
	if err != nil {
		return "", err
	}
	if len(glyphs) == 0 {
		return "", nil
	}
	return pdfGlyphsText(glyphs), nil
}

// pdfPageGlyphs interpreta la página (content stream) y devuelve cada glifo
// con su posición. Content() completa X/Y/W (a diferencia del texto crudo).
func pdfPageGlyphs(p pdfreader.Page) (glyphs []pdfGlyph, err error) {
	defer func() {
		if r := recover(); r != nil {
			glyphs, err = nil, fmt.Errorf("no se pudo interpretar el PDF: %v", r)
		}
	}()
	for _, t := range p.Content().Text {
		s := strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == '\t' {
				return -1
			}
			return r
		}, t.S)
		if s == "" {
			continue
		}
		glyphs = append(glyphs, pdfGlyph{x: t.X, y: t.Y, w: t.W, size: t.FontSize, text: s})
	}
	return glyphs, nil
}

// pdfGlyphsText agrupa glifos en líneas por altura, separa palabras por el
// espacio horizontal y arma párrafos según el salto vertical entre líneas.
func pdfGlyphsText(glyphs []pdfGlyph) string {
	size := medianSize(glyphs)
	tol := math.Max(1, 0.35*size)

	sort.SliceStable(glyphs, func(i, j int) bool {
		if glyphs[i].y != glyphs[j].y {
			return glyphs[i].y > glyphs[j].y
		}
		return glyphs[i].x < glyphs[j].x
	})

	lines := make([]pdfLine, 0, len(glyphs))
	current := make([]pdfGlyph, 0, 64)
	flush := func() {
		if len(current) == 0 {
			return
		}
		sort.SliceStable(current, func(i, j int) bool { return current[i].x < current[j].x })
		lines = append(lines, pdfLine{
			y:    current[0].y,
			size: medianSize(current),
			text: joinGlyphs(current),
		})
		current = current[:0]
	}
	for _, g := range glyphs {
		if len(current) > 0 && math.Abs(g.y-current[0].y) > tol {
			flush()
		}
		current = append(current, g)
	}
	flush()

	return joinLines(lines)
}

// joinGlyphs concatena los glifos de una línea insertando un espacio cuando
// la separación horizontal supera el ancho de un espacio tipográfico.
func joinGlyphs(glyphs []pdfGlyph) string {
	var b strings.Builder
	endsSpace := false
	for i, g := range glyphs {
		if i > 0 && !endsSpace && !strings.HasPrefix(g.text, " ") {
			prev := glyphs[i-1]
			prevW := prev.w
			if prevW <= 0 {
				prevW = 0.5 * prev.size
			}
			space := 0.2 * math.Max(prev.size, g.size)
			if g.x-(prev.x+prevW) > space {
				b.WriteByte(' ')
				endsSpace = true
			}
		}
		b.WriteString(g.text)
		endsSpace = strings.HasSuffix(g.text, " ")
	}
	return b.String()
}

// joinLines une las líneas de una página: espacio simple dentro de un
// párrafo, línea en blanco cuando el salto vertical es mayor al interlineado
// habitual, y une palabras cortadas con guion al final de línea.
func joinLines(lines []pdfLine) string {
	if len(lines) == 0 {
		return ""
	}
	leading := medianLeading(lines)
	if leading <= 0 {
		leading = math.Max(1, lines[0].size)
	}
	var paragraphs []string
	var cur strings.Builder
	cur.WriteString(lines[0].text)
	for i := 1; i < len(lines); i++ {
		prev, ln := lines[i-1], lines[i]
		gap := prev.y - ln.y
		hyphenated := strings.HasSuffix(prev.text, "-") && startsLower(ln.text)
		switch {
		case gap > 1.65*leading:
			if s := strings.TrimSpace(cur.String()); s != "" {
				paragraphs = append(paragraphs, s)
			}
			cur.Reset()
			cur.WriteString(ln.text)
		case hyphenated:
			cur.Reset()
			cur.WriteString(strings.TrimSuffix(cur.String(), "-"))
			cur.WriteString(ln.text)
		default:
			cur.WriteByte(' ')
			cur.WriteString(ln.text)
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		paragraphs = append(paragraphs, s)
	}
	return strings.Join(paragraphs, "\n\n")
}

// medianLeading devuelve la mediana (inferior) de los saltos verticales
// entre líneas consecutivas; es el interlineado típico de la página.
func medianLeading(lines []pdfLine) float64 {
	gaps := make([]float64, 0, len(lines)-1)
	for i := 1; i < len(lines); i++ {
		if gap := lines[i-1].y - lines[i].y; gap > 0 {
			gaps = append(gaps, gap)
		}
	}
	if len(gaps) == 0 {
		return 0
	}
	sort.Float64s(gaps)
	return gaps[(len(gaps)-1)/2]
}

func medianSize[T interface {
	pdfGlyph | pdfLine
}](items []T) float64 {
	if len(items) == 0 {
		return 0
	}
	sizes := make([]float64, len(items))
	for i, it := range items {
		switch v := any(it).(type) {
		case pdfGlyph:
			sizes[i] = v.size
		case pdfLine:
			sizes[i] = v.size
		}
	}
	sort.Float64s(sizes)
	return sizes[(len(sizes)-1)/2]
}

func startsLower(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsLower(r)
}

// docxText extrae el texto de un DOCX (word/document.xml dentro del zip),
// concatenando los runs <w:t> y separando párrafos con salto de línea.
func docxText(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("DOCX ilegible: %w", err)
	}
	var doc *zip.File
	for _, zf := range zr.File {
		if zf.Name == "word/document.xml" {
			doc = zf
			break
		}
	}
	if doc == nil {
		return "", errors.New("DOCX sin word/document.xml")
	}
	rc, err := doc.Open()
	if err != nil {
		return "", fmt.Errorf("DOCX ilegible: %w", err)
	}
	defer rc.Close()
	return docxXMLText(rc)
}

// docxXMLText recorre el XML del documento y concatena los elementos <w:t>
// (el prefijo de namespace se ignora: se compara solo el nombre local),
// agregando un salto de línea al cerrar cada párrafo <w:p>.
func docxXMLText(r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)
	var b strings.Builder
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("DOCX XML inválido: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "t" {
				var run string
				if err := dec.DecodeElement(&run, &t); err != nil {
					return "", fmt.Errorf("DOCX XML inválido: %w", err)
				}
				b.WriteString(run)
			}
		case xml.EndElement:
			if t.Name.Local == "p" {
				b.WriteByte('\n')
			}
		}
	}
	return b.String(), nil
}

// normalizeBriefText uniformiza saltos de línea y colapsa líneas vacías
// repetidas (el texto de PDF suele venir con ruido de espaciado).
func normalizeBriefText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t")
		if ln == "" && (len(out) == 0 || out[len(out)-1] == "") {
			continue
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// truncateRunes corta en límite de runes (multibyte-safe) y reporta corte.
func truncateRunes(s string, max int) (string, bool) {
	runes := []rune(s)
	if len(runes) <= max {
		return s, false
	}
	return strings.TrimSpace(string(runes[:max])), true
}
