package query

import (
	"encoding/json"
	"testing"
	"time"

	"open-lifelog.org/node/internal/olf"
	"open-lifelog.org/node/internal/store"
)

func mustInstant(t *testing.T, s string) time.Time {
	t.Helper()
	inst, err := olf.ParseInstant(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return inst
}

func rec(id, typ, occurredAt string) olf.Record {
	return olf.Record{
		ID: id, Type: typ, OLFVersion: "1.0",
		OccurredAt: occurredAt, RecordedAt: occurredAt,
		Source: "test", Payload: json.RawMessage(`{}`),
	}
}

// The occurred_at range must be evaluated by true instant: the same instant
// written with a different offset must filter identically against a boundary.
func TestList_OffsetIndependentRange(t *testing.T) {
	s := store.NewFSStore(t.TempDir())
	// Same instant, two offsets.
	if err := s.Append(rec("a", "weight", "2026-01-01T00:00:00+09:00")); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(rec("b", "weight", "2025-12-31T15:00:00Z")); err != nil {
		t.Fatal(err)
	}
	e := New(s)

	from := mustInstant(t, "2025-12-31T14:59:59Z")
	to := mustInstant(t, "2025-12-31T15:00:01Z")
	got, err := e.List("weight", &from, &to)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("both records denote the same instant inside the range; got %d", len(got))
	}

	// A from-bound just after that instant must exclude both, identically.
	after := mustInstant(t, "2025-12-31T15:00:01Z")
	got2, err := e.List("weight", &after, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 0 {
		t.Fatalf("both records precede the from-bound; got %d", len(got2))
	}
}

func TestList_RangeInclusive(t *testing.T) {
	s := store.NewFSStore(t.TempDir())
	for _, ts := range []string{
		"2026-01-01T00:00:00Z",
		"2026-01-02T00:00:00Z",
		"2026-01-03T00:00:00Z",
	} {
		if err := s.Append(rec("id-"+ts, "steps", ts)); err != nil {
			t.Fatal(err)
		}
	}
	from := mustInstant(t, "2026-01-02T00:00:00Z")
	to := mustInstant(t, "2026-01-03T00:00:00Z")
	got, err := New(s).List("steps", &from, &to)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("inclusive [Jan2, Jan3] should match 2; got %d", len(got))
	}
}

func TestGet_LatestWriteWins(t *testing.T) {
	s := store.NewFSStore(t.TempDir())
	_ = s.Append(rec("x", "weight", "2026-01-01T00:00:00Z"))
	upd := rec("x", "weight", "2026-01-01T00:00:00Z") // same id = update
	upd.Payload = json.RawMessage(`{"weight_kg":70.5}`)
	_ = s.Append(upd)

	got, ok, err := New(s).Get("weight", "x")
	if err != nil || !ok {
		t.Fatalf("get x: ok=%v err=%v", ok, err)
	}
	if string(got.Payload) != `{"weight_kg":70.5}` {
		t.Fatalf("latest write should win; got %s", got.Payload)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	s := store.NewFSStore(t.TempDir())
	r := rec("rt", "meal", "2026-01-01T12:00:00+09:00")
	r.Payload = json.RawMessage(`{"raw_input":"ramen and gyoza"}`)
	if err := s.Append(r); err != nil {
		t.Fatal(err)
	}
	got, ok, err := New(s).Get("meal", "rt")
	if err != nil || !ok {
		t.Fatalf("roundtrip: ok=%v err=%v", ok, err)
	}
	if got.OccurredAt != "2026-01-01T12:00:00+09:00" || string(got.Payload) != `{"raw_input":"ramen and gyoza"}` {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}
