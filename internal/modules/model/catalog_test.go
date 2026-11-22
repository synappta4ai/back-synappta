package model

import "testing"

func TestPreferredImageResolution(t *testing.T) {
	tests := []struct {
		name      string
		supported []string
		want      string
	}{
		{name: "2K wins", supported: []string{"1K", "2K", "4K"}, want: "2K"},
		{name: "2K after lower tiers", supported: []string{"720p", "1080p", "2K"}, want: "2K"},
		{name: "falls back to 1080p", supported: []string{"720p", "1080p"}, want: "1080p"},
		{name: "falls back to 1K", supported: []string{"1K"}, want: "1K"},
		{name: "case insensitive", supported: []string{"2k"}, want: "2k"},
		{name: "no tiers", supported: nil, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PreferredImageResolution(tt.supported); got != tt.want {
				t.Fatalf("PreferredImageResolution(%v) = %q, want %q", tt.supported, got, tt.want)
			}
		})
	}
}
