package agency

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// buildTestPDF genera un PDF de una página con una capa de texto, calculando
// los offsets de la tabla xref (requeridos por el parser) al concatenar.
func buildTestPDF(t *testing.T, text string) []byte {
	t.Helper()

	content := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs)+1)
	for i, o := range objs {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xrefStart := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objs)+1)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer << /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xrefStart)
	return buf.Bytes()
}

// buildTestDOCX genera un DOCX mínimo (zip con word/document.xml) con los
// párrafos indicados.
func buildTestDOCX(t *testing.T, paragraphs ...string) []byte {
	t.Helper()

	var body strings.Builder
	for _, p := range paragraphs {
		body.WriteString("<w:p><w:r><w:t>" + p + "</w:t></w:r></w:p>")
	}
	docXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:body>` + body.String() + `</w:body></w:document>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("crear entrada zip: %v", err)
	}
	if _, err := f.Write([]byte(docXML)); err != nil {
		t.Fatalf("escribir document.xml: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("cerrar zip: %v", err)
	}
	return buf.Bytes()
}

func TestExtractBriefText(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		data     []byte
		want     string
		wantErr  string
	}{
		{
			name:     "txt plano",
			filename: "folleto.txt",
			data:     []byte("Amenidades: alberca, gimnasio.\nTarget: familias."),
			want:     "Amenidades: alberca, gimnasio.\nTarget: familias.",
		},
		{
			name:     "md plano",
			filename: "notas.md",
			data:     []byte("# Proyecto\nPrecio promedio $2.5M"),
			want:     "# Proyecto\nPrecio promedio $2.5M",
		},
		{
			name:     "pdf con capa de texto",
			filename: "folleto.pdf",
			data:     buildTestPDF(t, "Hello Synapta"),
			want:     "Hello Synapta",
		},
		{
			name:     "docx con varios párrafos",
			filename: "folleto.docx",
			data:     buildTestDOCX(t, "Torre A de 24 niveles", "Escritorios amueblados"),
			want:     "Torre A de 24 niveles\nEscritorios amueblados",
		},
		{
			name:     "extensión en mayúsculas",
			filename: "FOLLETO.TXT",
			data:     []byte("contenido"),
			want:     "contenido",
		},
		{
			name:     "formato no soportado",
			filename: "folleto.doc",
			data:     []byte("binario"),
			wantErr:  "formato no soportado",
		},
		{
			name:     "archivo vacío",
			filename: "vacio.txt",
			data:     []byte("   \n  "),
			wantErr:  "archivo vacío",
		},
		{
			name:     "pdf corrupto",
			filename: "roto.pdf",
			data:     []byte("esto no es un pdf"),
			wantErr:  "PDF ilegible",
		},
		{
			name:     "docx corrupto",
			filename: "roto.docx",
			data:     []byte("esto no es un zip"),
			wantErr:  "DOCX ilegible",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractBriefText(tt.filename, tt.data)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("extractBriefText() sin error, quería %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, quería que contenga %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractBriefText() error inesperado: %v", err)
			}
			if got != tt.want {
				t.Errorf("extractBriefText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeBriefText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "colapsa líneas vacías repetidas y limpia bordes",
			in:   "\n\nHola  \n\n\n\nMundo\n\n",
			want: "Hola\n\nMundo",
		},
		{
			name: "convierte CRLF y CR",
			in:   "a\r\nb\rc",
			want: "a\nb\nc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeBriefText(tt.in); got != tt.want {
				t.Errorf("normalizeBriefText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		max         int
		want        string
		wantTrunced bool
	}{
		{name: "corto pasa intacto", in: "hola", max: 10, want: "hola", wantTrunced: false},
		{name: "multibyte no se corta a la mitad", in: "ñññññ", max: 3, want: "ñññ", wantTrunced: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, truncated := truncateRunes(tt.in, tt.max)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			if truncated != tt.wantTrunced {
				t.Errorf("truncated = %v, want %v", truncated, tt.wantTrunced)
			}
		})
	}
}
