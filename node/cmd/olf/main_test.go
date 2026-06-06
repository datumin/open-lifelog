package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionSubcommand(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "olf")
	build := exec.Command("go", "build",
		"-ldflags", "-X main.version=test-9.9.9",
		"-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "test-9.9.9" {
		t.Errorf("olf version = %q, want %q", got, "test-9.9.9")
	}
}

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
