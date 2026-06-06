// Command thirdparty is a minimal reference OAuth 2.1 client for an open-lifelog
// node. It shows exactly what a real third-party app (NOT the owner's local
// `olf token`) does to obtain and use a token:
//
//  1. Dynamic Client Registration (RFC 7591) — get a public client_id, no secret.
//  2. Authorization Code + PKCE (S256) through the owner's BROWSER consent.
//  3. Token exchange — receive an access token AND a refresh token.
//  4. An API call with the access token.
//  5. A refresh — swap the refresh token for a fresh access token.
//
// It speaks the REST surface (--surface api); MCP uses the same OAuth, only a
// different protocol after the token is obtained.
//
// Default (interactive): opens the consent screen in your browser; you log in
// with the owner secret and approve. A loopback redirect (RFC 8252,
// http://127.0.0.1:<port>/callback) catches the authorization code.
//
//	mise run build && ./olf serve --data ./dev-data &
//	mise exec -- go run ./examples/thirdparty --node http://localhost:8787 --cap meal:rw
//
// --headless: drives the owner login + consent automatically using OLF_SECRET
// (for scripted end-to-end checks without a browser).
//
//	OLF_SECRET=... mise exec -- go run ./examples/thirdparty --headless --cap meal:rw
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func main() {
	node := flag.String("node", "http://localhost:8787", "node base URL (must match the node's --base-url)")
	capability := flag.String("cap", "meal:rw", "capability to request, e.g. 'meal:rw' or 'meal:r,sleep:r'")
	surface := flag.String("surface", "api", "surface to target (this demo supports 'api')")
	headless := flag.Bool("headless", false, "drive owner login+consent via OLF_SECRET instead of a browser")
	flag.Parse()

	if *surface != "api" {
		log.Fatalf("this demo only drives the REST surface; got --surface %q", *surface)
	}
	app := &client{node: strings.TrimRight(*node, "/"), surface: *surface, cap: *capability}
	app.resource = fmt.Sprintf("%s/%s/%s", app.node, app.surface, app.cap)

	// In interactive mode the redirect_uri is a loopback listener we run; in
	// headless mode we parse the code straight from the 302 Location, so a fixed
	// (registered, strictly-matched) redirect_uri suffices without listening.
	var (
		redirectURI string
		ln          net.Listener
	)
	if *headless {
		redirectURI = "http://127.0.0.1:8989/callback"
	} else {
		var err error
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatalf("loopback listener: %v", err)
		}
		redirectURI = fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	}

	step(1, "Dynamic Client Registration (POST /register)")
	clientID := app.register(redirectURI)
	fmt.Printf("   client_id = %s  (public client — no secret)\n", clientID)

	verifier, challenge := pkce()
	state := randomString()
	ap := authParams{
		clientID: clientID, redirectURI: redirectURI, challenge: challenge,
		state: state, scope: "lifelog:read:* lifelog:write:*", resource: app.resource,
	}
	authURL := app.authorizeURL(ap)

	step(2, "Authorization Code + PKCE — owner consent")
	var code string
	if *headless {
		fmt.Println("   (headless) logging in + approving as the owner via OLF_SECRET")
		code = app.autoConsent(ap)
	} else {
		fmt.Printf("   opening the consent screen in your browser:\n   %s\n", authURL)
		code = app.browserConsent(ln, authURL, state)
	}
	fmt.Printf("   got authorization code = %s…\n", code[:min(10, len(code))])

	step(3, "Token exchange (POST /token, grant_type=authorization_code)")
	tok := app.exchangeCode(ap, code, verifier)
	fmt.Printf("   access_token  = %s…  (expires_in %v)\n", tok.Access[:min(10, len(tok.Access))], tok.ExpiresIn)
	if tok.Refresh != "" {
		fmt.Printf("   refresh_token = %s…  (third-party apps get this; `olf token` does not)\n", tok.Refresh[:min(10, len(tok.Refresh))])
	}
	fmt.Printf("   granted scope = %s\n", tok.Scope)

	step(4, "API round-trip with the access token")
	// What the app may actually do is the GRANTED scope (what the owner approved
	// on the consent screen), not what the capability URL or the app asked for.
	typ := firstReadableType(app.cap)
	switch {
	case typ == "":
		fmt.Println("   (capability has no readable type; skipping the API demo)")
	case typ != "meal":
		status, body := app.get(tok.Access, fmt.Sprintf("%s/query/%s", app.resource, typ))
		fmt.Printf("   GET /%s/%s/query/%s -> %d\n   %s\n", app.surface, app.cap, typ, status, truncate(body, 200))
	default:
		// Write a meal only if the owner granted write on meal. If the capability
		// allowed write but the owner unchecked it, demonstrate that PEP denies it.
		if scopeGrants(tok.Scope, "write", "meal") {
			id, ok := app.recordMeal(tok.Access)
			if ok {
				fmt.Printf("   POST records/meal           -> 201 created id %s\n", id)
				st, body := app.get(tok.Access, fmt.Sprintf("%s/query/meal/%s", app.resource, id))
				fmt.Printf("   GET  query/meal/%s…      -> %d\n   %s\n", id[:8], st, truncate(body, 280))
			}
		} else if capAllows(app.cap, "meal", "w") {
			fmt.Println("   write skipped: the owner approved read-only on the consent screen, so a")
			fmt.Println("                  write returns 403 (PEP). The capability allowed it; the owner didn't.")
		}
		if scopeGrants(tok.Scope, "read", "meal") {
			st, body := app.get(tok.Access, app.resource+"/query/meal")
			fmt.Printf("   GET  query/meal (list)      -> %d  (%d record(s))\n", st, countRecords(body))
		}
	}

	if tok.Refresh == "" {
		step(5, "Done (no refresh token issued)")
		return
	}
	step(5, "Refresh (POST /token, grant_type=refresh_token)")
	fresh := app.refresh(tok.Refresh, clientID)
	fmt.Printf("   new access_token = %s…\n", fresh.Access[:min(10, len(fresh.Access))])
	if typ != "" {
		status, _ := app.get(fresh.Access, fmt.Sprintf("%s/query/%s", app.resource, typ))
		fmt.Printf("   re-querying with the refreshed token -> %d\n", status)
	}
	fmt.Println("\n✓ full third-party flow OK. Revoke anytime from the owner's /grants page.")
}

// --- client ---

type client struct{ node, surface, cap, resource string }

type authParams struct {
	clientID, redirectURI, challenge, state, scope, resource string
}

type tokenResp struct {
	Access    string `json:"access_token"`
	Refresh   string `json:"refresh_token"`
	Scope     string `json:"scope"`
	ExpiresIn int    `json:"expires_in"`
}

func (c *client) register(redirectURI string) string {
	body, _ := json.Marshal(map[string]any{
		"client_name":   "thirdparty-demo",
		"redirect_uris": []string{redirectURI},
	})
	resp, err := http.Post(c.node+"/register", "application/json", strings.NewReader(string(body)))
	if err != nil {
		log.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		log.Fatalf("register: status %d: %s", resp.StatusCode, b)
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.ClientID
}

func (c *client) authorizeURL(ap authParams) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", ap.clientID)
	q.Set("redirect_uri", ap.redirectURI)
	q.Set("code_challenge", ap.challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("scope", ap.scope)
	q.Set("resource", ap.resource)
	q.Set("state", ap.state)
	return c.node + "/authorize?" + q.Encode()
}

// browserConsent opens the consent screen and waits for the loopback redirect to
// deliver the authorization code, validating that the state matches.
func (c *client) browserConsent(ln net.Listener, authURL, state string) string {
	codeCh := make(chan string, 1)
	errCh := make(chan string, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			io.WriteString(w, "Authorization failed: "+e+" — you can close this tab.")
			errCh <- e + ": " + q.Get("error_description")
			return
		}
		if q.Get("state") != state {
			io.WriteString(w, "State mismatch — possible CSRF. Close this tab.")
			errCh <- "state mismatch"
			return
		}
		io.WriteString(w, "Authorized. You can close this tab and return to the terminal.")
		codeCh <- q.Get("code")
	})}
	go srv.Serve(ln)
	defer srv.Close()

	openBrowser(authURL)
	select {
	case code := <-codeCh:
		return code
	case e := <-errCh:
		log.Fatalf("authorization denied/failed: %s", e)
	case <-time.After(5 * time.Minute):
		log.Fatal("timed out waiting for browser consent")
	}
	return ""
}

// autoConsent performs the owner login + approval over HTTP using OLF_SECRET,
// then reads the authorization code from the redirect — the same thing the
// browser would do, scripted, for headless end-to-end checks.
func (c *client) autoConsent(ap authParams) string {
	secret := os.Getenv("OLF_SECRET")
	if secret == "" {
		log.Fatal("--headless requires OLF_SECRET (the owner secret)")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		log.Fatalf("cookie jar: %v", err)
	}
	hc := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	// Owner login -> session cookie.
	if _, err := hc.PostForm(c.node+"/login", url.Values{"secret": {secret}}); err != nil {
		log.Fatalf("owner login: %v", err)
	}
	// Fetch the consent page for the CSRF token and the offered grants.
	page := httpGet(hc, c.authorizeURL(ap))
	csrf := between(page, `name="csrf_token" value="`, `"`)
	grants := allBetween(page, `name="grant" value="`, `"`)
	if csrf == "" || len(grants) == 0 {
		log.Fatalf("consent page missing csrf/grants (is the capability valid?)")
	}
	// Approve everything offered (the capability + consent already bound it).
	form := url.Values{
		"response_type": {"code"}, "client_id": {ap.clientID}, "redirect_uri": {ap.redirectURI},
		"code_challenge": {ap.challenge}, "code_challenge_method": {"S256"},
		"scope": {ap.scope}, "resource": {ap.resource}, "state": {ap.state},
		"action": {"approve"}, "csrf_token": {csrf}, "grant": grants,
	}
	resp, err := hc.PostForm(c.node+"/authorize", form)
	if err != nil {
		log.Fatalf("approve: %v", err)
	}
	defer resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc == nil || loc.Query().Get("code") == "" {
		log.Fatalf("approve did not return a code (status %d)", resp.StatusCode)
	}
	return loc.Query().Get("code")
}

func (c *client) exchangeCode(ap authParams, code, verifier string) tokenResp {
	return c.token(url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {ap.redirectURI}, "client_id": {ap.clientID}, "code_verifier": {verifier},
	})
}

func (c *client) refresh(refreshTok, clientID string) tokenResp {
	return c.token(url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refreshTok}, "client_id": {clientID},
	})
}

func (c *client) token(form url.Values) tokenResp {
	resp, err := http.PostForm(c.node+"/token", form)
	if err != nil {
		log.Fatalf("token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		log.Fatalf("token: status %d: %s", resp.StatusCode, b)
	}
	var out tokenResp
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func (c *client) get(token, fullURL string) (int, string) {
	req, _ := http.NewRequest("GET", fullURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("api get: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (c *client) post(token, fullURL, body string) (int, string) {
	req, _ := http.NewRequest("POST", fullURL, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("api post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// recordMeal writes one sample meal record and returns the node-minted id. The
// node owns id/recorded_at and defaults olf_version, so the app supplies only
// occurred_at (offset-bearing), source, and the payload. A non-201 (e.g. a 403
// because the owner didn't grant write) is reported, not fatal — the demo keeps
// going to the read step.
func (c *client) recordMeal(token string) (string, bool) {
	payload := `{"input_method":"text","raw_input":"鮭おにぎり 1個",` +
		`"total_kcal":180,"protein_g":4,"fat_g":2,"carbs_g":36,` +
		`"items":[{"name":"鮭おにぎり","kcal":180}]}`
	body := fmt.Sprintf(`{"occurred_at":%q,"source":"thirdparty-demo","payload":%s}`,
		time.Now().Format(time.RFC3339), payload)
	st, resp := c.post(token, c.resource+"/records/meal", body)
	if st != http.StatusCreated {
		fmt.Printf("   POST records/meal           -> %d  %s\n", st, truncate(resp, 120))
		return "", false
	}
	// Responses are the {data, meta} envelope; the record is under data.
	var env struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal([]byte(resp), &env)
	return env.Data.ID, true
}

// --- helpers ---

func step(n int, title string) { fmt.Printf("\n[%d] %s\n", n, title) }

func pkce() (verifier, challenge string) {
	verifier = randomString() + randomString()
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomString() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		log.Fatalf("randomness: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// firstReadableType returns a concrete type from the capability that the app may
// read (for the GET demo). A "*" segment maps to "meal" (a standard type).
func firstReadableType(capability string) string {
	for _, seg := range strings.Split(capability, ",") {
		typ, ops, ok := strings.Cut(seg, ":")
		if !ok || !strings.Contains(ops, "r") {
			continue
		}
		if typ == "*" {
			return "meal"
		}
		return typ
	}
	return ""
}

// scopeGrants reports whether the granted scope string (space-separated
// "lifelog:<op>:<type>") authorizes op ("read"/"write") on typ — the real
// authority, as approved by the owner on the consent screen.
func scopeGrants(scope, op, typ string) bool {
	want := "lifelog:" + op + ":"
	for _, s := range strings.Fields(scope) {
		if s == want+typ || s == want+"*" {
			return true
		}
	}
	return false
}

// capAllows reports whether the capability string permits op ("r"/"w") on typ.
func capAllows(capability, typ, op string) bool {
	for _, seg := range strings.Split(capability, ",") {
		t, ops, ok := strings.Cut(seg, ":")
		if ok && (t == typ || t == "*") && strings.Contains(ops, op) {
			return true
		}
	}
	return false
}

// countRecords counts the records in a {data:[…], meta:{…}} list response
// (best-effort).
func countRecords(body string) int {
	var env struct {
		Data []json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &env)
	return len(env.Data)
}

func openBrowser(u string) {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd = "explorer"
	default:
		cmd = "xdg-open"
	}
	_ = exec.Command(cmd, u).Start()
}

func httpGet(hc *http.Client, u string) string {
	resp, err := hc.Get(u)
	if err != nil {
		log.Fatalf("get %s: %v", u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i+len(start):]
	j := strings.Index(s, end)
	if j < 0 {
		return ""
	}
	return s[:j]
}

func allBetween(s, start, end string) []string {
	var out []string
	for {
		i := strings.Index(s, start)
		if i < 0 {
			return out
		}
		s = s[i+len(start):]
		j := strings.Index(s, end)
		if j < 0 {
			return out
		}
		out = append(out, s[:j])
		s = s[j+len(end):]
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
