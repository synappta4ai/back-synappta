package looks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedDocumentMatchesPromptOriginal(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Prompt", "looks_templates.md"))
	if err != nil {
		t.Fatalf("no se pudo leer internal/Prompt/looks_templates.md: %v", err)
	}
	if string(raw) != rawTemplates {
		t.Errorf("internal/Prompt/looks_templates.md y looks/data/looks_templates.md están desincronizados")
	}
}

func TestParseCatalog(t *testing.T) {
	catalog, err := Default()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := len(catalog.Order), 62; got != want {
		t.Fatalf("looks = %d, se esperaban %d", got, want)
	}
	for _, level := range []Level{LevelSubtle, LevelMedium, LevelStrong} {
		if strings.TrimSpace(catalog.Intensity[level]) == "" {
			t.Errorf("falta el bloque de intensidad %q", level)
		}
	}
	for _, id := range []string{"clamshell", "three-point", "bloom-halation", "blacklight-uv", "mixed-colour-temperature"} {
		if _, ok := catalog.Looks[id]; !ok {
			t.Errorf("falta el look %q", id)
		}
	}
	for _, id := range []string{"high-key", "low-key", "rembrandt", "monochrome", "rim-backlight", "screen-glow", "teal-and-orange"} {
		look, ok := catalog.Looks[id]
		if !ok {
			t.Errorf("falta el look del Set 2 %q", id)
			continue
		}
		if look.Set != 2 {
			t.Errorf("%q debería ser del Set 2, es %d", id, look.Set)
		}
	}
	if !catalog.Looks["clamshell"].Draft || !catalog.Looks["bloom-halation"].Draft {
		t.Errorf("clamshell y bloom-halation deben estar en borrador")
	}
	if !catalog.Looks["rim-backlight"].Draft || !catalog.Looks["bounce"].Draft || !catalog.Looks["monochrome"].Draft {
		t.Errorf("rim-backlight, bounce y monochrome deben estar en borrador")
	}
	if catalog.Looks["three-point"].Draft {
		t.Errorf("three-point no es borrador")
	}
	if catalog.Looks["three-point"].Set != 1 {
		t.Errorf("three-point debería ser del Set 1")
	}
	if catalog.BaseVideo == "" || catalog.BaseImage == "" || catalog.BaseShort == "" {
		t.Errorf("faltan plantillas base")
	}
}

func TestAssembleVideoWithReference(t *testing.T) {
	catalog, err := Default()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	prompt, err := catalog.Assemble(ModeVideo, "three-point", LevelMedium, "@video1", "@image2")
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	for _, want := range []string{
		"Create a cinematic video in one continuous shot, the same length as @video1.",
		"@image2 (only if provided) = a visual hint of the look (Three-point)",
		"INTENSITY: MEDIUM.",
		"LOOK - THREE-POINT.",
		"{SOURCE}",
	} {
		switch want {
		case "{SOURCE}":
			if strings.Contains(prompt, want) {
				t.Errorf("quedó %q sin reemplazar", want)
			}
		default:
			if !strings.Contains(prompt, want) {
				t.Errorf("falta %q en el prompt", want)
			}
		}
	}
}

func TestAssembleImageWithoutReferenceRemovesLine(t *testing.T) {
	catalog, err := Default()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	prompt, err := catalog.Assemble(ModeImage, "ring-light", "", "", "")
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if strings.Contains(prompt, "{REF}") || strings.Contains(prompt, "(only if provided)") {
		t.Errorf("la línea de REF debe eliminarse por completo")
	}
	if strings.Contains(prompt, "\n\n\n") {
		t.Errorf("la eliminación de REF dejó líneas vacías")
	}
	if !strings.Contains(prompt, "Re-light and re-grade @imagen1 with the look \"Ring light\".") {
		t.Errorf("la fuente por defecto de imagen debe ser @imagen1")
	}
	if !strings.Contains(prompt, "INTENSITY: MEDIUM.") {
		t.Errorf("la intensidad por defecto debe ser media")
	}
}

func TestAssembleShortSetWithoutReference(t *testing.T) {
	catalog, err := Default()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	prompt, err := catalog.Assemble(ModeImage, "high-key", LevelStrong, "@image1", "")
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if strings.Contains(prompt, "{REF}") || strings.Contains(prompt, "is a reference for lighting") {
		t.Errorf("la cláusula REF del Set 2 debe eliminarse")
	}
	if !strings.Contains(prompt, "LIGHTING & COLOUR CHANGE ONLY - High key.") {
		t.Errorf("falta el encabezado del Set 2")
	}
	if !strings.Contains(prompt, "REFERENCES: @image1 is my original") {
		t.Errorf("el Set 2 debe conservar el REFERENCES con la fuente")
	}
	if !strings.Contains(prompt, "INTENSITY: STRONG.") {
		t.Errorf("falta la intensidad fuerte")
	}
	if !strings.Contains(prompt, "LOOK - HIGH KEY:") {
		t.Errorf("falta el bloque del look High key")
	}
	if strings.Contains(prompt, "Re-light and re-grade") {
		t.Errorf("High key pertenece al Set 2 y no debe usar la plantilla del Set 1")
	}
}

func TestAssembleShortSetWithReference(t *testing.T) {
	prompt, err := Assemble(ModeVideo, "rim-backlight", LevelSubtle, "@video1", "@image2")
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if !strings.Contains(prompt, "@image2 (only if provided) is a reference for lighting") {
		t.Errorf("la referencia del Set 2 no se sustituyó")
	}
	if !strings.Contains(prompt, "References: @video1") && !strings.Contains(prompt, "REFERENCES: @video1") {
		t.Errorf("la fuente del Set 2 no se sustituyó")
	}
}

func TestAssembleRejectsUnknownLook(t *testing.T) {
	if _, err := Assemble(ModeImage, "no-existe", LevelMedium, "", ""); err == nil {
		t.Fatal("se esperaba error para un look desconocido")
	}
}

func TestNormalizeLevel(t *testing.T) {
	cases := map[string]Level{
		"sutil":  LevelSubtle,
		"SUBTLE": LevelSubtle,
		"media":  LevelMedium,
		"":       LevelMedium,
		"fuerte": LevelStrong,
		"otro":   LevelMedium,
	}
	for input, want := range cases {
		if got := NormalizeLevel(input); got != want {
			t.Errorf("NormalizeLevel(%q) = %q, se esperaba %q", input, got, want)
		}
	}
}
