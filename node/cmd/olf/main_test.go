package main

import "testing"

func TestFormatVersion(t *testing.T) {
	cases := []struct {
		injected string
		rev      string
		dirty    bool
		want     string
	}{
		{"v0.1.0", "abcdef1234", false, "v0.1.0"},        // injected wins
		{"dev", "abcdef1234567", false, "dev (abcdef1)"}, // short hash
		{"dev", "abcdef1234567", true, "dev (abcdef1, dirty)"},
		{"dev", "", false, "dev"}, // no VCS info
	}
	for _, c := range cases {
		if got := formatVersion(c.injected, c.rev, c.dirty); got != c.want {
			t.Errorf("formatVersion(%q,%q,%v) = %q, want %q", c.injected, c.rev, c.dirty, got, c.want)
		}
	}
}
