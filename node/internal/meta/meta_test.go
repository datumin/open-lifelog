package meta

import (
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenMigratesToLatest(t *testing.T) {
	s := openTemp(t)
	v, err := s.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != len(migrations) {
		t.Fatalf("schema version = %d, want %d", v, len(migrations))
	}
}

func TestReopenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	v, _ := s2.SchemaVersion()
	if v != len(migrations) {
		t.Fatalf("after reopen schema version = %d, want %d", v, len(migrations))
	}
}

func TestTablesExist(t *testing.T) {
	s := openTemp(t)
	for _, table := range []string{"oauth_clients", "grants"} {
		var name string
		err := s.DB().QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found: %v", table, err)
		}
	}
}

func TestWALEnabled(t *testing.T) {
	s := openTemp(t)
	var mode string
	if err := s.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

func TestGrantRoundTrips(t *testing.T) {
	s := openTemp(t)
	_, err := s.DB().Exec(
		`INSERT INTO grants (id, client_id, operation, types, status, granted_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"g1", "client-x", "read", `["meal","sleep"]`, "active", "2026-06-04T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("insert grant: %v", err)
	}

	var (
		clientID, operation, types, status string
		occurredTo                         *string // null window end stays NULL
	)
	err = s.DB().QueryRow(
		"SELECT client_id, operation, types, status, occurred_to FROM grants WHERE id=?", "g1",
	).Scan(&clientID, &operation, &types, &status, &occurredTo)
	if err != nil {
		t.Fatalf("query grant: %v", err)
	}
	if clientID != "client-x" || operation != "read" || types != `["meal","sleep"]` || status != "active" {
		t.Errorf("round-trip mismatch: %s %s %s %s", clientID, operation, types, status)
	}
	if occurredTo != nil {
		t.Errorf("occurred_to should be NULL, got %q", *occurredTo)
	}
}
