// Package owner authenticates the node's single resource owner — the person who
// approves consent. v1 uses a generated secret (not WebAuthn passkeys; see
// runtime design §8.2, decision 2026-06-04): on first run the node mints a
// high-entropy owner secret, prints it once, and stores only its hash. The owner
// logs in with that secret to obtain a browser session that gates /authorize.
//
// Because the secret is high-entropy and node-generated (not a human-chosen
// password), a plain SHA-256 is a sufficient at-rest hash — there is no
// low-entropy guessing surface to slow down.
package owner

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"html/template"
	"net/http"
	"time"

	"open-lifelog.org/node/internal/meta"
)

const sessionCookie = "olf_owner_session"

// ErrBadSecret is returned when a login secret does not match.
var ErrBadSecret = errors.New("invalid owner secret")

// Service handles owner bootstrap, login, and session checks.
type Service struct {
	db          *sql.DB
	now         func() time.Time
	sessionTTL  time.Duration
	nodeVersion string
}

func New(store *meta.Store, nodeVersion string) *Service {
	return &Service{db: store.DB(), now: time.Now, sessionTTL: 12 * time.Hour, nodeVersion: nodeVersion}
}

// EnsureSecret generates and stores the owner secret on first run. It returns
// the plaintext secret and created=true only when it had to create one (so the
// caller can print it exactly once); on subsequent runs it returns created=false.
func (s *Service) EnsureSecret() (secret string, created bool, err error) {
	var exists int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM owner_secret`).Scan(&exists); err != nil {
		return "", false, err
	}
	if exists > 0 {
		return "", false, nil
	}
	secret = randomToken()
	if _, err = s.db.Exec(
		`INSERT INTO owner_secret (id, secret_hash, created_at) VALUES (1, ?, ?)`,
		hash(secret), s.now().UTC().Format(time.RFC3339),
	); err != nil {
		return "", false, err
	}
	return secret, true, nil
}

// RotateSecret replaces the stored owner secret with a freshly generated one and
// returns the new plaintext (to print once). The old secret cannot be recovered
// — only its hash was ever stored — so rotation is the way to recover from a lost
// or compromised secret. All existing owner browser sessions are invalidated so
// the old secret cannot keep a session alive. Idempotent on a fresh DB (acts as
// first-time generation). Atomic via a transaction.
func (s *Service) RotateSecret() (string, error) {
	secret := randomToken()
	now := s.now().UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM owner_secret`); err != nil {
		return "", err
	}
	if _, err := tx.Exec(
		`INSERT INTO owner_secret (id, secret_hash, created_at) VALUES (1, ?, ?)`, hash(secret), now,
	); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`DELETE FROM owner_sessions`); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return secret, nil
}

// VerifySecret reports nil if secret matches the stored owner secret. It is the
// non-HTTP path the CLI uses to authorize owner-only operations (e.g. minting a
// personal access token), with the same constant-time check login() relies on.
func (s *Service) VerifySecret(secret string) error {
	var stored string
	if err := s.db.QueryRow(`SELECT secret_hash FROM owner_secret WHERE id=1`).Scan(&stored); err != nil {
		return ErrBadSecret
	}
	if subtle.ConstantTimeCompare([]byte(hash(secret)), []byte(stored)) != 1 {
		return ErrBadSecret
	}
	return nil
}

// login verifies the secret and, on success, creates a session and returns its
// token.
func (s *Service) login(secret string) (string, error) {
	if err := s.VerifySecret(secret); err != nil {
		return "", err
	}
	token := randomToken()
	now := s.now().UTC()
	if _, err := s.db.Exec(
		`INSERT INTO owner_sessions (session_hash, csrf_token, expires_at, created_at) VALUES (?, ?, ?, ?)`,
		hash(token), randomToken(), now.Add(s.sessionTTL).Format(time.RFC3339), now.Format(time.RFC3339),
	); err != nil {
		return "", err
	}
	return token, nil
}

// CSRFToken returns the per-session CSRF token for the request's owner session,
// or "" if there is no valid session. Embed it as a hidden field in any
// owner-driven POST form; verify with ValidateCSRF.
func (s *Service) CSRFToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	var csrf, expiresAt string
	if err := s.db.QueryRow(
		`SELECT csrf_token, expires_at FROM owner_sessions WHERE session_hash=?`, hash(c.Value),
	).Scan(&csrf, &expiresAt); err != nil {
		return ""
	}
	if exp, err := time.Parse(time.RFC3339, expiresAt); err != nil || s.now().After(exp) {
		return ""
	}
	return csrf
}

// ValidateCSRF reports whether token matches the session's CSRF token
// (constant-time). A missing session or empty token fails closed.
func (s *Service) ValidateCSRF(r *http.Request, token string) bool {
	want := s.CSRFToken(r)
	return want != "" && token != "" && subtle.ConstantTimeCompare([]byte(want), []byte(token)) == 1
}

// Authenticated reports whether the request carries a valid, unexpired owner
// session. It satisfies the interface oauth uses to gate /authorize.
func (s *Service) Authenticated(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	var expiresAt string
	if err := s.db.QueryRow(
		`SELECT expires_at FROM owner_sessions WHERE session_hash=?`, hash(c.Value),
	).Scan(&expiresAt); err != nil {
		return false
	}
	exp, err := time.Parse(time.RFC3339, expiresAt)
	return err == nil && !s.now().After(exp)
}

// Register mounts the login/logout endpoints.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", s.loginForm)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.logout)
}

var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Owner login — open-lifelog</title>
<link rel="stylesheet" href="/lifelog.css">
<style>
  .login-page { display: grid; place-items: start center; padding: 48px 20px 32px; }
  .login { width: 100%; max-width: 440px; }
  .login__brand {
    display: flex; flex-direction: column; align-items: center; gap: 12px;
    text-align: center; margin-bottom: 26px;
  }
  .login__brand .brand__mark { inline-size: 34px; block-size: 34px; border-width: 3px; }
  .login__brand .brand__mark::after { inline-size: 12px; block-size: 12px; }
  .login__brand .name { font-weight: 650; font-size: 1.15rem; letter-spacing: -0.02em; }
  .login__brand .name .brand__dot { color: var(--accent); }
  .login h1 { font-size: 1.5rem; font-weight: 660; text-align: center; }
  .login__sub { text-align: center; color: var(--ink-soft); margin-top: 6px; font-size: 0.95rem; }
  .login .field { margin-top: 8px; }
  #secret { font-family: var(--mono); letter-spacing: 0.02em; padding-block: 13px; }
  .login__foot { text-align: center; margin-top: 20px; font-size: 0.85rem; color: var(--muted); }
  .login__foot code { color: var(--ink-soft); }
  .errorbox { display: none; margin-bottom: 16px; }
  body.is-error .errorbox { display: flex; }
</style>
</head>
<body{{if .Error}} class="is-error"{{end}}>

<header class="site">
  <div class="site__in">
    <a class="brand" href="/">
      <span class="brand__mark" aria-hidden="true"></span>
      <span class="brand__name">open<span class="brand__dot">·</span>lifelog</span>
    </a>
  </div>
</header>

<main>
  <div class="login-page">
    <div class="login">
      <div class="login__brand">
        <span class="brand__mark" aria-hidden="true"></span>
        <span class="name">open<span class="brand__dot">·</span>lifelog</span>
      </div>

      <div class="card">
        <div class="card__bd">
          <h1>Owner login</h1>
          <p class="login__sub">Sign in to manage access to your node.</p>

          <div class="alert errorbox" role="alert">
            <span class="alert__icon" aria-hidden="true">
              <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M12 8v5M12 16h.01"/></svg>
            </span>
            <span>Incorrect secret.</span>
          </div>

          <form method="post" action="/login">
            <div class="field">
              <label for="secret">Owner secret</label>
              <input
                type="password"
                id="secret"
                name="secret"
                class="input input--mono"
                autocomplete="off"
                autocapitalize="off"
                autocorrect="off"
                spellcheck="false"
                autofocus
                aria-describedby="secret-hint"
                placeholder="paste your node secret">
              <p class="hint" id="secret-hint">
                The long secret your node printed at first run. Paste it — it isn't a password you'd remember.
              </p>
            </div>
            <input type="hidden" name="return" value="{{.Return}}">
            <button class="btn btn--primary btn--lg btn--block" type="submit" style="margin-top:8px;">Log in</button>
          </form>
        </div>
      </div>

      <p class="login__foot">
        Lost the secret? Rotate it from the CLI — <code class="mono">olf secret rotate</code>.
      </p>
    </div>
  </div>
</main>

<footer class="site-foot">
  <div class="site-foot__in">
    <span class="mono">open-lifelog node — {{.NodeVersion}}</span>
    <span aria-hidden="true">·</span>
    <span>Self-hosted. Your data stays on this node.</span>
  </div>
</footer>
</body>
</html>`))

func (s *Service) loginForm(w http.ResponseWriter, r *http.Request) {
	s.renderLogin(w, safeReturn(r.URL.Query().Get("return")), "")
}

func (s *Service) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ret := safeReturn(r.PostForm.Get("return"))
	token, err := s.login(r.PostForm.Get("secret"))
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		s.renderLogin(w, ret, "Incorrect secret.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  s.now().Add(s.sessionTTL),
	})
	http.Redirect(w, r, ret, http.StatusFound)
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_, _ = s.db.Exec(`DELETE FROM owner_sessions WHERE session_hash=?`, hash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Service) renderLogin(w http.ResponseWriter, ret, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = loginTmpl.Execute(w, map[string]string{"Return": ret, "Error": errMsg, "NodeVersion": s.nodeVersion})
}

// safeReturn keeps post-login redirects local (no open redirect): only a path
// beginning with a single "/" is allowed, else fall back to "/".
func safeReturn(ret string) string {
	if len(ret) == 0 || ret[0] != '/' || (len(ret) > 1 && ret[1] == '/') {
		return "/"
	}
	return ret
}

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("owner: out of randomness: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func hash(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}
