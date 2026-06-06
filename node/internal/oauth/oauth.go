// Package oauth is the node's OAuth 2.1 authorization server and the resource
// server protection for its two external surfaces — the MCP endpoint
// (internal/mcpserver) and the REST API (internal/restapi). It implements the
// core of the MCP authorization spec: protected-resource + authorization-server
// metadata (RFC 9728 / RFC 8414), dynamic client registration (RFC 7591), the
// authorization-code grant with mandatory PKCE (S256), and refresh tokens.
//
// Both surfaces are the same kind of protected resource, distinguished only by
// a URL segment ("mcp" or "api"); that segment is part of the audience, so a
// token minted for one surface (or one capability) cannot be replayed at
// another. RequireToken(surface, …) wraps either one.
//
// The /authorize endpoint gates consent on the owner being logged in (the
// owner package) and records the owner's approved subset as grants in the PEP
// ledger. Tokens are opaque random strings, stored hashed in the metadata DB
// and validated against it — so revocation is immediate and there are no
// signing keys to manage. This matches the PEP principle that the node's ledger
// is the single source of truth.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"open-lifelog.org/node/internal/meta"
	"open-lifelog.org/node/internal/pep"
)

// OwnerAuth is the owner login + CSRF surface /authorize depends on. The owner
// package implements it; consent is gated on Authenticated, and owner-driven
// POSTs carry a per-session CSRF token.
type OwnerAuth interface {
	Authenticated(r *http.Request) bool
	CSRFToken(r *http.Request) string
	ValidateCSRF(r *http.Request, token string) bool
}

// Link is the per-link surface oauth consults to bound consent and tokens. The
// links package satisfies it; nil means the unscoped /mcp endpoint.
type Link interface {
	Scopes() []string // upper-bound scope set this link exposes
}

// Links resolves a path segment (the {capability} in /mcp/{capability}) to a
// Link, returning nil if it is not a valid scoped endpoint.
type Links interface {
	Lookup(capability string) Link
}

const loginPath = "/login"

// Server is the OAuth authorization + resource server.
type Server struct {
	db       *sql.DB
	baseURL  string // external origin, e.g. http://localhost:8787
	owner    OwnerAuth
	grants   *pep.Store
	links    Links
	types    []string // standard payload types, to expand wildcard scopes
	loc      *time.Location // node timezone; defines the read window's calendar-day boundaries
	now      func() time.Time
	codeTTL  time.Duration
	tokenTTL time.Duration
}

func New(store *meta.Store, baseURL string, owner OwnerAuth, grants *pep.Store, links Links, types []string, loc *time.Location) *Server {
	if loc == nil {
		loc = time.Local
	}
	return &Server{
		db:       store.DB(),
		baseURL:  strings.TrimRight(baseURL, "/"),
		owner:    owner,
		grants:   grants,
		links:    links,
		types:    types,
		loc:      loc,
		now:      time.Now,
		codeTTL:  time.Minute,
		tokenTTL: time.Hour,
	}
}

// Register mounts the OAuth endpoints on mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.authServerMetadata)
	// Some clients probe the OIDC discovery path; serve the same AS metadata.
	mux.HandleFunc("GET /.well-known/openid-configuration", s.authServerMetadata)
	// RFC 9728 forms the metadata URL by inserting the well-known segment before
	// the resource's path, i.e. /.well-known/oauth-protected-resource/mcp. Serve
	// the bare path, the /mcp suffix, and any per-link /mcp/{id} resource.
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.prMetadata("mcp"))
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.prMetadata("mcp"))
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp/{linkID}", s.prMetadata("mcp"))
	// The REST API (internal/restapi) is a second protected resource with the
	// same scoping; its metadata lives under the /api surface segment. The REST
	// surface is always capability-scoped (no bare /api), so only the
	// per-capability metadata path is served.
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/api/{linkID}", s.prMetadata("api"))
	mux.HandleFunc("POST /register", s.register)
	mux.HandleFunc("GET /authorize", s.authorizeForm)
	mux.HandleFunc("POST /authorize", s.authorizeDecision)
	mux.HandleFunc("POST /token", s.token)
	// Owner governance dashboard: list apps, manage each app's access.
	mux.HandleFunc("GET /grants", s.grantsList)
	mux.HandleFunc("GET /grants/client", s.clientDetail)
	mux.HandleFunc("POST /grants/client", s.clientSave)
	// Stateless capability URL builder.
	mux.HandleFunc("GET /links", s.linksBuilder)
	// Owner-facing home page (links to dashboards when logged in).
	mux.HandleFunc("GET /{$}", s.home)
}

// resourceURL is the canonical identifier of a protected resource. surface is
// the URL segment that distinguishes the two surfaces — "mcp" or "api" — so a
// token minted for one can never be replayed at the other (audience binding).
// An empty linkID denotes the un-scoped endpoint (/mcp, /api); a non-empty id
// denotes a per-capability resource at /{surface}/{id}.
func (s *Server) resourceURL(surface, linkID string) string {
	base := s.baseURL + "/" + surface
	if linkID == "" {
		return base
	}
	return base + "/" + linkID
}

func (s *Server) prMetadataURL(surface, linkID string) string {
	base := s.baseURL + "/.well-known/oauth-protected-resource/" + surface
	if linkID == "" {
		return base
	}
	return base + "/" + linkID
}

// --- metadata ---

func (s *Server) authServerMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.baseURL,
		"authorization_endpoint":                s.baseURL + "/authorize",
		"token_endpoint":                        s.baseURL + "/token",
		"registration_endpoint":                 s.baseURL + "/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{"lifelog:read:*", "lifelog:write:*", "offline_access"},
	})
}

// prMetadata serves RFC 9728 protected-resource metadata for one surface
// ("mcp" or "api"). A per-capability path that does not parse to a known link
// is 404, so clients cannot discover a resource the runtime would never serve.
func (s *Server) prMetadata(surface string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		linkID := r.PathValue("linkID")
		if linkID != "" && s.lookupLink(linkID) == nil {
			http.Error(w, "no such link", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"resource":              s.resourceURL(surface, linkID),
			"authorization_servers": []string{s.baseURL},
		})
	}
}

// lookupLink returns the link if id is known and the resolver is configured.
func (s *Server) lookupLink(id string) Link {
	if s.links == nil || id == "" {
		return nil
	}
	return s.links.Lookup(id)
}

// --- dynamic client registration (RFC 7591) ---

type registrationRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope"`
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req registrationRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid JSON")
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_redirect_uri", "at least one redirect_uri is required")
		return
	}
	// This node issues public clients only (PKCE, no secret) — the MCP client model.
	grantTypes := req.GrantTypes
	if len(grantTypes) == 0 {
		grantTypes = []string{"authorization_code", "refresh_token"}
	}

	clientID := randomToken()
	now := s.now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(
		`INSERT INTO oauth_clients
		 (client_id, client_name, redirect_uris, token_endpoint_auth_method, grant_types, scope, registered_at)
		 VALUES (?, ?, ?, ?, 'none', ?, ?)`,
		clientID, req.ClientName, jsonArray(req.RedirectURIs), jsonArray(grantTypes), req.Scope, now,
	); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"client_name":                req.ClientName,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                grantTypes,
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"scope":                      req.Scope,
		"client_id_issued_at":        s.now().Unix(),
	})
}

// --- authorization endpoint (auth code + PKCE) with owner-gated consent ---

// authRequest is the validated set of authorization parameters carried between
// the consent form and the decision POST.
type authRequest struct {
	clientID, clientName, redirectURI, scope, resource, state, challenge string
	linkID                                                               string // parsed from resource, "" for the unscoped /mcp
}

// validateAuthParams checks the client, redirect_uri, response_type, and PKCE.
// It returns the parsed request; on a client/redirect problem it writes a direct
// error (ok=false, redirected=false); on a protocol problem it redirects the
// error to the client (ok=false, redirected=true).
func (s *Server) validateAuthParams(w http.ResponseWriter, r *http.Request, v url.Values) (authRequest, bool) {
	clientID := v.Get("client_id")
	redirectURI := v.Get("redirect_uri")

	client, ok := s.lookupClient(clientID)
	if !ok {
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return authRequest{}, false
	}
	if !contains(client.redirectURIs, redirectURI) {
		http.Error(w, "redirect_uri does not match a registered URI", http.StatusBadRequest)
		return authRequest{}, false
	}

	state := v.Get("state")
	if v.Get("response_type") != "code" {
		redirectError(w, r, redirectURI, state, "unsupported_response_type", "only response_type=code is supported")
		return authRequest{}, false
	}
	challenge := v.Get("code_challenge")
	if challenge == "" || v.Get("code_challenge_method") != "S256" {
		redirectError(w, r, redirectURI, state, "invalid_request", "PKCE with code_challenge_method=S256 is required")
		return authRequest{}, false
	}
	resource := v.Get("resource")
	linkID := linkIDFromResource(resource, s.baseURL)
	if linkID != "" && s.lookupLink(linkID) == nil {
		redirectError(w, r, redirectURI, state, "invalid_target", "unknown or revoked link")
		return authRequest{}, false
	}
	return authRequest{
		clientID: clientID, clientName: client.name, redirectURI: redirectURI,
		scope: v.Get("scope"), resource: resource, state: state, challenge: challenge,
		linkID: linkID,
	}, true
}

// linkIDFromResource extracts a capability from a `resource` parameter shaped
// like "<baseURL>/{surface}/{capability}" for either surface (/mcp or /api).
// The capability is surface-independent — it bounds consent the same way
// regardless of which surface the token targets — so only the capability is
// returned here; the full resource string is what carries the audience. An
// empty resource, or a bare /mcp or /api, returns "".
func linkIDFromResource(resource, baseURL string) string {
	if resource == "" {
		return ""
	}
	base := strings.TrimRight(baseURL, "/")
	for _, surface := range []string{"/mcp", "/api"} {
		prefix := base + surface
		// Match the surface exactly or as a path prefix — guard against
		// look-alikes like "/mcp-evil" matching the "/mcp" surface.
		if resource != prefix && !strings.HasPrefix(resource, prefix+"/") {
			continue
		}
		rest := strings.TrimPrefix(resource, prefix)
		if rest == "" || rest == "/" {
			return ""
		}
		return strings.TrimPrefix(rest, "/")
	}
	return ""
}

// authorizeForm (GET /authorize) validates the request, requires the owner to be
// logged in, and renders the consent screen.
func (s *Server) authorizeForm(w http.ResponseWriter, r *http.Request) {
	req, ok := s.validateAuthParams(w, r, r.URL.Query())
	if !ok {
		return
	}
	if !s.owner.Authenticated(r) {
		// Send the owner to log in, returning here afterward.
		http.Redirect(w, r, loginPath+"?return="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	perms, offline := s.candidatePerms(req.scope, s.lookupLink(req.linkID))
	// Pre-fill the data window from the client's existing read grant so an
	// unchanged Approve preserves it rather than silently widening to unbounded.
	existing, _ := s.grants.ReadWindowFor(req.clientID)
	from, to := windowDates(existing, s.loc)
	renderConsent(w, req, perms, offline, s.owner.CSRFToken(r), from, to, s.loc.String())
}

// permView is one selectable permission on the consent screen.
type permView struct {
	Scope string // full scope, e.g. "lifelog:read:weight"
	Op    string // "read" | "write"
	Type  string
}

// candidatePerms expands a client's requested scope into the concrete (op,type)
// permissions the owner may grant, expanding wildcards across the known types.
// When link is non-nil, the result is further intersected with the link's
// surface — the link is an extra upper bound the consent screen cannot exceed.
// The owner can approve any subset of what comes back. Returns the perms and
// whether offline_access was requested.
func (s *Server) candidatePerms(requestedScope string, link Link) ([]permView, bool) {
	var perms []permView
	seen := map[string]bool{}
	offline := false

	allowed := map[string]bool{} // link's upper bound, when present
	if link != nil {
		for _, sc := range link.Scopes() {
			allowed[sc] = true
		}
	}
	add := func(op, typ string) {
		full := "lifelog:" + op + ":" + typ
		if link != nil && !allowed[full] {
			return
		}
		if !seen[full] {
			seen[full] = true
			perms = append(perms, permView{Scope: full, Op: op, Type: typ})
		}
	}
	for _, sc := range strings.Fields(requestedScope) {
		if sc == "offline_access" {
			offline = true
			continue
		}
		op, typ, ok := pep.ParseScope(sc)
		if !ok {
			continue
		}
		if typ == "*" {
			for _, t := range s.types {
				add(op, t)
			}
		} else {
			add(op, typ)
		}
	}
	return perms, offline
}

// authorizeDecision (POST /authorize) handles the owner's approve/deny. It
// re-validates everything and re-checks the owner session — the consent form is
// not trusted on its own.
func (s *Server) authorizeDecision(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !s.owner.Authenticated(r) {
		http.Redirect(w, r, loginPath, http.StatusFound)
		return
	}
	// CSRF: the consent POST must carry the session's CSRF token. This blocks a
	// forged cross-site approval even if the owner is logged in.
	if !s.owner.ValidateCSRF(r, r.PostForm.Get("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	req, ok := s.validateAuthParams(w, r, r.PostForm)
	if !ok {
		return
	}
	if r.PostForm.Get("action") != "approve" {
		redirectError(w, r, req.redirectURI, req.state, "access_denied", "the owner denied the request")
		return
	}

	// The owner approves a SUBSET of the requested permissions. Only checkboxes
	// that are genuine candidates (within the request, AND within the link if
	// the request targets a per-link resource) are honored — the form cannot
	// escalate beyond either bound.
	candidates, offline := s.candidatePerms(req.scope, s.lookupLink(req.linkID))
	allowed := map[string]bool{}
	for _, p := range candidates {
		allowed[p.Scope] = true
	}
	var granted []string
	for _, sel := range r.PostForm["grant"] {
		if allowed[sel] {
			granted = append(granted, sel)
		}
	}
	if len(granted) == 0 {
		redirectError(w, r, req.redirectURI, req.state, "access_denied", "no permissions were granted")
		return
	}

	// The owner may bound how far back/forward the client can read (the data
	// window). A malformed window fails closed: deny rather than silently grant a
	// wider window than intended.
	readWindow, err := dayWindow(r.PostForm.Get("data_from"), r.PostForm.Get("data_to"), s.loc)
	if err != nil {
		redirectError(w, r, req.redirectURI, req.state, "access_denied", "invalid data window: "+err.Error())
		return
	}

	// Record the owner's consent as grants. Re-consent replaces the client's
	// prior grants, keeping the ledger free of duplicates.
	if err := s.grants.RevokeAllForClient(req.clientID); err != nil {
		redirectError(w, r, req.redirectURI, req.state, "server_error", err.Error())
		return
	}
	if err := s.grants.CreateFromScopes(req.clientID, granted, readWindow, randomToken); err != nil {
		redirectError(w, r, req.redirectURI, req.state, "server_error", err.Error())
		return
	}

	// The granted scope (what the owner approved) — not the requested scope —
	// flows into the code and token.
	grantedScope := strings.Join(granted, " ")
	if offline {
		grantedScope += " offline_access"
	}

	code := randomToken()
	now := s.now().UTC()
	if _, err := s.db.Exec(
		`INSERT INTO auth_codes
		 (code_hash, client_id, redirect_uri, scope, resource, code_challenge, code_challenge_method, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'S256', ?, ?)`,
		hashToken(code), req.clientID, req.redirectURI, grantedScope, req.resource,
		req.challenge, now.Add(s.codeTTL).Format(time.RFC3339), now.Format(time.RFC3339),
	); err != nil {
		redirectError(w, r, req.redirectURI, req.state, "server_error", err.Error())
		return
	}

	u, _ := url.Parse(req.redirectURI)
	rq := u.Query()
	rq.Set("code", code)
	if req.state != "" {
		rq.Set("state", req.state)
	}
	u.RawQuery = rq.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

var consentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<title>open-lifelog — authorize</title>
<h1>Authorize access</h1>
<p>The app <strong>{{.ClientName}}</strong> wants access to your lifelog.
   Choose exactly what to share — uncheck anything you don't want to grant.</p>
<form method="post" action="/authorize">
  <input type="hidden" name="client_id" value="{{.ClientID}}">
  <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
  <input type="hidden" name="response_type" value="code">
  <input type="hidden" name="scope" value="{{.Scope}}">
  <input type="hidden" name="resource" value="{{.Resource}}">
  <input type="hidden" name="state" value="{{.State}}">
  <input type="hidden" name="code_challenge" value="{{.Challenge}}">
  <input type="hidden" name="code_challenge_method" value="S256">
  <input type="hidden" name="csrf_token" value="{{.CSRF}}">
  <fieldset>
    <legend>Read</legend>
    {{range .Perms}}{{if eq .Op "read"}}
    <label><input type="checkbox" name="grant" value="{{.Scope}}" checked> {{.Type}}</label><br>
    {{end}}{{end}}
  </fieldset>
  <fieldset>
    <legend>Write</legend>
    {{range .Perms}}{{if eq .Op "write"}}
    <label><input type="checkbox" name="grant" value="{{.Scope}}" checked> {{.Type}}</label><br>
    {{end}}{{end}}
  </fieldset>
  <fieldset>
    <legend>Data window (optional)</legend>
    <p style="color:#555">Limit which data this app can read, by when it occurred.
       Leave a field blank for no limit. Dates are calendar days in this node's timezone
       (<strong>{{.TZName}}</strong>) and apply to reads only.</p>
    <label>From <input type="date" name="data_from" value="{{.DataFrom}}"></label>
    <label>To <input type="date" name="data_to" value="{{.DataTo}}"></label>
  </fieldset>
  <p style="color:#555">You can revoke any of this at any time from the grants page.</p>
  <button type="submit" name="action" value="approve">Approve selected</button>
  <button type="submit" name="action" value="deny">Deny</button>
</form>`))

func renderConsent(w http.ResponseWriter, req authRequest, perms []permView, offline bool, csrf, dataFrom, dataTo, tzName string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = consentTmpl.Execute(w, map[string]any{
		"ClientName":  orDefault(req.clientName, req.clientID),
		"ClientID":    req.clientID,
		"RedirectURI": req.redirectURI,
		"Scope":       req.scope,
		"Resource":    req.resource,
		"State":       req.state,
		"Challenge":   req.challenge,
		"CSRF":        csrf,
		"Perms":       perms,
		"Offline":     offline,
		"DataFrom":    dataFrom,
		"DataTo":      dataTo,
		"TZName":      tzName,
	})
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// dayWindow converts the consent screen's optional "data from"/"data to" dates
// (YYYY-MM-DD) into a read window, interpreting the dates in the node's timezone
// loc — NOT UTC — so the owner's local calendar day is what bounds reads (a
// JST owner's "2026-06-06" means JST midnight, not UTC midnight, which would
// strand the local morning). `from` is the start of its local day; `to` is the
// inclusive end of its local day (……T23:59:59.999999999, local) so a record
// anywhere on that local day is included. The resulting bounds are true instants,
// compared offset-independently like occurred_at. An empty string leaves that
// side unbounded; a malformed date, or from after to, is an error (fail closed).
func dayWindow(from, to string, loc *time.Location) (pep.Window, error) {
	var w pep.Window
	if from != "" {
		d, err := time.ParseInLocation("2006-01-02", from, loc)
		if err != nil {
			return pep.Window{}, fmt.Errorf("data_from: %w", err)
		}
		w.From = &d
	}
	if to != "" {
		d, err := time.ParseInLocation("2006-01-02", to, loc)
		if err != nil {
			return pep.Window{}, fmt.Errorf("data_to: %w", err)
		}
		end := d.AddDate(0, 0, 1).Add(-time.Nanosecond) // inclusive end of that local day
		w.To = &end
	}
	if w.From != nil && w.To != nil && w.From.After(*w.To) {
		return pep.Window{}, fmt.Errorf("data_from %s is after data_to %s", from, to)
	}
	return w, nil
}

// windowDates renders a window's bounds back to the YYYY-MM-DD form the consent
// and dashboard date inputs use, formatted in the node's timezone loc (the same
// loc dayWindow used), so an existing window pre-fills the form to the same dates
// and an unchanged save preserves it. A nil bound yields "".
func windowDates(w pep.Window, loc *time.Location) (from, to string) {
	if w.From != nil {
		from = w.From.In(loc).Format("2006-01-02")
	}
	if w.To != nil {
		to = w.To.In(loc).Format("2006-01-02")
	}
	return from, to
}

// --- owner home page ---

var homeTmpl = template.Must(template.New("home").Parse(`<!doctype html>
<title>open-lifelog</title>
<h1>open-lifelog</h1>
<p>Your self-hosted lifelog node.</p>
{{if .Authenticated}}
<h2>Owner tools</h2>
<ul>
  <li><a href="/grants">Connected apps</a> — what's accessing your lifelog right now, manage or revoke per app.</li>
  <li><a href="/links">Capability URL builder</a> — generate scoped MCP URLs to hand to apps.</li>
  <li><form method="post" action="/logout" style="display:inline">
    <button type="submit" style="background:none;border:none;color:#06c;cursor:pointer;padding:0;font:inherit;text-decoration:underline">Log out</button>
  </form></li>
</ul>
<h2>Endpoints</h2>
<ul>
  <li>MCP (all types): <code>{{.BaseURL}}/mcp</code> — for MCP clients (Claude, ChatGPT).</li>
  <li>MCP (scoped): <code>{{.BaseURL}}/mcp/&lt;capability&gt;</code> — see the URL builder.</li>
  <li>REST API (scoped): <code>{{.BaseURL}}/api/&lt;capability&gt;</code> — for HTTP clients; OAuth-protected, same consent + PEP. Use <code>/api/*:rw</code> for all types.</li>
</ul>
{{else}}
<p><a href="/login">Log in</a> to manage app access and generate MCP capability URLs.</p>
{{end}}`))

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = homeTmpl.Execute(w, map[string]any{
		"Authenticated": s.owner.Authenticated(r),
		"BaseURL":       s.baseURL,
	})
}

// --- owner capability URL builder ---

// linksBuilderTmpl is a stateless helper: the owner ticks types × ops and the
// page shows the resulting capability URL to copy. No record is created; the
// URL itself IS the capability (à la Discord bot invites).
var linksBuilderTmpl = template.Must(template.New("links").Parse(`<!doctype html>
<title>open-lifelog — capability URL builder</title>
<h1>MCP capability URL builder</h1>
<p>Build a scoped MCP URL by picking, per type, whether the app can read it,
   write it, both, or neither. The URL itself encodes the capability — there
   is nothing to save or revoke. Anyone using the URL still has to go through
   OAuth and your consent screen; the URL is only the upper bound.</p>

<form method="get" action="/links">
  <table border="1" cellpadding="6">
    <tr><th>Type</th><th>read</th><th>write</th></tr>
    <tr>
      <td><strong>*</strong> (all)</td>
      <td><input type="checkbox" name="r" value="*" {{if index $.Read "*"}}checked{{end}}></td>
      <td><input type="checkbox" name="w" value="*" {{if index $.Write "*"}}checked{{end}}></td>
    </tr>
    {{range $t := .Types}}
    <tr>
      <td>{{$t}}</td>
      <td><input type="checkbox" name="r" value="{{$t}}" {{if index $.Read $t}}checked{{end}}></td>
      <td><input type="checkbox" name="w" value="{{$t}}" {{if index $.Write $t}}checked{{end}}></td>
    </tr>
    {{end}}
  </table>
  <p><button type="submit">Update URL</button></p>
</form>

{{if .Capability}}
<h2>Capability URLs</h2>
<p>The same capability, on either surface — hand the app whichever it speaks:</p>
<p>MCP client (Claude, ChatGPT):<br><code style="font-size:1.2em">{{.BaseURL}}/mcp/{{.Capability}}</code></p>
<p>REST/HTTP client:<br><code style="font-size:1.2em">{{.BaseURL}}/api/{{.Capability}}</code></p>
{{else}}
<p><em>Tick at least one box to generate a URL.</em></p>
{{end}}

<h2>Examples</h2>
<ul>
  <li><code>{{.BaseURL}}/mcp/meal:w</code> — write meal only (MCP)</li>
  <li><code>{{.BaseURL}}/api/meal:rw,sleep:r</code> — read+write meal, read sleep (REST)</li>
  <li><code>{{.BaseURL}}/api/*:r</code> — read everything (REST)</li>
  <li><code>{{.BaseURL}}/mcp</code> — un-scoped (offers everything during consent)</li>
</ul>
<p style="color:#555">REST paths sit under the capability:
   <code>GET /api/&lt;capability&gt;/query/&lt;type&gt;</code>,
   <code>POST /api/&lt;capability&gt;/records/&lt;type&gt;</code>, etc.</p>`))

func (s *Server) linksBuilder(w http.ResponseWriter, r *http.Request) {
	if !s.owner.Authenticated(r) {
		http.Redirect(w, r, loginPath+"?return="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	q := r.URL.Query()
	read := map[string]bool{}
	for _, t := range q["r"] {
		read[t] = true
	}
	write := map[string]bool{}
	for _, t := range q["w"] {
		write[t] = true
	}

	// Group the ticked (type, op) pairs by type into r/w/rw segments. Order:
	// "*" first (so the leading segment is the wildcard when present), then the
	// known types in their natural order.
	type seg struct{ typ, ops string }
	var segs []seg
	addSeg := func(t string) {
		ops := ""
		if read[t] {
			ops += "r"
		}
		if write[t] {
			ops += "w"
		}
		if ops != "" {
			segs = append(segs, seg{t, ops})
		}
	}
	addSeg("*")
	for _, t := range s.types {
		addSeg(t)
	}

	parts := make([]string, 0, len(segs))
	for _, sg := range segs {
		parts = append(parts, sg.typ+":"+sg.ops)
	}
	capability := strings.Join(parts, ",")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = linksBuilderTmpl.Execute(w, map[string]any{
		"BaseURL":    s.baseURL,
		"Types":      s.types,
		"Read":       read,
		"Write":      write,
		"Capability": capability,
	})
}

// --- owner grants dashboard ---

var clientsTmpl = template.Must(template.New("clients").Parse(`<!doctype html>
<title>open-lifelog — connected apps</title>
<h1>Connected apps</h1>
<p>Apps you've granted access to your lifelog. Click Manage to change or revoke.</p>
<table border="1" cellpadding="6">
  <tr><th>App</th><th>Client ID</th><th>Active grants</th><th></th></tr>
  {{range .Clients}}
  <tr>
    <td><strong>{{.Name}}</strong></td>
    <td><code>{{.ID}}</code></td>
    <td>{{.Active}}</td>
    <td><a href="/grants/client?client_id={{.ID}}">Manage</a></td>
  </tr>
  {{else}}
  <tr><td colspan="4"><em>No apps connected yet.</em></td></tr>
  {{end}}
</table>`))

var clientDetailTmpl = template.Must(template.New("clientDetail").Parse(`<!doctype html>
<title>open-lifelog — manage {{.Name}}</title>
<p><a href="/grants">&larr; all apps</a></p>
<h1>Manage access — {{.Name}}</h1>
<p><code>{{.ID}}</code></p>
<p>Tick what this app may do. Changes take effect immediately (no re-approval needed).</p>
<form method="post" action="/grants/client">
  <input type="hidden" name="client_id" value="{{.ID}}">
  <input type="hidden" name="csrf_token" value="{{.CSRF}}">
  <table border="1" cellpadding="6">
    <tr><th>Type</th><th>Read</th><th>Write</th></tr>
    {{range .Rows}}
    <tr>
      <td>{{.Type}}</td>
      <td><input type="checkbox" name="grant" value="lifelog:read:{{.Type}}" {{if .Read}}checked{{end}}></td>
      <td><input type="checkbox" name="grant" value="lifelog:write:{{.Type}}" {{if .Write}}checked{{end}}></td>
    </tr>
    {{end}}
  </table>
  <fieldset>
    <legend>Read data window (optional)</legend>
    <p style="color:#555">Limit which data this app can read, by when it occurred.
       Blank means no limit. Dates are calendar days in this node's timezone
       (<strong>{{.TZName}}</strong>) and apply to reads only.</p>
    <label>From <input type="date" name="data_from" value="{{.DataFrom}}"></label>
    <label>To <input type="date" name="data_to" value="{{.DataTo}}"></label>
  </fieldset>
  <button type="submit">Save</button>
</form>`))

// clientSummary is a row on the connected-apps page.
type clientSummary struct {
	ID, Name string
	Active   int
}

// clientsWithGrants lists every client that has appeared in the grant ledger,
// joined to its registered name.
func (s *Server) clientsWithGrants() ([]clientSummary, error) {
	rows, err := s.db.Query(`
		SELECT g.client_id, COALESCE(c.client_name, ''),
		       SUM(CASE WHEN g.status='active' THEN 1 ELSE 0 END)
		FROM grants g LEFT JOIN oauth_clients c ON c.client_id = g.client_id
		GROUP BY g.client_id
		ORDER BY MAX(g.granted_at) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []clientSummary
	for rows.Next() {
		var cs clientSummary
		if err := rows.Scan(&cs.ID, &cs.Name, &cs.Active); err != nil {
			return nil, err
		}
		if cs.Name == "" {
			cs.Name = "(unnamed app)"
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

func (s *Server) grantsList(w http.ResponseWriter, r *http.Request) {
	if !s.owner.Authenticated(r) {
		http.Redirect(w, r, loginPath+"?return="+url.QueryEscape("/grants"), http.StatusFound)
		return
	}
	clients, err := s.clientsWithGrants()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = clientsTmpl.Execute(w, map[string]any{"Clients": clients})
}

// clientRow is one type's read/write state on the detail page.
type clientRow struct {
	Type        string
	Read, Write bool
}

func (s *Server) clientDetail(w http.ResponseWriter, r *http.Request) {
	if !s.owner.Authenticated(r) {
		http.Redirect(w, r, loginPath+"?return="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	clientID := r.URL.Query().Get("client_id")
	client, ok := s.lookupClient(clientID)
	if !ok {
		http.Error(w, "unknown client", http.StatusNotFound)
		return
	}
	// The checked state reflects the live ledger (via Authorize, so wildcards and
	// expiry are honored).
	var rows []clientRow
	for _, typ := range s.types {
		read, _ := s.grants.Authorize(clientID, "read", typ)
		write, _ := s.grants.Authorize(clientID, "write", typ)
		rows = append(rows, clientRow{Type: typ, Read: read.Allowed, Write: write.Allowed})
	}
	from, to := windowDates(s.readWindowOrZero(clientID), s.loc)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = clientDetailTmpl.Execute(w, map[string]any{
		"ID":       clientID,
		"Name":     orDefault(client.name, clientID),
		"Rows":     rows,
		"CSRF":     s.owner.CSRFToken(r),
		"DataFrom": from,
		"DataTo":   to,
		"TZName":   s.loc.String(),
	})
}

// readWindowOrZero is ReadWindowFor with the error swallowed (a ledger read
// failure here just shows an empty window in the form, never panics the page).
func (s *Server) readWindowOrZero(clientID string) pep.Window {
	w, _ := s.grants.ReadWindowFor(clientID)
	return w
}

func (s *Server) clientSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !s.owner.Authenticated(r) {
		http.Redirect(w, r, loginPath, http.StatusFound)
		return
	}
	if !s.owner.ValidateCSRF(r, r.PostForm.Get("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	clientID := r.PostForm.Get("client_id")
	if _, ok := s.lookupClient(clientID); !ok {
		http.Error(w, "unknown client", http.StatusNotFound)
		return
	}
	// Accept only well-formed scopes over known types — set the ledger to match.
	known := map[string]bool{}
	for _, t := range s.types {
		known[t] = true
	}
	var selected []string
	for _, sc := range r.PostForm["grant"] {
		if _, typ, ok := pep.ParseScope(sc); ok && known[typ] {
			selected = append(selected, sc)
		}
	}
	// Parse the read window BEFORE mutating the ledger so a malformed window fails
	// closed (leaving the prior grants — and their window — intact).
	readWindow, err := dayWindow(r.PostForm.Get("data_from"), r.PostForm.Get("data_to"), s.loc)
	if err != nil {
		http.Error(w, "invalid data window: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.grants.RevokeAllForClient(clientID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(selected) > 0 {
		if err := s.grants.CreateFromScopes(clientID, selected, readWindow, randomToken); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	http.Redirect(w, r, "/grants/client?client_id="+url.QueryEscape(clientID), http.StatusFound)
}

// --- owner personal token (CLI/cron, no browser dance) ---

// IssueOwnerToken mints an access token for the owner's own use (local testing,
// scripts, cron), skipping the interactive OAuth flow but writing the SAME
// client, grants, and token rows the browser flow does. So the result is a
// first-class citizen of the dashboard: it shows up in /grants under `name`, its
// per-(type, op) access is editable at /grants/client, and revoking it there
// takes effect on the very next request — the grant ledger is the single source
// of truth regardless of how the token was minted.
//
// It registers (or reuses, by name) a client, replaces that client's grants with
// the capability's (op, type) set, and mints an opaque token bound to the
// resource URL for (surface, capability). Returns the plaintext token and the
// client id. The caller is responsible for authenticating the owner first.
func (s *Server) IssueOwnerToken(name, surface, capability string, ttl time.Duration) (token, clientID string, err error) {
	if surface != "mcp" && surface != "api" {
		return "", "", fmt.Errorf("invalid surface %q (want \"api\" or \"mcp\")", surface)
	}
	link := s.lookupLink(capability)
	if link == nil {
		return "", "", fmt.Errorf("invalid or unknown capability %q", capability)
	}
	scopes := link.Scopes()
	if len(scopes) == 0 {
		return "", "", fmt.Errorf("capability %q grants nothing", capability)
	}

	clientID, err = s.ensureNamedClient(name)
	if err != nil {
		return "", "", err
	}
	// Replace the client's grants with the capability's, exactly like re-consent.
	// Preserve any read window already set for this (reused) client so re-minting a
	// token does not silently widen reads to unrestricted.
	readWindow, err := s.grants.ReadWindowFor(clientID)
	if err != nil {
		return "", "", err
	}
	if err = s.grants.RevokeAllForClient(clientID); err != nil {
		return "", "", err
	}
	if err = s.grants.CreateFromScopes(clientID, scopes, readWindow, randomToken); err != nil {
		return "", "", err
	}

	token = randomToken()
	now := s.now().UTC()
	if _, err = s.db.Exec(
		`INSERT INTO access_tokens (token_hash, client_id, scope, resource, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		hashToken(token), clientID, strings.Join(scopes, " "), s.resourceURL(surface, capability),
		now.Add(ttl).Format(time.RFC3339), now.Format(time.RFC3339),
	); err != nil {
		return "", "", err
	}
	return token, clientID, nil
}

// ensureNamedClient returns the id of the client registered under name, creating
// a minimal public-client row if none exists yet. Reuse keeps repeated `olf
// token` runs collapsed to one dashboard entry instead of piling up clients.
func (s *Server) ensureNamedClient(name string) (string, error) {
	var id string
	err := s.db.QueryRow(
		`SELECT client_id FROM oauth_clients WHERE client_name=? ORDER BY registered_at LIMIT 1`, name,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = randomToken()
	if _, err := s.db.Exec(
		`INSERT INTO oauth_clients
		 (client_id, client_name, redirect_uris, token_endpoint_auth_method, grant_types, scope, registered_at)
		 VALUES (?, ?, '[]', 'none', '[]', '', ?)`,
		id, name, s.now().UTC().Format(time.RFC3339),
	); err != nil {
		return "", err
	}
	return id, nil
}

// --- token endpoint ---

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.tokenAuthCode(w, r)
	case "refresh_token":
		s.tokenRefresh(w, r)
	default:
		writeError(w, http.StatusBadRequest, "unsupported_grant_type", "unsupported grant_type")
	}
}

func (s *Server) tokenAuthCode(w http.ResponseWriter, r *http.Request) {
	code := r.PostForm.Get("code")
	clientID := r.PostForm.Get("client_id")
	redirectURI := r.PostForm.Get("redirect_uri")
	verifier := r.PostForm.Get("code_verifier")

	var (
		cClient, cRedirect, scope, resource, challenge, expiresAt string
	)
	err := s.db.QueryRow(
		`SELECT client_id, redirect_uri, scope, resource, code_challenge, expires_at
		 FROM auth_codes WHERE code_hash=?`, hashToken(code),
	).Scan(&cClient, &cRedirect, &scope, &resource, &challenge, &expiresAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_grant", "unknown or used authorization code")
		return
	}
	// One-time use: consume the code regardless of outcome below.
	_, _ = s.db.Exec(`DELETE FROM auth_codes WHERE code_hash=?`, hashToken(code))

	if exp, e := time.Parse(time.RFC3339, expiresAt); e != nil || s.now().After(exp) {
		writeError(w, http.StatusBadRequest, "invalid_grant", "authorization code expired")
		return
	}
	if cClient != clientID || cRedirect != redirectURI {
		writeError(w, http.StatusBadRequest, "invalid_grant", "client_id or redirect_uri mismatch")
		return
	}
	if !verifyPKCE(verifier, challenge) {
		writeError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}

	s.issueTokens(w, clientID, scope, resource, true)
}

func (s *Server) tokenRefresh(w http.ResponseWriter, r *http.Request) {
	refresh := r.PostForm.Get("refresh_token")
	clientID := r.PostForm.Get("client_id")

	var cClient, scope, resource string
	err := s.db.QueryRow(
		`SELECT client_id, scope, resource FROM refresh_tokens WHERE token_hash=?`, hashToken(refresh),
	).Scan(&cClient, &scope, &resource)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_grant", "unknown refresh token")
		return
	}
	if clientID != "" && clientID != cClient {
		writeError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
		return
	}
	// Issue a fresh access token; keep the existing refresh token.
	s.issueTokens(w, cClient, scope, resource, false)
}

func (s *Server) issueTokens(w http.ResponseWriter, clientID, scope, resource string, withRefresh bool) {
	now := s.now().UTC()
	access := randomToken()
	if _, err := s.db.Exec(
		`INSERT INTO access_tokens (token_hash, client_id, scope, resource, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		hashToken(access), clientID, scope, resource,
		now.Add(s.tokenTTL).Format(time.RFC3339), now.Format(time.RFC3339),
	); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	resp := map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   int(s.tokenTTL.Seconds()),
		"scope":        scope,
	}
	if withRefresh {
		refresh := randomToken()
		if _, err := s.db.Exec(
			`INSERT INTO refresh_tokens (token_hash, client_id, scope, resource, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			hashToken(refresh), clientID, scope, resource, now.Format(time.RFC3339),
		); err != nil {
			writeError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		resp["refresh_token"] = refresh
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- resource-server protection for /mcp ---

// RequireToken wraps a protected surface ("mcp" or "api") with the SDK's bearer
// middleware, which validates the token via verifyTokenFor, attaches the
// TokenInfo to the request context (so handlers see the client via
// auth.TokenInfoFromContext / req.Extra.TokenInfo), and on failure returns 401
// with WWW-Authenticate pointing at the resource metadata. For per-capability
// endpoints the metadata URL is the capability-specific one so clients discover
// the resource's identity, not the un-scoped surface.
func (s *Server) RequireToken(surface string, next http.Handler) http.Handler {
	// Compose the middleware per request so the linkID picked up from the path
	// flows through to both the verifier (audience check) and the challenge URL.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		linkID := r.PathValue("linkID")
		mw := auth.RequireBearerToken(s.verifyTokenFor(surface, linkID), &auth.RequireBearerTokenOptions{
			ResourceMetadataURL: s.prMetadataURL(surface, linkID),
		})
		mw(next).ServeHTTP(w, r)
	})
}

// verifyTokenFor returns a TokenVerifier that enforces audience binding against
// the resource for the given (surface, linkID) — a token minted for another
// resource is rejected. This is what stops an /api token being replayed at the
// matching /mcp endpoint (or a different capability) and vice versa. An empty
// linkID is the un-scoped surface; an empty resource (legacy token with no
// audience) is accepted only there.
func (s *Server) verifyTokenFor(surface, linkID string) auth.TokenVerifier {
	expected := s.resourceURL(surface, linkID)
	return func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		var clientID, scope, resource, expiresAt string
		err := s.db.QueryRow(
			`SELECT client_id, scope, resource, expires_at FROM access_tokens WHERE token_hash=?`, hashToken(token),
		).Scan(&clientID, &scope, &resource, &expiresAt)
		if err != nil {
			return nil, auth.ErrInvalidToken
		}
		exp, err := time.Parse(time.RFC3339, expiresAt)
		if err != nil {
			return nil, auth.ErrInvalidToken
		}
		// Audience must match exactly. An empty resource (legacy token) is rejected
		// at per-capability endpoints; the un-scoped endpoint still accepts it.
		if linkID != "" && resource != expected {
			return nil, auth.ErrInvalidToken
		}
		if linkID == "" && resource != "" && resource != expected {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{
			Scopes:     strings.Fields(scope),
			Expiration: exp,
			UserID:     clientID,
			Extra:      map[string]any{"resource": resource, "surface": surface, "link_id": linkID},
		}, nil
	}
}

// --- client lookup ---

type clientRecord struct {
	id           string
	name         string
	redirectURIs []string
}

func (s *Server) lookupClient(clientID string) (clientRecord, bool) {
	if clientID == "" {
		return clientRecord{}, false
	}
	var name, redirects string
	if err := s.db.QueryRow(
		`SELECT client_name, redirect_uris FROM oauth_clients WHERE client_id=?`, clientID,
	).Scan(&name, &redirects); err != nil {
		return clientRecord{}, false
	}
	var uris []string
	_ = json.Unmarshal([]byte(redirects), &uris)
	return clientRecord{id: clientID, name: name, redirectURIs: uris}, true
}

// --- helpers ---

// redirectError reports an authorization error by redirecting back to the
// client's redirect_uri with OAuth error parameters (RFC 6749 §4.1.2.1).
func redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, desc string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, code+": "+desc, http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", desc)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func verifyPKCE(verifier, challenge string) bool {
	if verifier == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("oauth: out of randomness: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func jsonArray(v []string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}
