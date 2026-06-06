package restapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"open-lifelog.org/node/internal/links"
	"open-lifelog.org/node/internal/meta"
	"open-lifelog.org/node/internal/olf"
	"open-lifelog.org/node/internal/pep"
	"open-lifelog.org/node/internal/query"
	"open-lifelog.org/node/internal/store"
	"open-lifelog.org/node/internal/validate"
	"open-lifelog.org/node/internal/wire"
	"open-lifelog.org/node/internal/write"
)

// --- response-envelope decode helpers ---

func listData(t *testing.T, b []byte) ([]olf.Record, wire.ListMeta) {
	t.Helper()
	var env struct {
		Data []olf.Record  `json:"data"`
		Meta wire.ListMeta `json:"meta"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("decode list envelope: %v (%s)", err, b)
	}
	return env.Data, env.Meta
}

func recordData(t *testing.T, b []byte) (olf.Record, wire.OpMeta) {
	t.Helper()
	var env struct {
		Data olf.Record  `json:"data"`
		Meta wire.OpMeta `json:"meta"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("decode record envelope: %v (%s)", err, b)
	}
	return env.Data, env.Meta
}

func errorBody(t *testing.T, b []byte) wire.ErrorBody {
	t.Helper()
	var env wire.ErrorEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("decode error envelope: %v (%s)", err, b)
	}
	return env.Error
}

func hasWarning(ws []wire.Warning, code string) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}

// knownTypes are the standard payload types the capability parser expands "*"
// across in these tests.
var knownTypes = []string{"weight", "meal", "sleep", "steps"}

type fixture struct {
	api    *API
	grants *pep.Store
	w      *write.Service
	idn    int // monotonic source of unique grant ids
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	v, err := validate.New()
	if err != nil {
		t.Fatalf("validate.New: %v", err)
	}
	dir := t.TempDir()
	st := store.NewFSStore(dir)
	q := query.New(st)
	w := write.New(st, v, nil)

	mstore, err := meta.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { mstore.Close() })
	grants := pep.New(mstore)
	return &fixture{api: New(q, w, v, grants, knownTypes), grants: grants, w: w}
}

// grant gives clientID the listed scopes (e.g. "lifelog:read:weight"). Grant
// ids come from a monotonic counter so repeated calls never collide.
func (f *fixture) grant(t *testing.T, clientID string, scopes ...string) {
	t.Helper()
	if err := f.grants.CreateFromScopes(clientID, scopes, pep.Window{}, func() string {
		f.idn++
		return fmt.Sprintf("grant-%d", f.idn)
	}); err != nil {
		t.Fatalf("CreateFromScopes: %v", err)
	}
}

// seedWeight writes a weight record at the given instant and returns its id.
func (f *fixture) seedWeight(t *testing.T, occurredAt string, kg float64) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"weight_kg": kg})
	rec, err := f.w.Record(write.RecordInput{
		Type: "weight", OLFVersion: "1.0", OccurredAt: occurredAt,
		Source: "test", Payload: body,
	})
	if err != nil {
		t.Fatalf("seed weight: %v", err)
	}
	return rec.ID
}

// call drives a handler through the real bearer middleware so the client
// identity lands in the request context exactly as it would in production.
// clientID == "" simulates a missing/invalid token (no Authorization header).
// capability != "" attaches a parsed link as an upper bound.
func (f *fixture) call(t *testing.T, h http.HandlerFunc, method, target, clientID, capability, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, reqBody)
	// httptest.NewRequest does not populate path wildcards (only a ServeMux
	// match does), so set {type}/{id} the way the production routes would.
	typ, id := pathVals(target)
	if typ != "" {
		r.SetPathValue("type", typ)
	}
	if id != "" {
		r.SetPathValue("id", id)
	}

	// Attach the capability link the same way cmd/olf's restLinkRouter does.
	var handler http.Handler = h
	if capability != "" {
		l, err := links.Parse(capability, knownTypes)
		if err != nil {
			t.Fatalf("links.Parse(%q): %v", capability, err)
		}
		r = r.WithContext(WithLink(r.Context(), &l))
	}

	if clientID != "" {
		// A verifier that yields the chosen client for any presented token.
		verify := func(context.Context, string, *http.Request) (*auth.TokenInfo, error) {
			return &auth.TokenInfo{UserID: clientID, Expiration: time.Now().Add(time.Hour)}, nil
		}
		handler = auth.RequireBearerToken(verify, nil)(handler)
		r.Header.Set("Authorization", "Bearer dummy-token")
	}

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, r)
	return rr
}

// pathVals extracts the {type} and {id} segments from a REST target path the
// way the production route patterns (…/query/{type}/{id}, …/records/{type}/{id})
// bind them.
func pathVals(target string) (typ, id string) {
	p := target
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range segs {
		if (s == "query" || s == "records") && i+1 < len(segs) {
			typ = segs[i+1]
			if i+2 < len(segs) {
				id = segs[i+2]
			}
		}
	}
	return typ, id
}

func TestList_RequiresGrant(t *testing.T) {
	f := newFixture(t)
	f.seedWeight(t, "2026-05-28T07:00:00+09:00", 70.5)

	// No grant for this client → 403.
	rr := f.call(t, f.api.List, "GET", "/api/weight:r/query/weight", "client-x", "weight:r", "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("ungranted list: got %d, want 403 (%s)", rr.Code, rr.Body)
	}

	// After granting read:weight → 200 with the record.
	f.grant(t, "client-x", "lifelog:read:weight")
	rr = f.call(t, f.api.List, "GET", "/api/weight:r/query/weight", "client-x", "weight:r", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("granted list: got %d, want 200 (%s)", rr.Code, rr.Body)
	}
	recs, _ := listData(t, rr.Body.Bytes())
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
}

func TestUnauthenticated_Returns401(t *testing.T) {
	f := newFixture(t)
	// clientID "" → no Authorization header → the middleware 401s before the
	// handler. (The handler's own fail-closed branch is exercised below.)
	rr := f.call(t, f.api.List, "GET", "/api/weight:r/query/weight", "", "weight:r", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", rr.Code)
	}
}

func TestHandlerFailsClosedWithoutTokenInfo(t *testing.T) {
	f := newFixture(t)
	// Call the handler directly (no middleware, no TokenInfo in context): the
	// principal is empty, so it must fail closed with 401 rather than proceed.
	r := httptest.NewRequest("GET", "/api/weight:r/query/weight", nil)
	rr := httptest.NewRecorder()
	f.api.List(rr, r)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing TokenInfo: got %d, want 401", rr.Code)
	}
}

func TestCapabilityBound_DeniesBeyondURL(t *testing.T) {
	f := newFixture(t)
	f.seedWeight(t, "2026-05-28T07:00:00+09:00", 70.5)
	// The client is broadly granted at the ledger level...
	f.grant(t, "client-x", "lifelog:read:weight", "lifelog:write:weight", "lifelog:read:meal")

	// ...but the URL capability is meal:r only. Reading weight through it must be
	// refused even though the grant exists — the URL is the upper bound.
	rr := f.call(t, f.api.List, "GET", "/api/meal:r/query/weight", "client-x", "meal:r", "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("capability bound (read weight via meal:r): got %d, want 403 (%s)", rr.Code, rr.Body)
	}

	// Writing weight through a read-only meal capability is likewise refused.
	rr = f.call(t, f.api.Create, "POST", "/api/meal:r/records/weight", "client-x", "meal:r",
		`{"occurred_at":"2026-05-28T07:00:00+09:00","source":"t","payload":{"weight_kg":71}}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("capability bound (write weight via meal:r): got %d, want 403 (%s)", rr.Code, rr.Body)
	}
}

func TestNilLink_AppliesNoCapabilityBound(t *testing.T) {
	f := newFixture(t)
	f.seedWeight(t, "2026-05-28T07:00:00+09:00", 70.5)
	f.grant(t, "client-x", "lifelog:read:weight")
	// Defense-in-depth: if the handler runs without a link in context (no
	// capability bound), it must still enforce PEP and otherwise serve. Production
	// always attaches a capability, but the handler must fail safe, not open.
	rr := f.call(t, f.api.List, "GET", "/api/query/weight", "client-x", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("nil-link granted list: got %d, want 200 (%s)", rr.Code, rr.Body)
	}
}

func TestCreate_RoundTrips(t *testing.T) {
	f := newFixture(t)
	f.grant(t, "writer", "lifelog:write:weight")
	rr := f.call(t, f.api.Create, "POST", "/api/weight:w/records/weight", "writer", "weight:w",
		`{"occurred_at":"2026-05-28T07:00:00+09:00","source":"scale","payload":{"weight_kg":68.2}}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201 (%s)", rr.Code, rr.Body)
	}
	rec, _ := recordData(t, rr.Body.Bytes())
	if rec.ID == "" || rec.Type != "weight" {
		t.Fatalf("create returned no minted record: %s", rr.Body)
	}
	if rec.OLFVersion != "1.0" {
		t.Errorf("olf_version should default to 1.0, got %q", rec.OLFVersion)
	}
}

// A create that omits olf_version is stamped with the type's latest schema
// version (per spec, olf_version is per-type) — here meal's 1.0.
func TestCreateStampsTypeVersionWhenOmitted(t *testing.T) {
	f := newFixture(t)
	f.grant(t, "writer", "lifelog:write:meal")
	rr := f.call(t, f.api.Create, "POST", "/api/meal:w/records/meal", "writer", "meal:w",
		`{"occurred_at":"2026-05-28T07:00:00+09:00","source":"app","payload":{"raw_input":"a sandwich"}}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201 (%s)", rr.Code, rr.Body)
	}
	rec, _ := recordData(t, rr.Body.Bytes())
	if rec.OLFVersion != "1.0" {
		t.Errorf("omitted olf_version should stamp meal's latest (1.0), got %q", rec.OLFVersion)
	}
}

func TestReadWindow_HidesRecordsOutsideGrant(t *testing.T) {
	f := newFixture(t)
	oldID := f.seedWeight(t, "2026-01-01T07:00:00+09:00", 80.0) // before the window
	newID := f.seedWeight(t, "2026-05-28T07:00:00+09:00", 70.0) // inside the window

	// Grant read:weight with an occurred_from of 2026-05-01 (a windowed grant).
	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	if err := f.grants.Create(pep.Grant{
		ID: "g1", ClientID: "client-x", Operation: "read",
		Types: []string{"weight"}, OccurredFrom: &from,
	}); err != nil {
		t.Fatalf("create windowed grant: %v", err)
	}

	// List only returns the in-window record.
	rr := f.call(t, f.api.List, "GET", "/api/weight:r/query/weight", "client-x", "weight:r", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("windowed list: got %d (%s)", rr.Code, rr.Body)
	}
	recs, meta := listData(t, rr.Body.Bytes())
	if len(recs) != 1 || recs[0].ID != newID {
		t.Fatalf("window should hide the pre-window record, got %+v", recs)
	}
	// The list explicitly reports it was clipped by the read window.
	if !meta.Clipped || !hasWarning(meta.Warnings, wire.CodeRangeClippedByScope) {
		t.Errorf("windowed list must report clipped+warning, got %+v", meta)
	}

	// Get on the out-of-window record now DISCLOSES existence: 403 out_of_read_scope
	// with the granted window and the record's occurred_at (single-owner node).
	rr = f.call(t, f.api.Get, "GET", "/api/weight:r/query/weight/"+oldID, "client-x", "weight:r", "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("out-of-window get: got %d, want 403 (%s)", rr.Code, rr.Body)
	}
	eb := errorBody(t, rr.Body.Bytes())
	if eb.Code != wire.CodeOutOfReadScope || eb.GrantedReadWindow == nil || eb.RecordOccurredAt == nil {
		t.Errorf("403 body must carry code+window+occurred_at, got %+v", eb)
	}
	// Get on the in-window record succeeds.
	rr = f.call(t, f.api.Get, "GET", "/api/weight:r/query/weight/"+newID, "client-x", "weight:r", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("in-window get: got %d, want 200 (%s)", rr.Code, rr.Body)
	}
	if gr, _ := recordData(t, rr.Body.Bytes()); gr.ID != newID {
		t.Errorf("in-window get returned wrong record: %+v", gr)
	}
}

// A rw client whose READ window is narrower than where it writes is warned that
// the new record landed outside its read window (saved but unreadable).
func TestWrite_OutsideReadWindow_Warns(t *testing.T) {
	f := newFixture(t)
	f.grant(t, "wx", "lifelog:write:weight")
	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) // read window starts 2026-05-01
	if err := f.grants.Create(pep.Grant{
		ID: "rg", ClientID: "wx", Operation: "read", Types: []string{"weight"}, OccurredFrom: &from,
	}); err != nil {
		t.Fatal(err)
	}

	// Writing a record BEFORE the read window → warned.
	rr := f.call(t, f.api.Create, "POST", "/api/weight:rw/records/weight", "wx", "weight:rw",
		`{"occurred_at":"2026-01-01T07:00:00+09:00","source":"t","payload":{"weight_kg":80}}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: got %d (%s)", rr.Code, rr.Body)
	}
	_, meta := recordData(t, rr.Body.Bytes())
	if !hasWarning(meta.Warnings, wire.CodeWrittenOutsideReadWindow) {
		t.Errorf("past write should warn written_outside_read_window, got %+v", meta.Warnings)
	}

	// Writing a record INSIDE the read window → no warning.
	rr = f.call(t, f.api.Create, "POST", "/api/weight:rw/records/weight", "wx", "weight:rw",
		`{"occurred_at":"2026-06-01T07:00:00+09:00","source":"t","payload":{"weight_kg":79}}`)
	_, meta = recordData(t, rr.Body.Bytes())
	if hasWarning(meta.Warnings, wire.CodeWrittenOutsideReadWindow) {
		t.Errorf("in-window write must not warn, got %+v", meta.Warnings)
	}
}

func TestUnknownType_Rejected(t *testing.T) {
	f := newFixture(t)
	// A wildcard capability + wildcard grant: capability and PEP both pass, so
	// only the known-type gate can stop an unknown/traversal-shaped type. (Writes
	// were already protected by the validator; this closes the read parity gap.)
	f.grant(t, "client-x", "lifelog:read:*", "lifelog:write:*")

	for _, typ := range []string{"nonexistent", "..", ".", "etc"} {
		rr := f.call(t, f.api.List, "GET", "/api/*:r/query/"+typ, "client-x", "*:r", "")
		if rr.Code != http.StatusNotFound {
			t.Errorf("read unknown type %q: got %d, want 404 (%s)", typ, rr.Code, rr.Body)
		}
	}
	// Write of an unknown type is likewise refused (404 at the gate, before the
	// validator would also reject it).
	rr := f.call(t, f.api.Create, "POST", "/api/*:rw/records/nonexistent", "client-x", "*:rw",
		`{"occurred_at":"2026-05-28T07:00:00+09:00","source":"t","payload":{}}`)
	if rr.Code != http.StatusNotFound {
		t.Errorf("write unknown type: got %d, want 404 (%s)", rr.Code, rr.Body)
	}
}

func TestDelete_RequiresWriteGrant(t *testing.T) {
	f := newFixture(t)
	id := f.seedWeight(t, "2026-05-28T07:00:00+09:00", 70.5)

	// Read grant only → delete (a write) is refused.
	f.grant(t, "client-x", "lifelog:read:weight")
	rr := f.call(t, f.api.Delete, "DELETE", "/api/weight:rw/records/weight/"+id, "client-x", "weight:rw", "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("delete with read-only grant: got %d, want 403 (%s)", rr.Code, rr.Body)
	}

	// Add write → delete succeeds (204).
	f.grant(t, "client-x", "lifelog:write:weight")
	rr = f.call(t, f.api.Delete, "DELETE", "/api/weight:rw/records/weight/"+id, "client-x", "weight:rw", "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete with write grant: got %d, want 204 (%s)", rr.Code, rr.Body)
	}
}

func TestJSONErrors_RewritesPlainTextError(t *testing.T) {
	// Mimic the bearer middleware / router guard: a plain-text http.Error with a
	// WWW-Authenticate header (the 401 case).
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="x"`)
		http.Error(w, "invalid token", http.StatusUnauthorized)
	})
	rr := httptest.NewRecorder()
	JSONErrors(inner).ServeHTTP(rr, httptest.NewRequest("GET", "/api/x", nil))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
	if rr.Header().Get("WWW-Authenticate") == "" {
		t.Error("WWW-Authenticate header was dropped")
	}
	eb := errorBody(t, rr.Body.Bytes())
	if eb.Message != "invalid token" {
		t.Errorf("error message = %q, want %q", eb.Message, "invalid token")
	}
	if eb.Code != wire.CodeUnauthenticated {
		t.Errorf("error code = %q, want %q", eb.Code, wire.CodeUnauthenticated)
	}
}

func TestJSONErrors_PassesThroughJSONAndSuccess(t *testing.T) {
	// An already-JSON error body is not mutated.
	jsonErr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	})
	rr := httptest.NewRecorder()
	JSONErrors(jsonErr).ServeHTTP(rr, httptest.NewRequest("GET", "/api/x", nil))
	if rr.Code != http.StatusForbidden || rr.Body.String() != `{"error":"nope"}` {
		t.Errorf("JSON error mutated: %d %q", rr.Code, rr.Body.String())
	}

	// A success body passes through byte-for-byte.
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[1,2,3]`))
	})
	rr2 := httptest.NewRecorder()
	JSONErrors(ok).ServeHTTP(rr2, httptest.NewRequest("GET", "/api/x", nil))
	if rr2.Code != http.StatusOK || rr2.Body.String() != `[1,2,3]` {
		t.Errorf("success mutated: %d %q", rr2.Code, rr2.Body.String())
	}
}

func TestRevocationIsImmediate(t *testing.T) {
	f := newFixture(t)
	f.seedWeight(t, "2026-05-28T07:00:00+09:00", 70.5)
	f.grant(t, "client-x", "lifelog:read:weight")

	rr := f.call(t, f.api.List, "GET", "/api/weight:r/query/weight", "client-x", "weight:r", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("pre-revoke list: got %d", rr.Code)
	}
	if err := f.grants.RevokeAllForClient("client-x"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rr = f.call(t, f.api.List, "GET", "/api/weight:r/query/weight", "client-x", "weight:r", "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("post-revoke list: got %d, want 403 (revocation must be immediate)", rr.Code)
	}
}
