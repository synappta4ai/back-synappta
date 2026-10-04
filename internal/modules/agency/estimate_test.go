package agency

import "testing"

func TestHiggsfieldNumber(t *testing.T) {
	cases := []struct {
		in   interface{}
		want float64
	}{
		{"1.500", 1.5},
		{"0.094", 0.094},
		{float64(2), 2},
		{nil, 0},
		{"", 0},
		{"abc", 0},
		{true, 0},
	}
	for _, c := range cases {
		if got := HiggsfieldNumber(c.in); got != c.want {
			t.Fatalf("HiggsfieldNumber(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
