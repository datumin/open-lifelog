package pep

import (
	"path/filepath"
	"testing"
	"time"

	"open-lifelog.org/node/internal/meta"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	ms, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { ms.Close() })
	return New(ms)
}

func TestParseScope(t *testing.T) {
	cases := []struct {
		in      string
		op, typ string
		ok      bool
	}{
		{"lifelog:read:weight", "read", "weight", true},
		{"lifelog:write:meal", "write", "meal", true},
		{"lifelog:read:*", "read", "*", true},
		{"offline_access", "", "", false},
		{"lifelog:delete:weight", "", "", false},
		{"lifelog:read:", "", "", false},
		{"weird", "", "", false},
	}
	for _, c := range cases {
		op, typ, ok := ParseScope(c.in)
		if ok != c.ok || op != c.op || typ != c.typ {
			t.Errorf("ParseScope(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, op, typ, ok, c.op, c.typ, c.ok)
		}
	}
}

func TestAuthorizeMatching(t *testing.T) {
	s := newStore(t)
	if err := s.Create(Grant{ID: "g1", ClientID: "C", Operation: "write", Types: []string{"weight", "meal"}, Status: "active"}); err != nil {
		t.Fatal(err)
	}

	if d, _ := s.Authorize("C", "write", "weight"); !d.Allowed {
		t.Error("write weight should be allowed")
	}
	if d, _ := s.Authorize("C", "write", "meal"); !d.Allowed {
		t.Error("write meal should be allowed")
	}
	if d, _ := s.Authorize("C", "read", "weight"); d.Allowed {
		t.Error("read should NOT be allowed (write-only grant)")
	}
	if d, _ := s.Authorize("C", "write", "sleep"); d.Allowed {
		t.Error("write sleep should NOT be allowed (not in types)")
	}
	if d, _ := s.Authorize("other", "write", "weight"); d.Allowed {
		t.Error("a different client must not be allowed")
	}
}

func TestAuthorizeWildcard(t *testing.T) {
	s := newStore(t)
	s.Create(Grant{ID: "g1", ClientID: "C", Operation: "read", Types: []string{"*"}, Status: "active"})
	for _, typ := range []string{"weight", "meal", "x.com.acme.mood"} {
		if d, _ := s.Authorize("C", "read", typ); !d.Allowed {
			t.Errorf("wildcard read should allow %q", typ)
		}
	}
}

func TestRevokeDeniesImmediately(t *testing.T) {
	s := newStore(t)
	s.Create(Grant{ID: "g1", ClientID: "C", Operation: "read", Types: []string{"weight"}, Status: "active"})
	if d, _ := s.Authorize("C", "read", "weight"); !d.Allowed {
		t.Fatal("should be allowed before revoke")
	}
	ok, err := s.Revoke("g1")
	if err != nil || !ok {
		t.Fatalf("revoke: ok=%v err=%v", ok, err)
	}
	if d, _ := s.Authorize("C", "read", "weight"); d.Allowed {
		t.Error("should be denied immediately after revoke")
	}
}

func TestExpiredGrantDenied(t *testing.T) {
	s := newStore(t)
	past := time.Now().Add(-time.Hour)
	s.Create(Grant{ID: "g1", ClientID: "C", Operation: "read", Types: []string{"weight"}, ExpiresAt: &past, Status: "active"})
	if d, _ := s.Authorize("C", "read", "weight"); d.Allowed {
		t.Error("an expired grant must not authorize")
	}
}

func TestAuthorizeReturnsWindow(t *testing.T) {
	s := newStore(t)
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.Create(Grant{ID: "g1", ClientID: "C", Operation: "read", Types: []string{"weight"}, OccurredFrom: &from, Status: "active"})
	d, _ := s.Authorize("C", "read", "weight")
	if !d.Allowed || d.Window.From == nil || !d.Window.From.Equal(from) {
		t.Fatalf("expected window from %v, got %+v", from, d.Window)
	}
}

func TestCreateFromScopes(t *testing.T) {
	s := newStore(t)
	id := 0
	newID := func() string { id++; return "g" + string(rune('0'+id)) }
	err := s.CreateFromScopes("C", []string{"lifelog:read:weight", "lifelog:read:meal", "lifelog:write:weight", "offline_access"}, Window{}, newID)
	if err != nil {
		t.Fatal(err)
	}
	grants, _ := s.List()
	var read, write *Grant
	for i := range grants {
		switch grants[i].Operation {
		case "read":
			read = &grants[i]
		case "write":
			write = &grants[i]
		}
	}
	if read == nil || write == nil {
		t.Fatalf("expected one read and one write grant, got %+v", grants)
	}
	if len(read.Types) != 2 {
		t.Errorf("read grant should cover 2 types, got %v", read.Types)
	}
	if len(write.Types) != 1 || write.Types[0] != "weight" {
		t.Errorf("write grant types = %v", write.Types)
	}
}

// The owner's data window is applied to the READ grant only; the write grant is
// left unrestricted (a window bounds what data is disclosed, not what is added).
func TestCreateFromScopes_ReadWindowAppliesToReadOnly(t *testing.T) {
	s := newStore(t)
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 31, 23, 59, 59, 999999999, time.UTC)
	id := 0
	newID := func() string { id++; return "g" + string(rune('0'+id)) }
	err := s.CreateFromScopes("C",
		[]string{"lifelog:read:weight", "lifelog:write:weight"},
		Window{From: &from, To: &to}, newID)
	if err != nil {
		t.Fatal(err)
	}
	grants, _ := s.List()
	var read, write *Grant
	for i := range grants {
		switch grants[i].Operation {
		case "read":
			read = &grants[i]
		case "write":
			write = &grants[i]
		}
	}
	if read == nil || write == nil {
		t.Fatalf("expected one read and one write grant, got %+v", grants)
	}
	if read.OccurredFrom == nil || !read.OccurredFrom.Equal(from) {
		t.Errorf("read grant From = %v, want %v", read.OccurredFrom, from)
	}
	if read.OccurredTo == nil || !read.OccurredTo.Equal(to) {
		t.Errorf("read grant To = %v, want %v", read.OccurredTo, to)
	}
	if write.OccurredFrom != nil || write.OccurredTo != nil {
		t.Errorf("write grant must be unrestricted, got [%v,%v]", write.OccurredFrom, write.OccurredTo)
	}
	// The window flows into the read authorization decision.
	d, _ := s.Authorize("C", "read", "weight")
	if !d.Allowed || d.Window.From == nil || !d.Window.From.Equal(from) || d.Window.To == nil || !d.Window.To.Equal(to) {
		t.Errorf("read decision window = %+v, want [%v,%v]", d.Window, from, to)
	}
}

// When more than one active grant matches (op,typ) — which can happen if grant
// rows ever accumulate — Authorize returns the MOST RESTRICTIVE window
// (intersection), never the widest, so it can't fail open on grant ordering.
func TestAuthorizeIntersectsOverlappingGrants(t *testing.T) {
	s := newStore(t)
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	// g1 bounds only the lower side; g2 (wildcard) bounds only the upper side.
	if err := s.Create(Grant{ID: "g1", ClientID: "C", Operation: "read", Types: []string{"weight"}, OccurredFrom: &from, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(Grant{ID: "g2", ClientID: "C", Operation: "read", Types: []string{"*"}, OccurredTo: &to, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	d, _ := s.Authorize("C", "read", "weight")
	if !d.Allowed || d.Window.From == nil || !d.Window.From.Equal(from) || d.Window.To == nil || !d.Window.To.Equal(to) {
		t.Fatalf("expected intersected window [%v,%v], got %+v", from, to, d.Window)
	}
}

// ReadWindowFor returns a client's current read window so a re-grant can preserve
// it instead of silently widening to unrestricted.
func TestReadWindowFor(t *testing.T) {
	s := newStore(t)
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 31, 23, 59, 59, 999999999, time.UTC)
	if err := s.Create(Grant{ID: "g1", ClientID: "C", Operation: "read", Types: []string{"weight"}, OccurredFrom: &from, OccurredTo: &to, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	w, err := s.ReadWindowFor("C")
	if err != nil {
		t.Fatal(err)
	}
	if w.From == nil || !w.From.Equal(from) || w.To == nil || !w.To.Equal(to) {
		t.Fatalf("ReadWindowFor = %+v, want [%v,%v]", w, from, to)
	}
	// No read grant -> zero (unrestricted) window, no error.
	if w2, err := s.ReadWindowFor("D"); err != nil || w2.From != nil || w2.To != nil {
		t.Fatalf("ReadWindowFor(no grant) = %+v err=%v, want zero", w2, err)
	}
}

// An expired grant must be denied even when now carries sub-second precision —
// a lexicographic timestamp compare would fail OPEN here.
func TestExpiredGrantDeniedWithFractionalNow(t *testing.T) {
	s := newStore(t)
	s.now = func() time.Time { return time.Date(2026, 6, 6, 12, 0, 0, 500000000, time.UTC) }
	exp := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC) // already in the past relative to now
	if err := s.Create(Grant{ID: "g1", ClientID: "C", Operation: "read", Types: []string{"weight"}, ExpiresAt: &exp, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Authorize("C", "read", "weight"); d.Allowed {
		t.Error("expired grant must be denied (fail closed)")
	}
	// A future expiry is still allowed.
	future := time.Date(2026, 6, 6, 12, 0, 1, 0, time.UTC)
	s.Create(Grant{ID: "g2", ClientID: "E", Operation: "read", Types: []string{"weight"}, ExpiresAt: &future, Status: "active"})
	if d, _ := s.Authorize("E", "read", "weight"); !d.Allowed {
		t.Error("unexpired grant must be allowed")
	}
}

func TestIntersectRange(t *testing.T) {
	tp := func(y, m, d int) *time.Time {
		v := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
		return &v
	}
	cases := []struct {
		name                         string
		reqFrom, reqTo               *time.Time
		win                          Window
		wantFrom, wantTo             *time.Time
		wantClip                     bool
	}{
		{"no window, no request", nil, nil, Window{}, nil, nil, false},
		{"window narrows unbounded request", nil, nil, Window{From: tp(2026, 6, 6), To: tp(2026, 6, 6)}, tp(2026, 6, 6), tp(2026, 6, 6), true},
		{"request inside window: not clipped", tp(2026, 6, 6), tp(2026, 6, 6), Window{From: tp(2026, 1, 1), To: tp(2026, 12, 31)}, tp(2026, 6, 6), tp(2026, 6, 6), false},
		{"window cuts lower side", tp(2026, 1, 1), tp(2026, 6, 30), Window{From: tp(2026, 6, 1)}, tp(2026, 6, 1), tp(2026, 6, 30), true},
		{"window cuts upper side", tp(2026, 1, 1), tp(2026, 12, 31), Window{To: tp(2026, 6, 30)}, tp(2026, 1, 1), tp(2026, 6, 30), true},
		{"window wider than request: not clipped", tp(2026, 6, 1), tp(2026, 6, 30), Window{From: tp(2026, 1, 1), To: tp(2026, 12, 31)}, tp(2026, 6, 1), tp(2026, 6, 30), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ef, et, clip := IntersectRange(c.reqFrom, c.reqTo, c.win)
			eq := func(a, b *time.Time) bool { return (a == nil) == (b == nil) && (a == nil || a.Equal(*b)) }
			if !eq(ef, c.wantFrom) || !eq(et, c.wantTo) || clip != c.wantClip {
				t.Errorf("IntersectRange = (%v,%v,%v), want (%v,%v,%v)", ef, et, clip, c.wantFrom, c.wantTo, c.wantClip)
			}
		})
	}
}

func TestWindowContains(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	from := at("2026-06-06T00:00:00Z")
	to := at("2026-06-06T23:59:59Z")
	w := Window{From: &from, To: &to}
	if !w.Contains(at("2026-06-06T12:00:00Z")) {
		t.Error("midday should be inside")
	}
	if w.Contains(at("2026-06-05T23:00:00Z")) || w.Contains(at("2026-06-07T00:00:00Z")) {
		t.Error("outside instants must not be contained")
	}
	if !(Window{}).Contains(at("2000-01-01T00:00:00Z")) {
		t.Error("zero window contains everything")
	}
}

func TestNormalizeWildcardCollapses(t *testing.T) {
	s := newStore(t)
	s.CreateFromScopes("C", []string{"lifelog:read:weight", "lifelog:read:*"}, Window{}, func() string { return "g1" })
	grants, _ := s.List()
	if len(grants) != 1 || len(grants[0].Types) != 1 || grants[0].Types[0] != "*" {
		t.Fatalf("wildcard should collapse types to [*], got %+v", grants)
	}
}
