package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"open-lifelog.org/node/internal/olf"
)

func rec(id, payload string) olf.Record {
	return recOn(id, payload, "2026-05-28T07:05:00+09:00")
}

// recOn builds a weight record whose occurred_at (and thus its day partition) is
// the given offset-bearing timestamp.
func recOn(id, payload, occurredAt string) olf.Record {
	return olf.Record{
		ID:         id,
		Type:       "weight",
		OLFVersion: "1.0",
		OccurredAt: occurredAt,
		RecordedAt: "2026-05-28T07:06:00+09:00",
		Source:     "test",
		Payload:    json.RawMessage(payload),
	}
}

func ids(t *testing.T, s *FSStore) []string {
	t.Helper()
	var out []string
	if err := s.Scan("weight", func(r olf.Record) error { out = append(out, r.ID); return nil }); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}

func TestReplaceByID_Update(t *testing.T) {
	s := NewFSStore(t.TempDir())
	for _, r := range []olf.Record{rec("a", `{"weight_kg":1}`), rec("b", `{"weight_kg":2}`)} {
		if err := s.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	rep := rec("a", `{"weight_kg":99}`)
	found, err := s.ReplaceByID("weight", "a", &rep)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	got := ids(t, s)
	if len(got) != 2 {
		t.Fatalf("expected 2 records, got %v", got)
	}
	var a olf.Record
	_ = s.Scan("weight", func(r olf.Record) error {
		if r.ID == "a" {
			a = r
		}
		return nil
	})
	if string(a.Payload) != `{"weight_kg":99}` {
		t.Errorf("update not applied: %s", a.Payload)
	}
}

func TestReplaceByID_DeleteToEmpty(t *testing.T) {
	s := NewFSStore(t.TempDir())
	if err := s.Append(rec("a", `{"weight_kg":1}`)); err != nil {
		t.Fatal(err)
	}
	found, err := s.ReplaceByID("weight", "a", nil)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if got := ids(t, s); len(got) != 0 {
		t.Errorf("expected empty after deleting the only record, got %v", got)
	}
}

func TestReplaceByID_NotFoundNoRewrite(t *testing.T) {
	s := NewFSStore(t.TempDir())
	if err := s.Append(rec("a", `{"weight_kg":1}`)); err != nil {
		t.Fatal(err)
	}
	found, err := s.ReplaceByID("weight", "zzz", nil)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("expected not found for absent id")
	}
	if got := ids(t, s); len(got) != 1 {
		t.Errorf("store changed despite no match: %v", got)
	}
}

// A duplicate id (which the canonical model forbids, but which a corrupt/imported
// log could contain) must collapse to a single record on rewrite.
func TestReplaceByID_CollapsesDuplicates(t *testing.T) {
	s := NewFSStore(t.TempDir())
	if err := s.Append(rec("a", `{"weight_kg":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(rec("a", `{"weight_kg":2}`)); err != nil {
		t.Fatal(err)
	}
	rep := rec("a", `{"weight_kg":3}`)
	if _, err := s.ReplaceByID("weight", "a", &rep); err != nil {
		t.Fatal(err)
	}
	if got := ids(t, s); len(got) != 1 {
		t.Errorf("expected duplicates to collapse to 1, got %v", got)
	}
}

// Append places a record under <root>/<type>/<YYYY>/<MM>/<YYYY-MM-DD>.jsonl,
// keyed by the local calendar date of occurred_at (spec/on-disk.md).
func TestAppend_DailyLayout(t *testing.T) {
	root := t.TempDir()
	s := NewFSStore(root)
	if err := s.Append(recOn("a", `{"weight_kg":1}`, "2026-05-28T23:00:00+09:00")); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "weight", "2026", "05", "2026-05-28.jsonl")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected day file at %s: %v", want, err)
	}
}

// The day partition comes from the literal local date of occurred_at, with no
// time-zone math: an evening event keeps its written date even across offsets.
func TestAppend_DayFromLocalDate(t *testing.T) {
	root := t.TempDir()
	s := NewFSStore(root)
	// Same instant, different wall-clock dates by offset.
	if err := s.Append(recOn("jp", `{"weight_kg":1}`, "2026-05-29T07:00:00+09:00")); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(recOn("utc", `{"weight_kg":2}`, "2026-05-28T22:00:00+00:00")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "weight", "2026", "05", "2026-05-29.jsonl")); err != nil {
		t.Errorf("jp record should be filed on 2026-05-29: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "weight", "2026", "05", "2026-05-28.jsonl")); err != nil {
		t.Errorf("utc record should be filed on 2026-05-28: %v", err)
	}
}

// Scan reads across day files in chronological order regardless of append order.
func TestScan_AcrossDaysOrdered(t *testing.T) {
	s := NewFSStore(t.TempDir())
	for _, r := range []olf.Record{
		recOn("c", `{"weight_kg":3}`, "2026-06-02T07:00:00+09:00"),
		recOn("a", `{"weight_kg":1}`, "2026-05-28T07:00:00+09:00"),
		recOn("b", `{"weight_kg":2}`, "2026-05-30T07:00:00+09:00"),
	} {
		if err := s.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	got := ids(t, s)
	want := []string{"a", "b", "c"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("scan order = %v, want %v", got, want)
	}
}

// An update that changes occurred_at to a different day moves the record to the
// new day file and removes it from the old one.
func TestReplaceByID_UpdateMovesDay(t *testing.T) {
	root := t.TempDir()
	s := NewFSStore(root)
	if err := s.Append(recOn("a", `{"weight_kg":1}`, "2026-05-28T07:00:00+09:00")); err != nil {
		t.Fatal(err)
	}
	rep := recOn("a", `{"weight_kg":9}`, "2026-06-01T07:00:00+09:00")
	found, err := s.ReplaceByID("weight", "a", &rep)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if got := ids(t, s); len(got) != 1 || got[0] != "a" {
		t.Fatalf("expected exactly record a after move, got %v", got)
	}
	oldFile := filepath.Join(root, "weight", "2026", "05", "2026-05-28.jsonl")
	if data, err := os.ReadFile(oldFile); err == nil && len(data) > 0 {
		t.Errorf("old day file should be empty/removed, still has: %s", data)
	}
	newFile := filepath.Join(root, "weight", "2026", "06", "2026-06-01.jsonl")
	if _, err := os.Stat(newFile); err != nil {
		t.Errorf("record should now live in new day file %s: %v", newFile, err)
	}
}

// Deleting the only record in a day partition removes the now-empty day file and
// prunes its empty parent directories.
func TestReplaceByID_DeletePrunesEmpty(t *testing.T) {
	root := t.TempDir()
	s := NewFSStore(root)
	if err := s.Append(recOn("a", `{"weight_kg":1}`, "2026-05-28T07:00:00+09:00")); err != nil {
		t.Fatal(err)
	}
	found, err := s.ReplaceByID("weight", "a", nil)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	dayFile := filepath.Join(root, "weight", "2026", "05", "2026-05-28.jsonl")
	if _, err := os.Stat(dayFile); !os.IsNotExist(err) {
		t.Errorf("empty day file should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "weight", "2026")); !os.IsNotExist(err) {
		t.Errorf("empty year dir should be pruned")
	}
}

func TestInvalidTypeRejected(t *testing.T) {
	s := NewFSStore(t.TempDir())
	bad := []string{"", ".", "..", "../escape", "a/b", "x/../../etc"}
	for _, typ := range bad {
		if err := s.Scan(typ, func(olf.Record) error { return nil }); err != ErrInvalidType {
			t.Errorf("Scan(%q): got %v, want ErrInvalidType", typ, err)
		}
		r := rec("z", `{"weight_kg":1}`)
		r.Type = typ
		if err := s.Append(r); err != ErrInvalidType {
			t.Errorf("Append(type=%q): got %v, want ErrInvalidType", typ, err)
		}
		if _, err := s.ReplaceByID(typ, "z", nil); err != ErrInvalidType {
			t.Errorf("ReplaceByID(%q): got %v, want ErrInvalidType", typ, err)
		}
	}
	// A normal type and a reverse-DNS extension type are accepted.
	for _, typ := range []string{"weight", "x.com.acme.mood"} {
		if err := s.Scan(typ, func(olf.Record) error { return nil }); err != nil {
			t.Errorf("Scan(%q): unexpected error %v", typ, err)
		}
	}
}
