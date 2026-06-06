package write

import (
	"encoding/json"
	"testing"
	"time"

	"open-lifelog.org/node/internal/olf"
	"open-lifelog.org/node/internal/query"
	"open-lifelog.org/node/internal/store"
	"open-lifelog.org/node/internal/validate"
)

func newService(t *testing.T, now time.Time) (*Service, *store.FSStore) {
	t.Helper()
	v, err := validate.New()
	if err != nil {
		t.Fatalf("validate.New: %v", err)
	}
	st := store.NewFSStore(t.TempDir())
	return New(st, v, func() time.Time { return now }), st
}

func TestRecord_MintsEnvelopeAndPersists(t *testing.T) {
	now := time.Date(2026, 5, 29, 7, 10, 0, 0, time.FixedZone("JST", 9*3600))
	svc, st := newService(t, now)

	got, err := svc.Record(RecordInput{
		Type:       "weight",
		OLFVersion: "1.0",
		OccurredAt: "2026-05-28T07:05:00+09:00",
		TZ:         "Asia/Tokyo",
		Source:     "test",
		Payload:    json.RawMessage(`{"weight_kg":70.5}`),
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	// The node mints id (UUIDv7) and recorded_at; the caller does not supply them.
	if _, err := olf.ParseInstant(got.RecordedAt); err != nil {
		t.Errorf("recorded_at not a valid instant: %v", err)
	}
	if want := now.Format(time.RFC3339Nano); got.RecordedAt != want {
		t.Errorf("recorded_at = %q, want %q", got.RecordedAt, want)
	}
	if len(got.ID) != 36 || got.ID[14] != '7' {
		t.Errorf("id %q is not a UUIDv7", got.ID)
	}

	// read-after-write: the record is queryable by id.
	eng := query.New(st)
	back, found, err := eng.Get("weight", got.ID)
	if err != nil || !found {
		t.Fatalf("Get after write: found=%v err=%v", found, err)
	}
	if back.OccurredAt != got.OccurredAt || string(back.Payload) != `{"weight_kg":70.5}` {
		t.Errorf("round-trip mismatch: %+v", back)
	}
}

func TestRecord_RejectsInvalidPayload_NoWrite(t *testing.T) {
	now := time.Date(2026, 5, 29, 7, 10, 0, 0, time.UTC)
	svc, st := newService(t, now)

	_, err := svc.Record(RecordInput{
		Type:       "weight",
		OLFVersion: "1.0",
		OccurredAt: "2026-05-28T07:05:00+09:00",
		Source:     "test",
		Payload:    json.RawMessage(`{"weight_kg":-1}`), // exclusiveMinimum 0
	})
	if err == nil {
		t.Fatal("expected validation error for negative weight")
	}

	// The store must be untouched on a validation failure.
	n := 0
	if err := st.Scan("weight", func(olf.Record) error { n++; return nil }); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 0 {
		t.Errorf("expected no records persisted, found %d", n)
	}
}

// countType returns how many lines (records) the store holds for typ.
func countType(t *testing.T, st *store.FSStore, typ string) int {
	t.Helper()
	n := 0
	if err := st.Scan(typ, func(olf.Record) error { n++; return nil }); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return n
}

func TestUpdate_RewritesInPlace(t *testing.T) {
	now := time.Date(2026, 5, 29, 7, 10, 0, 0, time.UTC)
	svc, st := newService(t, now)

	orig, err := svc.Record(RecordInput{
		Type: "weight", OLFVersion: "1.0",
		OccurredAt: "2026-05-28T07:05:00+09:00", Source: "test",
		Payload: json.RawMessage(`{"weight_kg":70.5}`),
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Edit at a later time; recorded_at should re-mint, id must be preserved.
	later := now.Add(time.Hour)
	svc.now = func() time.Time { return later }

	upd, err := svc.Update(UpdateInput{
		Type: "weight", ID: orig.ID, OLFVersion: "1.0",
		OccurredAt: "2026-05-28T07:05:00+09:00", Source: "test",
		Payload: json.RawMessage(`{"weight_kg":71.0}`),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd.ID != orig.ID {
		t.Errorf("id changed on update: %q -> %q", orig.ID, upd.ID)
	}
	if upd.RecordedAt == orig.RecordedAt {
		t.Errorf("recorded_at was not re-minted on update")
	}

	// Canonical invariant: the id still appears exactly once.
	if n := countType(t, st, "weight"); n != 1 {
		t.Errorf("expected exactly 1 record after update, got %d", n)
	}
	back, found, err := query.New(st).Get("weight", orig.ID)
	if err != nil || !found {
		t.Fatalf("Get after update: found=%v err=%v", found, err)
	}
	if string(back.Payload) != `{"weight_kg":71.0}` {
		t.Errorf("update not reflected: %s", back.Payload)
	}
}

func TestUpdate_NotFound(t *testing.T) {
	svc, _ := newService(t, time.Now())
	_, err := svc.Update(UpdateInput{
		Type: "weight", ID: "0192f3a0-7b1e-7c3d-8e4f-0a1b2c3d4e5f", OLFVersion: "1.0",
		OccurredAt: "2026-05-28T07:05:00+09:00", Source: "test",
		Payload: json.RawMessage(`{"weight_kg":71.0}`),
	})
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestUpdate_InvalidLeavesStoreUntouched(t *testing.T) {
	now := time.Date(2026, 5, 29, 7, 10, 0, 0, time.UTC)
	svc, st := newService(t, now)
	orig, err := svc.Record(RecordInput{
		Type: "weight", OLFVersion: "1.0",
		OccurredAt: "2026-05-28T07:05:00+09:00", Source: "test",
		Payload: json.RawMessage(`{"weight_kg":70.5}`),
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	if _, err := svc.Update(UpdateInput{
		Type: "weight", ID: orig.ID, OLFVersion: "1.0",
		OccurredAt: "2026-05-28T07:05:00+09:00", Source: "test",
		Payload: json.RawMessage(`{"weight_kg":-5}`), // invalid
	}); err == nil {
		t.Fatal("expected validation error on update")
	}

	back, _, _ := query.New(st).Get("weight", orig.ID)
	if string(back.Payload) != `{"weight_kg":70.5}` {
		t.Errorf("store mutated by a failed update: %s", back.Payload)
	}
}

func TestDelete_RemovesRecord(t *testing.T) {
	now := time.Date(2026, 5, 29, 7, 10, 0, 0, time.UTC)
	svc, st := newService(t, now)
	rec1, _ := svc.Record(RecordInput{
		Type: "weight", OLFVersion: "1.0",
		OccurredAt: "2026-05-28T07:05:00+09:00", Source: "test",
		Payload: json.RawMessage(`{"weight_kg":70.5}`),
	})
	rec2, _ := svc.Record(RecordInput{
		Type: "weight", OLFVersion: "1.0",
		OccurredAt: "2026-05-28T08:05:00+09:00", Source: "test",
		Payload: json.RawMessage(`{"weight_kg":71.0}`),
	})

	if err := svc.Delete("weight", rec1.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, found, _ := query.New(st).Get("weight", rec1.ID); found {
		t.Error("deleted record is still queryable")
	}
	// The other record survives; exactly one remains.
	if n := countType(t, st, "weight"); n != 1 {
		t.Errorf("expected 1 record after delete, got %d", n)
	}
	if _, found, _ := query.New(st).Get("weight", rec2.ID); !found {
		t.Error("unrelated record was removed by delete")
	}
}

func TestDelete_NotFound(t *testing.T) {
	svc, _ := newService(t, time.Now())
	if err := svc.Delete("weight", "0192f3a0-7b1e-7c3d-8e4f-0a1b2c3d4e5f"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestRecord_RejectsSemanticInvariantViolation(t *testing.T) {
	now := time.Date(2026, 5, 29, 7, 10, 0, 0, time.UTC)
	svc, _ := newService(t, now)

	_, err := svc.Record(RecordInput{
		Type:       "steps",
		OLFVersion: "1.0",
		OccurredAt: "2026-05-28T10:00:00+09:00",
		Source:     "test",
		Payload:    json.RawMessage(`{"count":100,"ended_at":"2026-05-28T09:00:00+09:00"}`),
	})
	if err == nil {
		t.Fatal("expected ended_at < occurred_at to be rejected by the write usecase")
	}
}
