// Package skills embeds the shared Synappta skills that more than one agent
// needs at runtime. The acting engine is the only section the writing copilot
// and the agency prompt steps borrow from the infoproduct acting skill.
package skills

import (
	_ "embed"
	"errors"
	"strings"
)

//go:embed "Synappta Skill actuacion Infoproducto.md"
var actingRaw string

// actingBody strips the UTF-8 BOM, normalises CRLF and removes the YAML
// frontmatter. It does not validate the skill name: this package exposes a
// section, not the whole skill contract.
func actingBody() (string, error) {
	raw := strings.TrimSpace(strings.ReplaceAll(strings.TrimPrefix(actingRaw, "\ufeff"), "\r\n", "\n"))
	if raw == "" {
		return "", errors.New("el skill de actuacion esta vacio")
	}
	if strings.HasPrefix(raw, "---\n") || raw == "---" {
		lines := strings.Split(raw, "\n")
		end := -1
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				end = i
				break
			}
		}
		if end < 0 {
			return "", errors.New("el frontmatter del skill de actuacion no tiene cierre")
		}
		raw = strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
	}
	if raw == "" {
		return "", errors.New("el skill de actuacion quedo vacio tras quitar el frontmatter")
	}
	return raw, nil
}

// ActingEngine returns section 6 (ACTING & ANATOMY ENGINE) of the infoproduct
// acting skill: the performance engine (Weston/Mamet, anatomical dictionary,
// micro-fidgeting and positive locks) without the recreation output contract.
// It returns an error if the section cannot be found.
func ActingEngine() (string, error) {
	body, err := actingBody()
	if err != nil {
		return "", err
	}
	start := strings.Index(body, "## 6.")
	if start < 0 {
		return "", errors.New("el skill de actuacion no tiene la seccion 6")
	}
	rest := body[start:]
	end := strings.Index(rest, "\n## 7.")
	if end < 0 {
		return "", errors.New("la seccion 6 del skill de actuacion no tiene cierre")
	}
	section := strings.TrimSpace(rest[:end])
	if section == "" {
		return "", errors.New("la seccion 6 del skill de actuacion esta vacia")
	}
	return section, nil
}
