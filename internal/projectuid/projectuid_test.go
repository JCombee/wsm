package projectuid

import "testing"

func TestValid(t *testing.T) {
	tests := []struct {
		uid  int
		want bool
	}{
		{Min, true},
		{Max, true},
		{Min - 1, false},
		{Max + 1, false},
		{0, false},
		{-Min, false},
	}
	for _, tt := range tests {
		if got := Valid(tt.uid); got != tt.want {
			t.Errorf("Valid(%d) = %v, want %v", tt.uid, got, tt.want)
		}
	}
}
