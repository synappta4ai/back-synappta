package agent

import (
	"strings"
	"testing"
)

func TestPersonaFor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		workflow   string
		wantName   string
		wantInChat string
	}{
		{name: "commercial", workflow: "commercial", wantName: "Comercial publicitario", wantInChat: "publicitario"},
		{name: "real_estate", workflow: "real_estate", wantName: "Inmobiliaria", wantInChat: "inmobiliario"},
		{name: "cinema", workflow: "cinema", wantName: "Cine", wantInChat: "cine"},
		{name: "slug real-estate", workflow: "real-estate", wantName: "Inmobiliaria", wantInChat: "inmobiliario"},
		{name: "default ante vacío", workflow: "", wantName: "Comercial publicitario", wantInChat: "publicitario"},
		{name: "default ante desconocido", workflow: "otra_cosa", wantName: "Comercial publicitario", wantInChat: "publicitario"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := personaFor(tc.workflow)
			if p.Name != tc.wantName {
				t.Errorf("Name=%q, want %q", p.Name, tc.wantName)
			}
			if !strings.Contains(strings.ToLower(p.ChatSystem), tc.wantInChat) {
				t.Errorf("ChatSystem no menciona %q", tc.wantInChat)
			}
			// Los dos prompts exigen el mismo contrato de artefacto y
			// ninguno queda vacío: un prompt mudo rompe el flujo.
			if !strings.Contains(p.AnglesSystem, `"angles"`) {
				t.Error("AnglesSystem sin el contrato sales_angles")
			}
			if strings.TrimSpace(p.ChatSystem) == "" || strings.TrimSpace(p.AnglesSystem) == "" {
				t.Error("prompt vacío")
			}
		})
	}
}

func TestPersonasDistintas(t *testing.T) {
	t.Parallel()

	seen := map[string]string{}
	for wf, p := range personas {
		if prev, ok := seen[p.ChatSystem]; ok {
			t.Errorf("workflows %q y %q comparten ChatSystem", prev, wf)
		}
		seen[p.ChatSystem] = wf
	}
	if len(personas) != 3 {
		t.Errorf("personas=%d, want 3", len(personas))
	}
	if _, ok := personas[defaultWorkflow]; !ok {
		t.Errorf("defaultWorkflow %q no existe en personas", defaultWorkflow)
	}
}

func TestWorkflowFromSlug(t *testing.T) {
	t.Parallel()

	cases := []struct {
		slug   string
		want   string
		wantOK bool
	}{
		{slug: "commercial", want: "commercial", wantOK: true},
		{slug: "real-estate", want: "real_estate", wantOK: true},
		{slug: "real_estate", want: "real_estate", wantOK: true},
		{slug: "cinema", want: "cinema", wantOK: true},
		{slug: "Cinema", want: "cinema", wantOK: true},
		{slug: "bogus", wantOK: false},
		{slug: "", wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.slug, func(t *testing.T) {
			t.Parallel()
			got, ok := workflowFromSlug(tc.slug)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v, want %v", ok, tc.wantOK)
			}
			if tc.wantOK && got != tc.want {
				t.Errorf("got=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestConvStore(t *testing.T) {
	t.Parallel()

	key := func(conv string) conversationKey {
		return conversationKey{TenantID: 1, UserID: 7, ConvID: conv}
	}

	t.Run("historial por conversación", func(t *testing.T) {
		t.Parallel()
		s := newConvStore()
		s.append(key("a"), chatMsg{Role: "user", Content: "hola"})
		s.append(key("a"), chatMsg{Role: "assistant", Content: "hey"})
		if got := s.history(key("a")); len(got) != 2 {
			t.Fatalf("len=%d, want 2", len(got))
		}
		if got := s.history(key("b")); len(got) != 0 {
			t.Errorf("la conversación b no debería tener historial, len=%d", len(got))
		}
	})

	t.Run("aisla el mismo conversation_id entre tenants y usuarios", func(t *testing.T) {
		t.Parallel()
		s := newConvStore()
		tA := conversationKey{TenantID: 1, UserID: 7, ConvID: "shared"}
		tB := conversationKey{TenantID: 2, UserID: 7, ConvID: "shared"}
		uB := conversationKey{TenantID: 1, UserID: 8, ConvID: "shared"}
		s.append(tA, chatMsg{Role: "user", Content: "dato sensible del tenant 1"})
		if got := s.history(tB); len(got) != 0 {
			t.Errorf("el tenant 2 no debería ver el historial del tenant 1, len=%d", len(got))
		}
		if got := s.history(uB); len(got) != 0 {
			t.Errorf("otro usuario del mismo tenant no debería ver el historial, len=%d", len(got))
		}
	})

	t.Run("recorta a convMaxMsgs", func(t *testing.T) {
		t.Parallel()
		s := newConvStore()
		for i := 0; i < convMaxMsgs+6; i++ {
			s.append(key("a"), chatMsg{Role: "user", Content: "m"})
		}
		if got := s.history(key("a")); len(got) != convMaxMsgs {
			t.Errorf("len=%d, want %d", len(got), convMaxMsgs)
		}
	})

	t.Run("expulsa la conversación más vieja", func(t *testing.T) {
		t.Parallel()
		s := newConvStore()
		for i := 0; i < convMaxConvs+3; i++ {
			s.append(key(string(rune('a'+i%26))+string(rune('a'+i/26))), chatMsg{Role: "user", Content: "m"})
		}
		s.mu.Lock()
		n := len(s.turns)
		s.mu.Unlock()
		if n > convMaxConvs {
			t.Errorf("conversaciones=%d, want <= %d", n, convMaxConvs)
		}
	})
}
