package olf

import (
	"regexp"
	"testing"
	"time"
)

var uuidV7Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewUUIDv7_ShapeAndOrdering(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	id0, err := NewUUIDv7(t0)
	if err != nil {
		t.Fatal(err)
	}
	if !uuidV7Re.MatchString(id0) {
		t.Fatalf("not a v7 uuid: %s", id0)
	}
	// A later timestamp yields a lexicographically-greater id (time-ordered).
	id1, err := NewUUIDv7(t0.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !(id1 > id0) {
		t.Fatalf("later id should sort after earlier: %s !> %s", id1, id0)
	}
}

func TestParseInstant_RejectsOffsetLess(t *testing.T) {
	if _, err := ParseInstant("2026-01-01T00:00:00"); err == nil {
		t.Fatal("offset-less (naive) timestamp must be rejected")
	}
}

func TestParseInstant_OffsetIndependentEquality(t *testing.T) {
	// 2026-01-01T00:00:00+09:00 and 2025-12-31T15:00:00Z denote the same instant.
	a, err := ParseInstant("2026-01-01T00:00:00+09:00")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseInstant("2025-12-31T15:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Equal(b) {
		t.Fatalf("same instant must compare equal regardless of offset: %v != %v", a, b)
	}
}
