package agency

import "testing"

func TestIsNonPublicURL(t *testing.T) {
	cases := map[string]bool{
		"http://localhost:8099/api/v1/t/x/files/1/serve": true,
		"http://127.0.0.1:8099/a":                        true,
		"http://192.168.1.5/a":                           true,
		"https://api.example.com/a.jpg":                  false,
		"https://203.0.113.9/a.jpg":                      false,
		"asset://abc":                                    false,
	}
	for in, want := range cases {
		if got := isNonPublicURL(in); got != want {
			t.Errorf("isNonPublicURL(%q)=%v want %v", in, got, want)
		}
	}
}

func TestRequirePublicInputs(t *testing.T) {
	err := requirePublicInputs([]ContentItem{
		{Type: "text", Text: "hola"},
		{Type: "image", Name: "a.png", DataURL: "http://localhost:8099/x"},
	})
	if err == nil {
		t.Fatal("expected error for loopback input")
	}
	if err := requirePublicInputs([]ContentItem{{Type: "image", DataURL: "https://cdn.example.com/a.png"}}); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}
