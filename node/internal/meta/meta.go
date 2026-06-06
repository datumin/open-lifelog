// Package meta is the node's embedded metadata store: a single-file SQLite
// database (pure-Go driver, no cgo, so the node stays a true single binary) that
// holds everything that is *not* lifelog data — OAuth client registrations and
// the PEP grant ledger. The lifelog itself stays in the canonical JSONL store;
// this DB is derived/operational state.
//
// Portable-type rule (design §5): so the same schema works on SQLite here and
// Postgres in the SaaS, ids are TEXT (uuid strings), list-valued columns are
// JSON TEXT (no array type), and timestamps are RFC 3339 TEXT.
package meta

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

// Store wraps the metadata database.
type Store struct {
	db *sql.DB
}

// migrations are applied in order; the applied count is tracked in the SQLite
// PRAGMA user_version. Never edit a released migration — append a new one.
var migrations = []string{
	// 1: OAuth client registrations (DCR) and the PEP grant ledger.
	`
CREATE TABLE oauth_clients (
    client_id                  TEXT PRIMARY KEY,
    client_name                TEXT NOT NULL DEFAULT '',
    redirect_uris              TEXT NOT NULL DEFAULT '[]',   -- JSON array
    token_endpoint_auth_method TEXT NOT NULL DEFAULT 'none',
    grant_types                TEXT NOT NULL DEFAULT '[]',   -- JSON array
    scope                      TEXT NOT NULL DEFAULT '',
    registered_at              TEXT NOT NULL                 -- RFC 3339
);

CREATE TABLE grants (
    id            TEXT PRIMARY KEY,
    client_id     TEXT NOT NULL,
    operation     TEXT NOT NULL,                 -- 'read' | 'write'
    types         TEXT NOT NULL,                 -- JSON array, or ["*"]
    occurred_from TEXT,                          -- date | null (data window start)
    occurred_to   TEXT,                          -- date | null (null = future-continuing)
    expires_at    TEXT,                          -- timestamp | null (null = until revoked)
    status        TEXT NOT NULL,                 -- 'active' | 'revoked' | 'expired'
    granted_at    TEXT NOT NULL,
    revoked_at    TEXT
);

CREATE INDEX grants_by_client ON grants(client_id, status);
`,
	// 2: OAuth authorization codes and tokens (opaque, stored hashed). Keeping
	// tokens server-side gives immediate revocation, matching the PEP philosophy.
	`
CREATE TABLE auth_codes (
    code_hash             TEXT PRIMARY KEY,      -- sha256(code)
    client_id             TEXT NOT NULL,
    redirect_uri          TEXT NOT NULL,
    scope                 TEXT NOT NULL DEFAULT '',
    resource              TEXT NOT NULL DEFAULT '',
    code_challenge        TEXT NOT NULL,
    code_challenge_method TEXT NOT NULL,
    expires_at            TEXT NOT NULL,
    created_at            TEXT NOT NULL
);

CREATE TABLE access_tokens (
    token_hash  TEXT PRIMARY KEY,                -- sha256(token)
    client_id   TEXT NOT NULL,
    scope       TEXT NOT NULL DEFAULT '',
    resource    TEXT NOT NULL DEFAULT '',
    expires_at  TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE TABLE refresh_tokens (
    token_hash  TEXT PRIMARY KEY,                -- sha256(token)
    client_id   TEXT NOT NULL,
    scope       TEXT NOT NULL DEFAULT '',
    resource    TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
);
`,
	// 3: owner authentication. A single generated owner secret (stored hashed)
	// gates the consent screen; owner_sessions are browser login sessions.
	`
CREATE TABLE owner_secret (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    secret_hash TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE TABLE owner_sessions (
    session_hash TEXT PRIMARY KEY,               -- sha256(session token)
    csrf_token   TEXT NOT NULL,                  -- per-session CSRF token (form-embedded)
    expires_at   TEXT NOT NULL,
    created_at   TEXT NOT NULL
);
`,
	// 4: per-link MCP endpoints. Each link is a scoped MCP URL the owner can
	// hand out (e.g. a meal-only link); the link bounds tools, scopes, audience.
	`
CREATE TABLE mcp_links (
    id         TEXT PRIMARY KEY,           -- random URL segment
    label      TEXT NOT NULL DEFAULT '',   -- human note ("Claude — meal only")
    types      TEXT NOT NULL,              -- JSON array of OLF types
    ops        TEXT NOT NULL,              -- JSON array of operations ("read"/"write")
    created_at TEXT NOT NULL,
    revoked_at TEXT                        -- null = active
);
`,
}

// Open opens (creating if needed) the SQLite database at path and applies any
// pending migrations. The caller owns Close.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One writer keeps it simple for a single-user node; WAL still allows
	// lock-free readers. busy_timeout avoids spurious SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// DB exposes the underlying handle for the repositories (OAuth clients, grants)
// built on top of this store in later steps.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Close() error { return s.db.Close() }

// SchemaVersion reports how many migrations have been applied.
func (s *Store) SchemaVersion() (int, error) {
	var v int
	err := s.db.QueryRow("PRAGMA user_version").Scan(&v)
	return v, err
}

func (s *Store) migrate() error {
	current, err := s.SchemaVersion()
	if err != nil {
		return err
	}
	for i := current; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		// PRAGMA user_version doesn't accept placeholders.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("bump user_version to %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
