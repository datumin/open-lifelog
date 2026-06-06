package links

import (
	"errors"
	"strings"
	"testing"
)

var known = []string{"meal", "sleep", "steps", "weight"}

func TestParseSingle(t *testing.T) {
	l, err := Parse("meal:w", known)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !l.Allows("write", "meal") {
		t.Error("should allow write meal")
	}
	if l.Allows("read", "meal") {
		t.Error("should NOT allow read meal")
	}
	if l.Allows("write", "sleep") {
		t.Error("should NOT allow write sleep")
	}
	if l.Capability != "meal:w" {
		t.Errorf("canonical = %q, want meal:w", l.Capability)
	}
}

func TestParseCombined(t *testing.T) {
	l, err := Parse("meal:rw,sleep:r", known)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !l.Allows("read", "meal") || !l.Allows("write", "meal") {
		t.Error("meal rw should allow both")
	}
	if !l.Allows("read", "sleep") || l.Allows("write", "sleep") {
		t.Error("sleep:r should be read-only")
	}
	if !strings.Contains(l.Capability, "meal:rw") || !strings.Contains(l.Capability, "sleep:r") {
		t.Errorf("canonical missing parts: %q", l.Capability)
	}
}

func TestParseWildcardExpands(t *testing.T) {
	l, err := Parse("*:r", known)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, typ := range known {
		if !l.Allows("read", typ) {
			t.Errorf("*:r should allow read %s", typ)
		}
		if l.Allows("write", typ) {
			t.Errorf("*:r should NOT allow write %s", typ)
		}
	}
	// Wildcard also allows reads of unknown types (the link's surface IS *)
	if !l.Allows("read", "x.com.acme.mood") {
		t.Error("*:r should also cover unknown types via the wildcard")
	}
	if l.Capability != "*:r" {
		t.Errorf("canonical = %q, want *:r", l.Capability)
	}
}

func TestParseWildcardCollapsesRedundant(t *testing.T) {
	// meal:r is redundant when *:r already covers it — canonical drops it.
	l, err := Parse("*:r,meal:r", known)
	if err != nil {
		t.Fatal(err)
	}
	if l.Capability != "*:r" {
		t.Errorf("redundant should collapse to *:r, got %q", l.Capability)
	}

	// But meal:w stays because *:r doesn't cover it.
	l, err = Parse("*:r,meal:w", known)
	if err != nil {
		t.Fatal(err)
	}
	if l.Capability != "*:r,meal:w" {
		t.Errorf("expected '*:r,meal:w', got %q", l.Capability)
	}
}

func TestScopesEmitted(t *testing.T) {
	l, _ := Parse("meal:w,sleep:r", known)
	scopes := l.Scopes()
	want := map[string]bool{
		"lifelog:write:meal": true,
		"lifelog:read:sleep": true,
	}
	if len(scopes) != len(want) {
		t.Fatalf("scopes = %v, want 2 (%v)", scopes, want)
	}
	for _, s := range scopes {
		if !want[s] {
			t.Errorf("unexpected scope: %q", s)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{
		"",               // empty
		"meal",           // missing :ops
		"meal:",          // empty ops
		":w",             // empty type
		"meal:x",         // bad op letter
		"meal:rwr",       // too many ops
		"BadType:w",      // uppercase type
		"unknown:w",      // not in knownTypes
		"meal:w,",        // trailing comma
		"meal:w,sleep:",  // bad second segment
		"meal:w sleep:r", // bad separator
	} {
		if _, err := Parse(in, known); !errors.Is(err, ErrInvalidCapability) {
			t.Errorf("Parse(%q) should fail, got err=%v", in, err)
		}
	}
}

func TestParseExtensionType(t *testing.T) {
	ext := append([]string{}, known...)
	ext = append(ext, "x.com.acme.mood")
	l, err := Parse("x.com.acme.mood:rw", ext)
	if err != nil {
		t.Fatalf("extension type should parse: %v", err)
	}
	if !l.Allows("write", "x.com.acme.mood") {
		t.Error("extension type not allowed")
	}
}

func TestOperationOrderInsensitive(t *testing.T) {
	a, _ := Parse("meal:rw", known)
	b, _ := Parse("meal:wr", known)
	if a.Capability != b.Capability {
		t.Errorf("rw and wr should canonicalize the same: %q vs %q", a.Capability, b.Capability)
	}
}
