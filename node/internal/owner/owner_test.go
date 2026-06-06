package owner

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"open-lifelog.org/node/internal/meta"
)

func newService(t *testing.T) *Service {
	t.Helper()
	store, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return New(store)
}

func TestEnsureSecretOnceThenIdempotent(t *testing.T) {
	s := newService(t)
	secret, created, err := s.EnsureSecret()
	if err != nil || !created || secret == "" {
		t.Fatalf("first EnsureSecret: secret=%q created=%v err=%v", secret, created, err)
	}
	again, created2, err := s.EnsureSecret()
	if err != nil || created2 || again != "" {
		t.Fatalf("second EnsureSecret should be a no-op: %q %v %v", again, created2, err)
	}
}

func TestLoginAndAuthenticate(t *testing.T) {
	s := newService(t)
	secret, _, _ := s.EnsureSecret()

	// Wrong secret is rejected.
	if _, err := s.login("nope"); err != ErrBadSecret {
		t.Fatalf("wrong secret: want ErrBadSecret, got %v", err)
	}

	token, err := s.login(secret)
	if err != nil || token == "" {
		t.Fatalf("login: token=%q err=%v", token, err)
	}

	// A request carrying the session cookie is authenticated.
	r := httptest.NewRequest("GET", "/authorize", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	if !s.Authenticated(r) {
		t.Error("expected authenticated request with valid session cookie")
	}

	// No cookie, or a bogus one, is not authenticated.
	if s.Authenticated(httptest.NewRequest("GET", "/authorize", nil)) {
		t.Error("expected unauthenticated without cookie")
	}
	r2 := httptest.NewRequest("GET", "/authorize", nil)
	r2.AddCookie(&http.Cookie{Name: sessionCookie, Value: "garbage"})
	if s.Authenticated(r2) {
		t.Error("expected unauthenticated with bogus cookie")
	}
}

func TestLoginEndpointSetsSessionCookie(t *testing.T) {
	s := newService(t)
	secret, _, _ := s.EnsureSecret()

	mux := http.NewServeMux()
	s.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	form := url.Values{}
	form.Set("secret", secret)
	form.Set("return", "/authorize?x=1")
	resp, err := client.Post(ts.URL+"/login", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login status %d, want 302", resp.StatusCode)
	}
	if resp.Header.Get("Location") != "/authorize?x=1" {
		t.Errorf("login did not redirect to return: %q", resp.Header.Get("Location"))
	}
	var found bool
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Error("login did not set a session cookie")
	}
}

func TestLoginWrongSecretShowsError(t *testing.T) {
	s := newService(t)
	s.EnsureSecret()

	mux := http.NewServeMux()
	s.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	form := url.Values{}
	form.Set("secret", "wrong")
	resp, err := http.Post(ts.URL+"/login", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong secret status %d, want 401", resp.StatusCode)
	}
}

func TestCSRFToken(t *testing.T) {
	s := newService(t)
	secret, _, _ := s.EnsureSecret()
	token, _ := s.login(secret)

	r := httptest.NewRequest("GET", "/grants", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})

	csrf := s.CSRFToken(r)
	if csrf == "" {
		t.Fatal("expected a CSRF token for a valid session")
	}
	if !s.ValidateCSRF(r, csrf) {
		t.Error("ValidateCSRF should accept the session's token")
	}
	if s.ValidateCSRF(r, "wrong") {
		t.Error("ValidateCSRF must reject a wrong token")
	}
	if s.ValidateCSRF(r, "") {
		t.Error("ValidateCSRF must reject an empty token")
	}

	// No session → no token, validation fails closed.
	bare := httptest.NewRequest("GET", "/grants", nil)
	if s.CSRFToken(bare) != "" || s.ValidateCSRF(bare, csrf) {
		t.Error("no-session request must have no CSRF and fail validation")
	}
}

func TestSafeReturn(t *testing.T) {
	cases := map[string]string{
		"/authorize?x=1": "/authorize?x=1",
		"":               "/",
		"http://evil/x":  "/",
		"//evil/x":       "/",
		"/ok":            "/ok",
	}
	for in, want := range cases {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVerifySecret(t *testing.T) {
	s := newService(t)
	secret, created, err := s.EnsureSecret()
	if err != nil || !created {
		t.Fatalf("EnsureSecret: %v created=%v", err, created)
	}
	if err := s.VerifySecret(secret); err != nil {
		t.Errorf("correct secret rejected: %v", err)
	}
	if err := s.VerifySecret(secret + "x"); err == nil {
		t.Error("wrong secret accepted")
	}
	if err := s.VerifySecret(""); err == nil {
		t.Error("empty secret accepted")
	}
}

func TestRotateSecret(t *testing.T) {
	s := newService(t)
	old, _, err := s.EnsureSecret()
	if err != nil {
		t.Fatalf("EnsureSecret: %v", err)
	}
	// Establish a session under the old secret.
	sess, err := s.login(old)
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	fresh, err := s.RotateSecret()
	if err != nil {
		t.Fatalf("RotateSecret: %v", err)
	}
	if fresh == old {
		t.Fatal("rotate returned the same secret")
	}
	if err := s.VerifySecret(old); err == nil {
		t.Error("old secret still valid after rotate")
	}
	if err := s.VerifySecret(fresh); err != nil {
		t.Errorf("new secret should verify: %v", err)
	}
	// The old session must be invalidated.
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess})
	if s.Authenticated(req) {
		t.Error("old session should be invalid after rotate")
	}
}
