package skills

import (
	"strings"
	"testing"
)

func TestActingEngineSection(t *testing.T) {
	engine, err := ActingEngine()
	if err != nil {
		t.Fatalf("ActingEngine() error = %v", err)
	}
	for _, want := range []string{
		"ACTING & ANATOMY ENGINE",
		"Weston step",
		"Mamet step",
		"Micro-fidgeting",
		"Positive locks",
	} {
		if !strings.Contains(engine, want) {
			t.Errorf("ActingEngine() no contiene %q", want)
		}
	}
	if strings.Contains(engine, "OUTPUT FORMAT (LOCKED)") {
		t.Error("ActingEngine() no debe incluir el contrato de salida de recreacion")
	}
	if strings.Contains(engine, "name: DCS-RECREATE") {
		t.Error("ActingEngine() no debe incluir el frontmatter")
	}
}
