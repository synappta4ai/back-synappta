package agency

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

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
func pdfText(data []byte) (string, error) {
	r, err := pdfreader.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("PDF ilegible: %w", err)
	}
	if r.NumPage() == 0 {
		return "", errors.New("PDF sin páginas")
	}
	pr, err := r.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("no se pudo extraer el texto del PDF: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(pr, 4<<20))
	if err != nil {
		return "", fmt.Errorf("no se pudo leer el texto del PDF: %w", err)
	}
	return string(raw), nil
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
