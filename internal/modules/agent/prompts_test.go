package agent

import (
	"strings"
	"testing"
)

func TestParseAgentDoc(t *testing.T) {
	t.Parallel()

	md := "# Título\n\n## ROL\nrol aquí\n\n## REGLAS COMUNES\nreglas\n\n" +
		"## MÓDULO DE ACTUACIÓN\n### Fase 1 — verbo\nfase uno\n\n" +
		"## PASO 1 — UNO\npaso uno\n\n## PASO 2 — DOS\npaso dos\n\n" +
		"## PASO 3 — TRES\npaso tres\n\n## LISTA DE CONTROL (revisa)\ncheck\n"
	doc := parseAgentDoc(md)

	checks := []struct {
		name string
		got  string
		want string
	}{
		{"rol", doc.rol, "rol aquí"},
		{"reglas", doc.reglas, "reglas"},
		{"modulo conserva subsections", doc.modulo, "fase uno"},
		{"paso 1", doc.steps[1], "paso uno"},
		{"paso 2", doc.steps[2], "paso dos"},
		{"paso 3", doc.steps[3], "paso tres"},
		{"checklist", doc.checklist, "check"},
	}
	for _, c := range checks {
		if !strings.Contains(c.got, c.want) {
			t.Errorf("%s: no contiene %q (got %q)", c.name, c.want, c.got)
		}
	}
	// El encabezado viaja con su sección.
	if !strings.Contains(doc.steps[1], "## PASO 1 — UNO") {
		t.Errorf("paso 1 sin su encabezado: %q", doc.steps[1])
	}
	// Una subsection ### no abre sección: su contenido no puede quedar en
	// el paso siguiente.
	if strings.Contains(doc.steps[1], "fase uno") {
		t.Errorf("el contenido del módulo se filtró al paso 1: %q", doc.steps[1])
	}
	// Un encabezado desconocido no rompe el parseo.
	if doc.rol == "" {
		t.Error("rol vacío")
	}
}

func TestSystemForEmbeddedDocs(t *testing.T) {
	t.Parallel()

	workflows := []string{workflowCommercial, workflowRealEstate, workflowCinema}
	for _, wf := range workflows {
		for step := 1; step <= stepCount; step++ {
			system := systemFor(wf, step)
			if system == "" {
				t.Fatalf("%s paso %d: system vacío (¿.md no embebido?)", wf, step)
			}
			for _, want := range []string{"REGLAS COMUNES", "PASO "} {
				if !strings.Contains(system, want) {
					t.Errorf("%s paso %d: sin %q", wf, step, want)
				}
			}
			if !strings.Contains(system, "LISTA DE CONTROL") {
				t.Errorf("%s paso %d: sin LISTA DE CONTROL", wf, step)
			}
		}
	}

	// Comercial y cine tienen módulo de actuación; inmobiliaria no.
	if !strings.Contains(systemFor(workflowCinema, 1), "MÓDULO DE ACTUACIÓN") {
		t.Error("cine sin MÓDULO DE ACTUACIÓN")
	}
	if !strings.Contains(systemFor(workflowCommercial, 1), "MÓDULO DE ACTUACIÓN") {
		t.Error("comercial sin MÓDULO DE ACTUACIÓN")
	}
	if strings.Contains(systemFor(workflowRealEstate, 1), "MÓDULO DE ACTUACIÓN") {
		t.Error("inmobiliaria no debería tener MÓDULO DE ACTUACIÓN")
	}

	// Cada paso de un agente es distinto.
	s1, s2 := systemFor(workflowRealEstate, 1), systemFor(workflowRealEstate, 2)
	if s1 == s2 {
		t.Error("pasos 1 y 2 idénticos en inmobiliaria")
	}

	// Pasos fuera de rango no existen.
	if got := systemFor(workflowCommercial, 0); got != "" {
		t.Errorf("paso 0 = %q, quiero vacío", got)
	}
	if got := systemFor(workflowCommercial, stepCount+1); got != "" {
		t.Errorf("paso 4 = %q, quiero vacío", got)
	}
	// Un workflow desconocido cae al default comercial.
	if got := systemFor("no_existe", 1); got != systemFor(defaultWorkflow, 1) {
		t.Error("workflow desconocido no cae al default comercial")
	}
}
