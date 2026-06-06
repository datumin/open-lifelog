package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"open-lifelog.org/node/internal/meta"
	"open-lifelog.org/node/internal/pep"
)

// fakeLink + fakeLinks satisfy the oauth.Links interface for tests.
type fakeLink struct{ scopes []string }

func (f fakeLink) Scopes() []string { return f.scopes }

type fakeLinks map[string]fakeLink

func (m fakeLinks) Lookup(id string) Link {
	l, ok := m[id]
	if !ok {
		return nil
	}
	return l
}

// fakeOwner is a stand-in for the owner package's session + CSRF checks.
type fakeOwner struct {
	authed bool
	csrf   string
}

func (f *fakeOwner) Authenticated(*http.Request) bool { return f.authed }
func (f *fakeOwner) CSRFToken(*http.Request) string {
	if f.authed {
		return f.csrf
	}
	return ""
}
func (f *fakeOwner) ValidateCSRF(_ *http.Request, t string) bool {
	return f.authed && t != "" && t == f.csrf
}

type harness struct {
	srv    *Server
	owner  *fakeOwner
	grants *pep.Store
	ts     *httptest.Server
	client *http.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	fo := &fakeOwner{authed: true, csrf: "csrf-token-123"}
	grants := pep.New(store)
	mux := http.NewServeMux()
	// Known links, addressed by their capability string.
	fl := fakeLinks{
		"meal:w": {scopes: []string{"lifelog:write:meal"}},
		"meal:r": {scopes: []string{"lifelog:read:meal"}},
	}
	// Tests pin the node tz to UTC so window date assertions are offset-free.
	s := New(store, "http://example.test", fo, grants, fl, []string{"weight", "meal", "sleep", "steps"}, time.UTC)
	s.Register(mux)
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ti := auth.TokenInfoFromContext(r.Context())
		_, _ = io.WriteString(w, "ok:"+ti.UserID)
	})
	mux.Handle("/mcp", s.RequireToken("mcp", echo))
	mux.Handle("/mcp/{linkID}", s.RequireToken("mcp", echo))
	// The REST surface shares the same bearer protection, on the /api segment.
	mux.Handle("/api/{linkID}/query/{type}", s.RequireToken("api", echo))

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	s.baseURL = ts.URL // self-consistent metadata URLs

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return &harness{srv: s, owner: fo, grants: grants, ts: ts, client: client}
}

func (h *harness) registerClient(t *testing.T, redirectURI string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"client_name":   "test-client",
		"redirect_uris": []string{redirectURI},
	})
	resp, err := http.Post(h.ts.URL+"/register", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status %d", resp.StatusCode)
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.ClientID == "" {
		t.Fatal("register returned empty client_id")
	}
	return out.ClientID
}

func pkce() (verifier, challenge string) {
	verifier = "this-is-a-sufficiently-long-code-verifier-1234567890"
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func authParams(clientID, redirectURI, challenge string) url.Values {
	v := url.Values{}
	v.Set("response_type", "code")
	v.Set("client_id", clientID)
	v.Set("redirect_uri", redirectURI)
	v.Set("code_challenge", challenge)
	v.Set("code_challenge_method", "S256")
	v.Set("state", "xyz")
	v.Set("scope", "lifelog:read:weight")
	return v
}

// approve posts the consent decision (with CSRF token) and returns the redirect.
func (h *harness) approve(t *testing.T, clientID, redirectURI, challenge string) url.Values {
	t.Helper()
	form := authParams(clientID, redirectURI, challenge)
	form.Set("action", "approve")
	form.Set("csrf_token", h.owner.csrf)
	// Approve everything that was requested (full grant).
	for _, sc := range strings.Fields(form.Get("scope")) {
		form.Add("grant", sc)
	}
	resp, err := h.client.Post(h.ts.URL+"/authorize", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("approve status %d, want 302", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	return loc.Query()
}

func (h *harness) tokenForm(t *testing.T, form url.Values) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Post(h.ts.URL+"/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestDayWindow(t *testing.T) {
	mustT := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatalf("bad test time %q: %v", s, err)
		}
		return tm
	}
	eq := func(got *time.Time, want string) bool {
		if want == "" {
			return got == nil
		}
		return got != nil && got.Equal(mustT(want))
	}

	t.Run("empty is unrestricted", func(t *testing.T) {
		w, err := dayWindow("", "", time.UTC)
		if err != nil || w.From != nil || w.To != nil {
			t.Fatalf("got %+v err=%v, want zero window", w, err)
		}
	})
	t.Run("UTC: from is start of UTC day", func(t *testing.T) {
		w, err := dayWindow("2026-01-01", "", time.UTC)
		if err != nil || !eq(w.From, "2026-01-01T00:00:00Z") || w.To != nil {
			t.Fatalf("got %+v err=%v", w, err)
		}
	})
	t.Run("UTC: to is inclusive end of UTC day", func(t *testing.T) {
		w, err := dayWindow("", "2026-01-31", time.UTC)
		if err != nil || w.From != nil || !eq(w.To, "2026-01-31T23:59:59.999999999Z") {
			t.Fatalf("got %+v err=%v", w, err)
		}
	})
	t.Run("from after to is an error", func(t *testing.T) {
		if _, err := dayWindow("2026-02-01", "2026-01-01", time.UTC); err == nil {
			t.Fatal("expected error when from > to")
		}
	})
	t.Run("malformed date is an error (fail closed)", func(t *testing.T) {
		if _, err := dayWindow("2026/01/01", "", time.UTC); err == nil {
			t.Fatal("expected error for malformed from")
		}
	})
}

// The window's day boundaries are computed in the NODE's timezone, not UTC. For a
// JST node, a record at 02:00 JST belongs to that JST day and must be readable
// within a window set for that day — fixing the ~9h UTC blind spot.
func TestDayWindow_NodeTimezone(t *testing.T) {
	jst, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("no Asia/Tokyo tzdata")
	}
	w, err := dayWindow("2026-06-06", "2026-06-06", jst)
	if err != nil {
		t.Fatal(err)
	}
	// JST 2026-06-06 00:00 == 2026-06-05T15:00:00Z; inclusive end == 2026-06-06T14:59:59.999999999Z.
	wantFrom := time.Date(2026, 6, 5, 15, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, 6, 6, 15, 0, 0, 0, time.UTC).Add(-time.Nanosecond)
	if w.From == nil || !w.From.Equal(wantFrom) || w.To == nil || !w.To.Equal(wantTo) {
		t.Fatalf("JST window = [%v,%v], want [%v,%v]", w.From, w.To, wantFrom, wantTo)
	}
	// A 02:00 JST meal falls inside the JST-6/6 window (it would be EXCLUDED under
	// the old UTC-day interpretation).
	meal, _ := time.Parse(time.RFC3339, "2026-06-06T02:00:00+09:00")
	if !w.Contains(meal) {
		t.Errorf("02:00 JST meal must be inside the JST-day window; got from=%v meal=%v", w.From, meal.UTC())
	}
	// Sanity: under UTC the same meal would be excluded (regression guard).
	wu, _ := dayWindow("2026-06-06", "2026-06-06", time.UTC)
	if wu.Contains(meal) {
		t.Errorf("under UTC the 02:00 JST meal should be excluded (this is the bug being fixed)")
	}
}

func TestMetadataEndpoints(t *testing.T) {
	h := newHarness(t)
	resp, _ := http.Get(h.ts.URL + "/.well-known/oauth-authorization-server")
	var as map[string]any
	json.NewDecoder(resp.Body).Decode(&as)
	resp.Body.Close()
	if as["token_endpoint"] == nil || as["registration_endpoint"] == nil {
		t.Errorf("AS metadata missing endpoints: %v", as)
	}
	if methods, _ := as["code_challenge_methods_supported"].([]any); len(methods) == 0 || methods[0] != "S256" {
		t.Errorf("AS metadata must advertise S256: %v", as["code_challenge_methods_supported"])
	}

	resp, _ = http.Get(h.ts.URL + "/.well-known/oauth-protected-resource")
	var pr map[string]any
	json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()
	if pr["resource"] == nil || pr["authorization_servers"] == nil {
		t.Errorf("PR metadata incomplete: %v", pr)
	}
}

func TestAuthorizeRequiresOwnerLogin(t *testing.T) {
	h := newHarness(t)
	h.owner.authed = false
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect)
	_, challenge := pkce()

	resp, err := h.client.Get(h.ts.URL + "/authorize?" + authParams(clientID, redirect, challenge).Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected redirect to login, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Errorf("expected redirect to /login, got %q", loc)
	}
}

func TestConsentPageRenders(t *testing.T) {
	h := newHarness(t)
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect)
	_, challenge := pkce()

	resp, err := h.client.Get(h.ts.URL + "/authorize?" + authParams(clientID, redirect, challenge).Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Approve") {
		t.Fatalf("expected consent page with Approve, got %d: %s", resp.StatusCode, body)
	}
	// The consent page offers the optional data-window controls.
	for _, want := range []string{`name="data_from"`, `name="data_to"`, `type="date"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("consent page missing data-window control %q", want)
		}
	}
}

func TestDenyReturnsAccessDenied(t *testing.T) {
	h := newHarness(t)
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect)
	_, challenge := pkce()

	form := authParams(clientID, redirect, challenge)
	form.Set("action", "deny")
	form.Set("csrf_token", h.owner.csrf)
	resp, err := h.client.Post(h.ts.URL+"/authorize", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("error") != "access_denied" {
		t.Fatalf("expected access_denied, got %q", loc.RawQuery)
	}
}

func TestApproveRejectedWithoutCSRF(t *testing.T) {
	h := newHarness(t)
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect)
	_, challenge := pkce()

	form := authParams(clientID, redirect, challenge)
	form.Set("action", "approve") // no csrf_token
	resp, err := h.client.Post(h.ts.URL+"/authorize", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without CSRF token, got %d", resp.StatusCode)
	}
}

func TestApproveCreatesGrant(t *testing.T) {
	h := newHarness(t)
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect)
	_, challenge := pkce()

	// authParams requests scope "lifelog:read:weight".
	h.approve(t, clientID, redirect, challenge)

	grants, err := h.grants.List()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, g := range grants {
		if g.ClientID == clientID && g.Operation == "read" && g.Status == "active" &&
			len(g.Types) == 1 && g.Types[0] == "weight" {
			found = true
		}
	}
	if !found {
		t.Fatalf("approval did not create the expected read:weight grant: %+v", grants)
	}
}

// The owner can bound the read grant's data window at consent time; the window
// is applied to the read grant only and flows into the authorization decision.
func TestApproveWithDataWindow(t *testing.T) {
	h := newHarness(t)
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect)
	_, challenge := pkce()

	form := authParams(clientID, redirect, challenge) // scope lifelog:read:weight
	form.Set("scope", "lifelog:read:weight lifelog:write:weight")
	form.Set("action", "approve")
	form.Set("csrf_token", h.owner.csrf)
	form.Add("grant", "lifelog:read:weight")
	form.Add("grant", "lifelog:write:weight")
	form.Set("data_from", "2026-01-01")
	form.Set("data_to", "2026-01-31")

	resp := h.postAuthorize(t, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("approve status %d, want 302", resp.StatusCode)
	}

	wantFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC).Add(-time.Nanosecond)
	grants, _ := h.grants.List()
	var read, write *pep.Grant
	for i := range grants {
		if grants[i].ClientID != clientID || grants[i].Status != "active" {
			continue
		}
		switch grants[i].Operation {
		case "read":
			read = &grants[i]
		case "write":
			write = &grants[i]
		}
	}
	if read == nil || write == nil {
		t.Fatalf("expected read+write grants, got %+v", grants)
	}
	if read.OccurredFrom == nil || !read.OccurredFrom.Equal(wantFrom) || read.OccurredTo == nil || !read.OccurredTo.Equal(wantTo) {
		t.Errorf("read window = [%v,%v], want [%v,%v]", read.OccurredFrom, read.OccurredTo, wantFrom, wantTo)
	}
	if write.OccurredFrom != nil || write.OccurredTo != nil {
		t.Errorf("write grant must stay unrestricted, got [%v,%v]", write.OccurredFrom, write.OccurredTo)
	}
	d, _ := h.grants.Authorize(clientID, "read", "weight")
	if !d.Allowed || d.Window.From == nil || !d.Window.From.Equal(wantFrom) {
		t.Errorf("read decision window = %+v, want from %v", d.Window, wantFrom)
	}
}

// A malformed data window fails closed: no grant is created and the client is
// sent an access_denied error.
func TestApproveRejectsMalformedDataWindow(t *testing.T) {
	h := newHarness(t)
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect)
	_, challenge := pkce()

	form := authParams(clientID, redirect, challenge)
	form.Set("action", "approve")
	form.Set("csrf_token", h.owner.csrf)
	form.Add("grant", "lifelog:read:weight")
	form.Set("data_from", "01/01/2026") // malformed

	resp := h.postAuthorize(t, form)
	defer resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("error") != "access_denied" {
		t.Errorf("expected access_denied redirect, got error=%q (status %d)", loc.Query().Get("error"), resp.StatusCode)
	}
	grants, _ := h.grants.List()
	for _, g := range grants {
		if g.ClientID == clientID && g.Status == "active" {
			t.Errorf("no grant should exist after a malformed window, got %+v", g)
		}
	}
}

// postAuthorize sends a raw consent decision and returns the response.
func (h *harness) postAuthorize(t *testing.T, form url.Values) *http.Response {
	t.Helper()
	resp, err := h.client.Post(h.ts.URL+"/authorize", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestOwnerNarrowsScope(t *testing.T) {
	h := newHarness(t)
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect)
	_, challenge := pkce()

	form := url.Values{}
	form.Set("response_type", "code")
	form.Set("client_id", clientID)
	form.Set("redirect_uri", redirect)
	form.Set("code_challenge", challenge)
	form.Set("code_challenge_method", "S256")
	form.Set("scope", "lifelog:read:*") // app asks for ALL reads
	form.Set("action", "approve")
	form.Set("csrf_token", h.owner.csrf)
	form.Add("grant", "lifelog:read:meal") // owner grants ONLY meal

	resp := h.postAuthorize(t, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") == "" {
		t.Fatalf("expected redirect with code, got %d", resp.StatusCode)
	}

	grants, _ := h.grants.List()
	var readGrants int
	for _, g := range grants {
		if g.ClientID == clientID && g.Status == "active" && g.Operation == "read" {
			readGrants++
			if len(g.Types) != 1 || g.Types[0] != "meal" {
				t.Errorf("expected read grant narrowed to [meal], got %v", g.Types)
			}
		}
	}
	if readGrants != 1 {
		t.Fatalf("expected exactly one narrowed read grant, got %d", readGrants)
	}
	// The narrowed token must NOT carry weight access.
	if d, _ := h.grants.Authorize(clientID, "read", "weight"); d.Allowed {
		t.Error("read:weight must be denied — the owner only granted meal")
	}
}

func TestCannotEscalateBeyondRequest(t *testing.T) {
	h := newHarness(t)
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect)
	_, challenge := pkce()

	form := url.Values{}
	form.Set("response_type", "code")
	form.Set("client_id", clientID)
	form.Set("redirect_uri", redirect)
	form.Set("code_challenge", challenge)
	form.Set("code_challenge_method", "S256")
	form.Set("scope", "lifelog:read:meal") // app only asked for read:meal
	form.Set("action", "approve")
	form.Set("csrf_token", h.owner.csrf)
	form.Add("grant", "lifelog:write:weight") // tamper: not in the request

	resp := h.postAuthorize(t, form)
	defer resp.Body.Close()
	// The only "grant" is outside the request, so nothing is granted → denied.
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("error") != "access_denied" {
		t.Fatalf("escalation attempt should be denied, got %q", loc.RawQuery)
	}
	if d, _ := h.grants.Authorize(clientID, "write", "weight"); d.Allowed {
		t.Error("must not grant a permission the client never requested")
	}
}

func TestFullAuthCodeFlow(t *testing.T) {
	h := newHarness(t)
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect)
	verifier, challenge := pkce()

	q := h.approve(t, clientID, redirect, challenge)
	if q.Get("state") != "xyz" {
		t.Errorf("state not echoed: %q", q.Get("state"))
	}
	code := q.Get("code")
	if code == "" {
		t.Fatalf("no code in redirect: %v", q)
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirect)
	form.Set("client_id", clientID)
	form.Set("code_verifier", verifier)
	resp, out := h.tokenForm(t, form)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token status %d: %v", resp.StatusCode, out)
	}
	access, _ := out["access_token"].(string)
	refresh, _ := out["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("missing tokens: %v", out)
	}

	req, _ := http.NewRequest("GET", h.ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	mResp, _ := h.client.Do(req)
	body, _ := io.ReadAll(mResp.Body)
	mResp.Body.Close()
	if mResp.StatusCode != http.StatusOK || !strings.HasPrefix(string(body), "ok:"+clientID) {
		t.Fatalf("protected /mcp: status %d body %q", mResp.StatusCode, body)
	}

	resp2, _ := h.tokenForm(t, form)
	if resp2.StatusCode == http.StatusOK {
		t.Error("authorization code was replayable")
	}

	rf := url.Values{}
	rf.Set("grant_type", "refresh_token")
	rf.Set("refresh_token", refresh)
	rf.Set("client_id", clientID)
	rResp, rOut := h.tokenForm(t, rf)
	if rResp.StatusCode != http.StatusOK || rOut["access_token"] == "" {
		t.Fatalf("refresh failed: %d %v", rResp.StatusCode, rOut)
	}
}

func TestPKCEMismatchRejected(t *testing.T) {
	h := newHarness(t)
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect)
	_, challenge := pkce()
	code := h.approve(t, clientID, redirect, challenge).Get("code")

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirect)
	form.Set("client_id", clientID)
	form.Set("code_verifier", "the-wrong-verifier")
	resp, out := h.tokenForm(t, form)
	if resp.StatusCode != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("expected invalid_grant for bad PKCE, got %d %v", resp.StatusCode, out)
	}
}

func TestAuthorizeRejectsUnregisteredRedirect(t *testing.T) {
	h := newHarness(t)
	clientID := h.registerClient(t, "http://app.test/callback")
	_, challenge := pkce()

	v := authParams(clientID, "http://evil.test/steal", challenge)
	resp, err := h.client.Get(h.ts.URL + "/authorize?" + v.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for unregistered redirect_uri, got %d", resp.StatusCode)
	}
}

func TestGrantsListShowsClientName(t *testing.T) {
	h := newHarness(t)
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect) // registers with client_name "test-client"
	_, challenge := pkce()
	h.approve(t, clientID, redirect, challenge) // creates a grant

	resp, err := h.client.Get(h.ts.URL + "/grants")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "test-client") {
		t.Errorf("/grants should show the client name, got: %s", body)
	}
}

func TestClientDetailSaveManagesGrants(t *testing.T) {
	h := newHarness(t)
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect)
	_, challenge := pkce()
	h.approve(t, clientID, redirect, challenge) // initial grant: read:weight

	if d, _ := h.grants.Authorize(clientID, "read", "weight"); !d.Allowed {
		t.Fatal("precondition: read:weight should be granted")
	}

	// Owner replaces the grant set with write:meal (adds a new perm the app never
	// requested, and drops read:weight) — effective immediately.
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("csrf_token", h.owner.csrf)
	form.Add("grant", "lifelog:write:meal")
	resp := h.postClientSave(t, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("save status %d, want 302", resp.StatusCode)
	}

	if d, _ := h.grants.Authorize(clientID, "write", "meal"); !d.Allowed {
		t.Error("owner-added write:meal should be allowed")
	}
	if d, _ := h.grants.Authorize(clientID, "read", "weight"); d.Allowed {
		t.Error("dropped read:weight should now be denied")
	}
}

func (h *harness) postClientSave(t *testing.T, form url.Values) *http.Response {
	t.Helper()
	resp, err := h.client.Post(h.ts.URL+"/grants/client", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// readGrantWindow returns a client's active read-grant window for assertions.
func (h *harness) readGrantWindow(t *testing.T, clientID string) pep.Window {
	t.Helper()
	w, err := h.grants.ReadWindowFor(clientID)
	if err != nil {
		t.Fatalf("ReadWindowFor: %v", err)
	}
	return w
}

// seedWindowedRead gives clientID a read grant bounded to [from,to] dates.
func (h *harness) seedWindowedRead(t *testing.T, clientID, from, to string) {
	t.Helper()
	w, err := dayWindow(from, to, time.UTC)
	if err != nil {
		t.Fatalf("dayWindow: %v", err)
	}
	n := 0
	if err := h.grants.RevokeAllForClient(clientID); err != nil {
		t.Fatal(err)
	}
	if err := h.grants.CreateFromScopes(clientID, []string{"lifelog:read:weight"}, w, func() string { n++; return "seed-" + string(rune('0'+n)) }); err != nil {
		t.Fatal(err)
	}
}

// The dashboard surfaces an existing read window as prefilled, editable date
// inputs so an unchanged Save preserves it (no silent widening).
func TestClientDetailPrefillsWindow(t *testing.T) {
	h := newHarness(t)
	clientID := h.registerClient(t, "http://app.test/callback")
	h.seedWindowedRead(t, clientID, "2026-01-01", "2026-01-31")

	resp, err := h.client.Get(h.ts.URL + "/grants/client?client_id=" + url.QueryEscape(clientID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)
	for _, want := range []string{`name="data_from"`, `name="data_to"`, `value="2026-01-01"`, `value="2026-01-31"`} {
		if !strings.Contains(page, want) {
			t.Errorf("client detail page missing %q", want)
		}
	}
}

// Saving from the dashboard applies the form's data window to the read grant
// (rather than always resetting it to unrestricted).
func TestClientSaveAppliesWindow(t *testing.T) {
	h := newHarness(t)
	clientID := h.registerClient(t, "http://app.test/callback")
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("csrf_token", h.owner.csrf)
	form.Add("grant", "lifelog:read:weight")
	form.Set("data_from", "2026-03-01")
	form.Set("data_to", "2026-03-31")
	resp := h.postClientSave(t, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("save status %d, want 302", resp.StatusCode)
	}
	w := h.readGrantWindow(t, clientID)
	wantFrom := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if w.From == nil || !w.From.Equal(wantFrom) || w.To == nil {
		t.Fatalf("dashboard save did not apply window: %+v", w)
	}
}

// A malformed window on dashboard save fails closed and does not touch the
// ledger (the prior grants survive).
func TestClientSaveRejectsMalformedWindowAndKeepsGrants(t *testing.T) {
	h := newHarness(t)
	clientID := h.registerClient(t, "http://app.test/callback")
	h.seedWindowedRead(t, clientID, "2026-01-01", "2026-01-31")

	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("csrf_token", h.owner.csrf)
	form.Add("grant", "lifelog:read:weight")
	form.Set("data_from", "03/01/2026") // malformed
	resp := h.postClientSave(t, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed window, got %d", resp.StatusCode)
	}
	// The original window must be intact (not revoked) — fail closed.
	w := h.readGrantWindow(t, clientID)
	if w.From == nil || !w.From.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("malformed save must not alter the ledger, got %+v", w)
	}
}

// The consent screen pre-fills the client's existing read window, so re-consent
// with an unchanged Approve preserves it instead of widening to unrestricted.
func TestConsentPrefillsExistingWindow(t *testing.T) {
	h := newHarness(t)
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect)
	_, challenge := pkce()
	h.seedWindowedRead(t, clientID, "2026-05-01", "2026-05-31")

	resp, err := h.client.Get(h.ts.URL + "/authorize?" + authParams(clientID, redirect, challenge).Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)
	for _, want := range []string{`value="2026-05-01"`, `value="2026-05-31"`} {
		if !strings.Contains(page, want) {
			t.Errorf("consent page did not pre-fill existing window (%q)", want)
		}
	}
}

func TestClientSaveRequiresCSRF(t *testing.T) {
	h := newHarness(t)
	clientID := h.registerClient(t, "http://app.test/callback")
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Add("grant", "lifelog:write:meal") // no csrf
	resp := h.postClientSave(t, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without CSRF, got %d", resp.StatusCode)
	}
}

// TestLinkBoundsConsent verifies that when /authorize is hit with a per-link
// resource, the consent screen offers ONLY the link's surface, even if the
// client requested broader scopes.
func TestLinkBoundsConsent(t *testing.T) {
	h := newHarness(t)
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect)
	_, challenge := pkce()

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirect)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("scope", "lifelog:read:* lifelog:write:*") // app wants everything
	q.Set("resource", h.ts.URL+"/mcp/meal:w")        // but link is meal-write only
	q.Set("state", "xyz")

	resp, err := h.client.Get(h.ts.URL + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)

	// Only meal write should appear as a checkbox candidate.
	if !strings.Contains(page, `value="lifelog:write:meal"`) {
		t.Errorf("expected write:meal candidate in consent page: %s", page)
	}
	for _, banned := range []string{
		`value="lifelog:read:meal"`,
		`value="lifelog:write:weight"`,
		`value="lifelog:read:weight"`,
	} {
		if strings.Contains(page, banned) {
			t.Errorf("link-bounded consent must not offer %s", banned)
		}
	}
}

func TestLinkUnknownIsRejected(t *testing.T) {
	h := newHarness(t)
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect)
	_, challenge := pkce()

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirect)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("scope", "lifelog:write:meal")
	q.Set("resource", h.ts.URL+"/mcp/nonexistent")
	q.Set("state", "s")

	resp, err := h.client.Get(h.ts.URL + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("error") != "invalid_target" {
		t.Fatalf("unknown link should return invalid_target, got %q", loc.RawQuery)
	}
}

func TestHomeShowsDashboardLinksWhenLoggedIn(t *testing.T) {
	h := newHarness(t) // fakeOwner is authed by default
	resp, err := h.client.Get(h.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)
	for _, want := range []string{`href="/grants"`, `href="/links"`, "Log out"} {
		if !strings.Contains(page, want) {
			t.Errorf("logged-in home missing %q", want)
		}
	}
}

func TestHomeShowsLoginWhenAnonymous(t *testing.T) {
	h := newHarness(t)
	h.owner.authed = false
	resp, err := h.client.Get(h.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)
	if !strings.Contains(page, `href="/login"`) {
		t.Errorf("anonymous home should link to /login: %s", page)
	}
	if strings.Contains(page, `href="/grants"`) {
		t.Errorf("anonymous home must NOT leak dashboard links")
	}
}

// tokenFor runs the full auth-code flow (with a resource = audience) and
// returns an access token bound to that resource with the granted scopes.
func (h *harness) tokenFor(t *testing.T, clientID, redirect, resource string, scopes ...string) string {
	t.Helper()
	verifier, challenge := pkce()
	form := url.Values{}
	form.Set("response_type", "code")
	form.Set("client_id", clientID)
	form.Set("redirect_uri", redirect)
	form.Set("code_challenge", challenge)
	form.Set("code_challenge_method", "S256")
	form.Set("scope", strings.Join(scopes, " "))
	if resource != "" {
		form.Set("resource", resource)
	}
	form.Set("action", "approve")
	form.Set("csrf_token", h.owner.csrf)
	for _, sc := range scopes {
		form.Add("grant", sc)
	}
	resp := h.postAuthorize(t, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("authorize: got %d (%s)", resp.StatusCode, body)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in redirect: %s", loc.RawQuery)
	}
	tf := url.Values{}
	tf.Set("grant_type", "authorization_code")
	tf.Set("code", code)
	tf.Set("redirect_uri", redirect)
	tf.Set("client_id", clientID)
	tf.Set("code_verifier", verifier)
	tresp, out := h.tokenForm(t, tf)
	if tresp.StatusCode != http.StatusOK {
		t.Fatalf("token: got %d %v", tresp.StatusCode, out)
	}
	access, _ := out["access_token"].(string)
	if access == "" {
		t.Fatalf("no access token: %v", out)
	}
	return access
}

// getWithToken issues a GET to path with a Bearer token and returns status.
func (h *harness) getWithToken(t *testing.T, path, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", h.ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestAPISurfaceMetadata(t *testing.T) {
	h := newHarness(t)
	// Per-capability /api/meal:w resource metadata (meal:w is a known link). The
	// REST surface is always capability-scoped, so this is the only /api form.
	resp, _ := http.Get(h.ts.URL + "/.well-known/oauth-protected-resource/api/meal:w")
	var pr map[string]any
	json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()
	if pr["resource"] != h.ts.URL+"/api/meal:w" {
		t.Errorf("scoped api PR metadata resource = %v, want %s/api/meal:w", pr["resource"], h.ts.URL)
	}
	// An unknown capability must 404 (do not advertise a resource we won't serve).
	resp, _ = http.Get(h.ts.URL + "/.well-known/oauth-protected-resource/api/nope")
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusNotFound {
		t.Errorf("unknown api capability metadata: got %d, want 404", code)
	}
}

func TestAPITokenReachesAPIEndpoint(t *testing.T) {
	h := newHarness(t)
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect)
	token := h.tokenFor(t, clientID, redirect, h.ts.URL+"/api/meal:w", "lifelog:write:meal")

	code, body := h.getWithToken(t, "/api/meal:w/query/meal", token)
	if code != http.StatusOK || !strings.HasPrefix(body, "ok:"+clientID) {
		t.Fatalf("api token at api endpoint: got %d %q", code, body)
	}
}

// TestCrossSurfaceTokenRejected is the crux of the two-surface design: a token
// minted for the REST /api surface must NOT be replayable at the matching MCP
// endpoint, and vice versa — the surface segment is part of the audience.
func TestCrossSurfaceTokenRejected(t *testing.T) {
	h := newHarness(t)
	const redirect = "http://app.test/callback"
	clientID := h.registerClient(t, redirect)

	apiToken := h.tokenFor(t, clientID, redirect, h.ts.URL+"/api/meal:w", "lifelog:write:meal")
	// The /api token presented at the /mcp endpoint of the same capability → 401.
	if code, _ := h.getWithToken(t, "/mcp/meal:w", apiToken); code != http.StatusUnauthorized {
		t.Errorf("api token at /mcp/meal:w: got %d, want 401", code)
	}

	mcpToken := h.tokenFor(t, clientID, redirect, h.ts.URL+"/mcp/meal:w", "lifelog:write:meal")
	// The /mcp token presented at the /api endpoint → 401.
	if code, _ := h.getWithToken(t, "/api/meal:w/query/meal", mcpToken); code != http.StatusUnauthorized {
		t.Errorf("mcp token at /api/meal:w: got %d, want 401", code)
	}
}

func TestAPIRequiresBearer(t *testing.T) {
	h := newHarness(t)
	resp, err := h.client.Get(h.ts.URL + "/api/meal:w/query/meal")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}
	if wa := resp.Header.Get("WWW-Authenticate"); !strings.Contains(wa, "/api/meal:w") {
		t.Errorf("401 must point at the api resource metadata, got %q", wa)
	}
}

func TestIssueOwnerToken(t *testing.T) {
	h := newHarness(t)
	// meal:w is the harness's one known link (scopes lifelog:write:meal).
	tok, clientID, err := h.srv.IssueOwnerToken("cli-token", "api", "meal:w", time.Hour)
	if err != nil {
		t.Fatalf("IssueOwnerToken: %v", err)
	}
	if tok == "" || clientID == "" {
		t.Fatal("empty token or client id")
	}

	// 1. It wrote a real grant, so the ledger authorizes the capability.
	if d, _ := h.grants.Authorize(clientID, "write", "meal"); !d.Allowed {
		t.Error("expected write:meal grant from the issued token")
	}
	// 2. It shows up in the owner dashboard under its name.
	clients, err := h.srv.clientsWithGrants()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range clients {
		if c.ID == clientID && c.Name == "cli-token" {
			found = true
		}
	}
	if !found {
		t.Errorf("issued token's client not listed in dashboard: %+v", clients)
	}
	// 3. The token is accepted at its bound resource (/api/meal:w) ...
	if code, body := h.getWithToken(t, "/api/meal:w/query/meal", tok); code != http.StatusOK || !strings.HasPrefix(body, "ok:"+clientID) {
		t.Errorf("token rejected at its own resource: %d %q", code, body)
	}
	// ... and rejected at the matching MCP endpoint (audience binding).
	if code, _ := h.getWithToken(t, "/mcp/meal:w", tok); code != http.StatusUnauthorized {
		t.Errorf("api token must not work at /mcp/meal:w, got %d", code)
	}
	// 4. Re-running with the same name reuses one client (no dashboard pile-up).
	_, clientID2, err := h.srv.IssueOwnerToken("cli-token", "api", "meal:w", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if clientID2 != clientID {
		t.Errorf("same name should reuse client: %q vs %q", clientID, clientID2)
	}
	// 5. Revoking from the ledger (as the dashboard does) disables it immediately.
	if err := h.grants.RevokeAllForClient(clientID); err != nil {
		t.Fatal(err)
	}
	if d, _ := h.grants.Authorize(clientID, "write", "meal"); d.Allowed {
		t.Error("revoked token's grant should be denied immediately")
	}

	// Re-issuing a token for a reused client preserves a previously-set read
	// window (it must not silently widen reads to unrestricted).
	_, rc, err := h.srv.IssueOwnerToken("cli-read", "api", "meal:r", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	if err := h.grants.RevokeAllForClient(rc); err != nil {
		t.Fatal(err)
	}
	win, _ := dayWindow("2026-01-01", "2026-01-31", time.UTC)
	if err := h.grants.CreateFromScopes(rc, []string{"lifelog:read:meal"}, win, func() string { n++; return "wr-" + string(rune('0'+n)) }); err != nil {
		t.Fatal(err)
	}
	if _, rc2, err := h.srv.IssueOwnerToken("cli-read", "api", "meal:r", time.Hour); err != nil || rc2 != rc {
		t.Fatalf("reissue: rc2=%q err=%v", rc2, err)
	}
	if w := h.readGrantWindow(t, rc); w.From == nil || !w.From.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("reissue must preserve the read window, got %+v", w)
	}

	// Invalid inputs are rejected.
	if _, _, err := h.srv.IssueOwnerToken("x", "api", "unknowncap", time.Hour); err == nil {
		t.Error("unknown capability should error")
	}
	if _, _, err := h.srv.IssueOwnerToken("x", "ftp", "meal:w", time.Hour); err == nil {
		t.Error("invalid surface should error")
	}
}

func TestMCPRequiresBearer(t *testing.T) {
	h := newHarness(t)
	resp, err := h.client.Get(h.ts.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}
	if wa := resp.Header.Get("WWW-Authenticate"); !strings.Contains(wa, "resource_metadata=") {
		t.Errorf("401 must point at resource metadata, got %q", wa)
	}
}
