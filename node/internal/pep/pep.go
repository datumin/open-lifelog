// Package pep is the Policy Enforcement Point: the grant ledger plus the
// per-request authorization decision. A grant records what a client may do —
// operation (read/write) × type set × data window (occurred_at range) × lifetime
// — and the node consults the ledger on every request (runtime design §7,
// pep-consent-design). The ledger is the single source of truth, so revoking a
// grant (flipping its status) takes effect on the very next request: no token
// revocation lists, no TTL waiting.
package pep

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"open-lifelog.org/node/internal/meta"
)

// Grant is one authorization the owner has given a client.
type Grant struct {
	ID           string
	ClientID     string
	Operation    string // "read" | "write"
	Types        []string
	OccurredFrom *time.Time // data window start; nil = unrestricted
	OccurredTo   *time.Time // data window end; nil = future-continuing
	ExpiresAt    *time.Time // grant lifetime; nil = until revoked
	Status       string     // "active" | "revoked"
	GrantedAt    time.Time
}

// Store is the grant ledger.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

func New(store *meta.Store) *Store {
	return &Store{db: store.DB(), now: time.Now}
}

// Create inserts a new active grant.
func (s *Store) Create(g Grant) error {
	_, err := s.db.Exec(
		`INSERT INTO grants
		 (id, client_id, operation, types, occurred_from, occurred_to, expires_at, status, granted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'active', ?)`,
		g.ID, g.ClientID, g.Operation, jsonArray(g.Types),
		nullTime(g.OccurredFrom), nullTime(g.OccurredTo), nullTime(g.ExpiresAt),
		s.now().UTC().Format(time.RFC3339),
	)
	return err
}

// CreateFromScopes records the owner's consent as grants: one grant per
// operation (read/write), with the union of the approved types. readWindow is the
// owner-chosen data window (occurred_at range) applied to the READ grant only — a
// window bounds what data is disclosed, not what may be written; a zero Window
// means unrestricted. Lifetime defaults to until-revoke (v1). offline_access and
// other non-lifelog scopes are ignored.
func (s *Store) CreateFromScopes(clientID string, scopes []string, readWindow Window, newID func() string) error {
	byOp := map[string][]string{}
	for _, sc := range scopes {
		if op, typ, ok := ParseScope(sc); ok {
			byOp[op] = append(byOp[op], typ)
		}
	}
	for _, op := range []string{"read", "write"} { // deterministic order
		types, present := byOp[op]
		if !present {
			continue
		}
		g := Grant{ID: newID(), ClientID: clientID, Operation: op, Types: normalizeTypes(types)}
		if op == "read" { // the data window bounds disclosure, i.e. reads only
			g.OccurredFrom = readWindow.From
			g.OccurredTo = readWindow.To
		}
		if err := s.Create(g); err != nil {
			return err
		}
	}
	return nil
}

// RevokeAllForClient revokes every active grant of a client. Used to replace a
// client's grants when it re-consents, keeping the ledger free of duplicates.
func (s *Store) RevokeAllForClient(clientID string) error {
	_, err := s.db.Exec(
		`UPDATE grants SET status='revoked', revoked_at=? WHERE client_id=? AND status='active'`,
		s.now().UTC().Format(time.RFC3339), clientID,
	)
	return err
}

func normalizeTypes(types []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range types {
		if t == "*" {
			return []string{"*"} // wildcard subsumes the rest
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// Window is the occurred_at range a read is constrained to.
type Window struct {
	From, To *time.Time
}

// Contains reports whether instant t falls within the window (nil bound =
// unbounded on that side; both bounds inclusive).
func (w Window) Contains(t time.Time) bool {
	if w.From != nil && t.Before(*w.From) {
		return false
	}
	if w.To != nil && t.After(*w.To) {
		return false
	}
	return true
}

// IntersectRange narrows a client's requested [reqFrom,reqTo] range by the grant
// window w, returning the effective range actually served and whether the window
// clipped the request (i.e. the client asked for more than the window allows).
// clipped lets a read surface tell a client "0 results = not visible" apart from
// "0 results = nothing happened".
func IntersectRange(reqFrom, reqTo *time.Time, w Window) (effFrom, effTo *time.Time, clipped bool) {
	effFrom = laterOf(reqFrom, w.From)
	effTo = earlierOf(reqTo, w.To)
	lowerClipped := w.From != nil && (reqFrom == nil || w.From.After(*reqFrom))
	upperClipped := w.To != nil && (reqTo == nil || w.To.Before(*reqTo))
	return effFrom, effTo, lowerClipped || upperClipped
}

// Decision is the result of an authorization check.
type Decision struct {
	Allowed bool
	Window  Window // for reads: the occurred_at window to enforce
}

// Authorize decides whether clientID may perform operation on typ right now. For
// an allowed read, Window carries the grant's occurred_at bounds to inject into
// the query. When more than one active grant matches (op,typ), their windows are
// INTERSECTED (most restrictive wins) so the decision never fails open on grant
// ordering or on a broad grant shadowing a narrow one.
func (s *Store) Authorize(clientID, operation, typ string) (Decision, error) {
	grants, err := s.activeForClient(clientID)
	if err != nil {
		return Decision{}, err
	}
	allowed := false
	var win Window
	for _, g := range grants {
		if g.Operation != operation || !coversType(g.Types, typ) {
			continue
		}
		if !allowed {
			win = Window{From: g.OccurredFrom, To: g.OccurredTo}
			allowed = true
			continue
		}
		win.From = laterOf(win.From, g.OccurredFrom)
		win.To = earlierOf(win.To, g.OccurredTo)
	}
	return Decision{Allowed: allowed, Window: win}, nil
}

// ReadWindowFor returns clientID's current read window — the intersection of its
// active read grants' windows. A re-grant (consent re-issue, dashboard edit,
// owner token) reads this first so it can PRESERVE the window rather than reset
// it to unrestricted. A client with no active read grant yields a zero window.
func (s *Store) ReadWindowFor(clientID string) (Window, error) {
	d, err := s.authorizeAnyType(clientID, "read")
	if err != nil {
		return Window{}, err
	}
	return d.Window, nil
}

// authorizeAnyType is Authorize without a type filter: it intersects the windows
// of every active grant for the operation, regardless of which types they cover.
func (s *Store) authorizeAnyType(clientID, operation string) (Decision, error) {
	grants, err := s.activeForClient(clientID)
	if err != nil {
		return Decision{}, err
	}
	allowed := false
	var win Window
	for _, g := range grants {
		if g.Operation != operation {
			continue
		}
		if !allowed {
			win = Window{From: g.OccurredFrom, To: g.OccurredTo}
			allowed = true
			continue
		}
		win.From = laterOf(win.From, g.OccurredFrom)
		win.To = earlierOf(win.To, g.OccurredTo)
	}
	return Decision{Allowed: allowed, Window: win}, nil
}

// laterOf / earlierOf pick the more restrictive of two optional bounds (nil =
// unbounded on that side), so intersecting windows narrows, never widens.
func laterOf(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case b.After(*a):
		return b
	default:
		return a
	}
}

func earlierOf(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case b.Before(*a):
		return b
	default:
		return a
	}
}

// List returns all grants (newest first) for the owner dashboard.
func (s *Store) List() ([]Grant, error) {
	return s.query(`SELECT id, client_id, operation, types, occurred_from, occurred_to, expires_at, status, granted_at
		FROM grants ORDER BY granted_at DESC`)
}

// Revoke flips a grant to revoked. Returns found=false if no such grant.
func (s *Store) Revoke(id string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE grants SET status='revoked', revoked_at=? WHERE id=? AND status='active'`,
		s.now().UTC().Format(time.RFC3339), id,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// activeForClient returns the client's grants that are active and not past their
// lifetime. Expiry is evaluated in Go on the parsed instant — NOT as a SQL string
// compare, which on variable-width RFC3339Nano timestamps would order
// lexicographically and could treat an expired grant as active (fail open).
func (s *Store) activeForClient(clientID string) ([]Grant, error) {
	grants, err := s.query(`SELECT id, client_id, operation, types, occurred_from, occurred_to, expires_at, status, granted_at
		FROM grants
		WHERE client_id=? AND status='active' ORDER BY granted_at`, clientID)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	out := grants[:0]
	for _, g := range grants {
		if g.ExpiresAt != nil && !g.ExpiresAt.After(now) { // expired (expiry <= now)
			continue
		}
		out = append(out, g)
	}
	return out, nil
}

func (s *Store) query(q string, args ...any) ([]Grant, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		var types, granted string
		var from, to, exp sql.NullString
		if err := rows.Scan(&g.ID, &g.ClientID, &g.Operation, &types, &from, &to, &exp, &g.Status, &granted); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(types), &g.Types)
		g.OccurredFrom = parseTime(from)
		g.OccurredTo = parseTime(to)
		g.ExpiresAt = parseTime(exp)
		if t, err := time.Parse(time.RFC3339, granted); err == nil {
			g.GrantedAt = t
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func coversType(types []string, typ string) bool {
	for _, t := range types {
		if t == "*" || t == typ {
			return true
		}
	}
	return false
}

func jsonArray(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	// Nano precision so a window bound (e.g. an end-of-day 23:59:59.999999999)
	// round-trips exactly — a precision loss here would shift the boundary and
	// silently widen or narrow what a grant discloses.
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s.String) // also parses non-fractional RFC 3339
	if err != nil {
		return nil
	}
	return &t
}

// ParseScope splits an OLF scope "lifelog:<op>:<type>" into operation and type.
// ok is false for non-lifelog or malformed scopes (e.g. offline_access). The
// type may be "*" (wildcard).
func ParseScope(scope string) (operation, typ string, ok bool) {
	parts := strings.SplitN(scope, ":", 3)
	if len(parts) != 3 || parts[0] != "lifelog" {
		return "", "", false
	}
	op, t := parts[1], parts[2]
	if (op != "read" && op != "write") || t == "" {
		return "", "", false
	}
	return op, t, true
}
