// Command olf is the self-hosted OLF node. It exposes a single subcommand,
// `serve`, which starts the HTTP server over the local lifelog: two
// OAuth-protected external surfaces (MCP and the REST API), the OAuth
// authorization server, and the owner dashboard. The default bind is localhost;
// set --base-url and a non-loopback --addr to expose it (e.g. behind a tunnel).
package main

import (
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	_ "time/tzdata" // embed the IANA tz database so --tz works without system zoneinfo

	"open-lifelog.org/node/internal/links"
	"open-lifelog.org/node/internal/mcpserver"
	"open-lifelog.org/node/internal/meta"
	"open-lifelog.org/node/internal/oauth"
	"open-lifelog.org/node/internal/owner"
	"open-lifelog.org/node/internal/pep"
	"open-lifelog.org/node/internal/query"
	"open-lifelog.org/node/internal/restapi"
	"open-lifelog.org/node/internal/store"
	"open-lifelog.org/node/internal/validate"
	"open-lifelog.org/node/internal/write"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "token":
		tokenCmd(os.Args[2:])
	case "secret":
		secretCmd(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  olf serve  [--addr host:port] [--data dir] [--base-url url]")
	fmt.Fprintln(os.Stderr, "  olf token  [--data dir] [--base-url url] [--cap cap] [--surface api|mcp] [--name label] [--ttl dur]")
	fmt.Fprintln(os.Stderr, "  olf secret rotate [--data dir]")
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8787", "address to bind (localhost only by default)")
	data := fs.String("data", "./lifelog", "directory holding the JSONL lifelog")
	baseURL := fs.String("base-url", "", "external origin for OAuth metadata (default http://<addr>)")
	tz := fs.String("tz", "", "IANA timezone for read-window calendar days (default: host local time)")
	_ = fs.Parse(args)

	if *baseURL == "" {
		*baseURL = "http://" + *addr
	}
	loc := mustLoadLocation(*tz)

	v, err := validate.New()
	if err != nil {
		log.Fatalf("compile schemas: %v", err)
	}
	if err := os.MkdirAll(*data, 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}
	st := store.NewFSStore(*data)
	q := query.New(st)
	w := write.New(st, v, nil)

	metaStore, err := meta.Open(filepath.Join(*data, "meta.db"))
	if err != nil {
		log.Fatalf("open metadata db: %v", err)
	}
	defer metaStore.Close()

	own := owner.New(metaStore)
	if secret, created, err := own.EnsureSecret(); err != nil {
		log.Fatalf("owner bootstrap: %v", err)
	} else if created {
		log.Printf("owner secret (shown once — save it; needed to approve app access):\n\n    %s\n", secret)
	}
	grants := pep.New(metaStore)
	types := v.PayloadTypes()
	adapter := linkAdapter{types: types}
	authz := oauth.New(metaStore, *baseURL, own, grants, adapter, types, loc)
	mcps := mcpserver.New(q, w, v, grants)
	api := restapi.New(q, w, grants, types)

	// One HTTP server hosts both external surfaces over the same core, each an
	// OAuth-protected resource with identical three-stage scoping (capability
	// URL → consent → PEP). /mcp is the un-scoped MCP endpoint;
	// /mcp/{capability} and /api/{capability}/… bound tools/routes, consent, and
	// audience to the URL's (types × ops). The surface segment ("mcp"/"api") is
	// part of the audience, so a token for one cannot be replayed at the other.
	root := http.NewServeMux()
	root.Handle("/mcp", authz.RequireToken("mcp", mcps.Handler()))
	root.Handle("/mcp/{linkID}", authz.RequireToken("mcp", linkRouter(types, mcps)))
	mountREST(root, authz, api, types)
	root.HandleFunc("GET /favicon.ico", favicon)
	authz.Register(root)
	own.Register(root)

	log.Printf("olf serving on http://%s | base-url %s | data %s | tz %s", *addr, *baseURL, *data, loc)
	if err := http.ListenAndServe(*addr, logging(root)); err != nil {
		log.Fatal(err)
	}
}

// restRoute is one REST method+path under a capability, paired with its handler.
type restRoute struct {
	method, suffix string
	h              http.HandlerFunc
}

// mountREST wires the REST API under the per-capability /api/{capability}
// surface. Every REST call is capability-scoped — for "all types" the owner
// issues an /api/*:rw (or *:r) capability; there is deliberately no ambiguous
// un-scoped /api endpoint. Each route is OAuth-protected (audience bound to the
// surface + capability) and carries the parsed capability so the handler
// enforces it as an upper bound the grant ledger cannot exceed.
func mountREST(root *http.ServeMux, authz *oauth.Server, api *restapi.API, types []string) {
	routes := []restRoute{
		{"GET", "/query/{type}", api.List},
		{"GET", "/query/{type}/{id}", api.Get},
		{"POST", "/records/{type}", api.Create},
		{"PUT", "/records/{type}/{id}", api.Update},
		{"DELETE", "/records/{type}/{id}", api.Delete},
	}
	for _, rt := range routes {
		// JSONErrors is outermost so it also normalizes the bearer middleware's
		// 401 (text/plain) into the REST surface's {"error": …} envelope.
		root.Handle(rt.method+" /api/{linkID}"+rt.suffix,
			restapi.JSONErrors(authz.RequireToken("api", restLinkRouter(types, rt.h))))
	}
}

// restLinkRouter parses the {capability} path segment and attaches the resulting
// link to the request context so the REST handler enforces it as an upper bound.
// A malformed capability is 404 (mirrors the MCP linkRouter).
func restLinkRouter(types []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l, err := links.Parse(r.PathValue("linkID"), types)
		if err != nil {
			http.Error(w, "invalid capability", http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r.WithContext(restapi.WithLink(r.Context(), &l)))
	})
}

// linkAdapter parses the path segment after /mcp/ as a capability string, so
// the URL itself is the link (no stored records). Inspired by Discord bot
// invite URLs: the URL IS the capability. The known type list bounds parsing
// so capabilities for unknown types are rejected.
type linkAdapter struct{ types []string }

func (a linkAdapter) Lookup(capability string) oauth.Link {
	l, err := links.Parse(capability, a.types)
	if err != nil {
		return nil
	}
	return l
}

// linkRouter handles /mcp/{linkID}: it parses the path segment as a capability
// and attaches the resulting link to the request context so the MCP handler
// only registers tools the link allows. A malformed capability is 404.
func linkRouter(types []string, mcps *mcpserver.Server) http.Handler {
	handler := mcps.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l, err := links.Parse(r.PathValue("linkID"), types)
		if err != nil {
			http.Error(w, "invalid capability", http.StatusNotFound)
			return
		}
		handler.ServeHTTP(w, r.WithContext(mcpserver.WithLink(r.Context(), &l)))
	})
}

// tokenCmd mints an access token for the owner's own use (local testing,
// scripts, cron) without the browser OAuth dance. It opens the node's data dir
// directly and writes the same client + grants + token the interactive flow
// would — so the token shows up in the dashboard (/grants), is editable per
// (type, op), and is revocable there immediately. The token is printed to
// stdout; diagnostics go to stderr.
//
// No owner secret is required: this is a local command that already needs write
// access to the data dir's meta.db, which is itself owner-level authority
// (anyone with it could forge a token by editing the DB directly). The secret
// guards the *browser* login, where there is no such filesystem boundary.
//
// --base-url MUST match the running node's --base-url, since it forms the
// token's audience (resource). Use a capability like '*:rw' for all types, or
// 'meal:r,sleep:r' to scope it.
func tokenCmd(args []string) {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	data := fs.String("data", "./lifelog", "data directory of the node (holds meta.db)")
	baseURL := fs.String("base-url", "http://localhost:8787", "external origin; MUST match the node's --base-url (forms the token audience)")
	capability := fs.String("cap", "*:rw", "capability to grant, e.g. 'meal:r,sleep:r' ('*' = all types)")
	surface := fs.String("surface", "api", "surface the token targets: api or mcp")
	name := fs.String("name", "local CLI token", "client name shown in the owner dashboard (reused across runs)")
	ttl := fs.Duration("ttl", 365*24*time.Hour, "token lifetime")
	_ = fs.Parse(args)

	v, err := validate.New()
	if err != nil {
		log.Fatalf("compile schemas: %v", err)
	}
	metaStore, err := meta.Open(filepath.Join(*data, "meta.db"))
	if err != nil {
		log.Fatalf("open metadata db: %v", err)
	}
	defer metaStore.Close()

	own := owner.New(metaStore)
	grants := pep.New(metaStore)
	types := v.PayloadTypes()
	// The token command issues/preserves grants but never parses window dates, so
	// the timezone is irrelevant here; nil → host local in oauth.New.
	authz := oauth.New(metaStore, *baseURL, own, grants, linkAdapter{types: types}, types, nil)

	token, clientID, err := authz.IssueOwnerToken(*name, *surface, *capability, *ttl)
	if err != nil {
		log.Fatalf("issue token: %v", err)
	}
	base := strings.TrimRight(*baseURL, "/")
	fmt.Fprintf(os.Stderr, "resource : %s/%s/%s\n", base, *surface, *capability)
	fmt.Fprintf(os.Stderr, "manage   : %s/grants/client?client_id=%s\n", base, clientID)
	fmt.Fprintf(os.Stderr, "example  : curl -H 'Authorization: Bearer <token>' '%s/%s/%s/query/<type>'\n", base, *surface, *capability)
	fmt.Println(token)
}

// secretCmd manages the owner secret. The only action is `rotate`: it generates
// a fresh owner secret (the old one is unrecoverable — only its hash was stored),
// invalidates existing owner sessions, and prints the new secret once. Like
// `token`, it is a local command gated by data-dir access, not by the secret
// itself (you cannot be asked for what you've lost).
func secretCmd(args []string) {
	if len(args) < 1 || args[0] != "rotate" {
		fmt.Fprintln(os.Stderr, "usage: olf secret rotate [--data dir]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("secret rotate", flag.ExitOnError)
	data := fs.String("data", "./lifelog", "data directory of the node (holds meta.db)")
	_ = fs.Parse(args[1:])

	metaStore, err := meta.Open(filepath.Join(*data, "meta.db"))
	if err != nil {
		log.Fatalf("open metadata db: %v", err)
	}
	defer metaStore.Close()

	secret, err := owner.New(metaStore).RotateSecret()
	if err != nil {
		log.Fatalf("rotate owner secret: %v", err)
	}
	fmt.Fprintln(os.Stderr, "owner secret rotated — the previous secret and all owner sessions are now invalid.")
	fmt.Fprintln(os.Stderr, "save this; it is shown once and cannot be recovered:")
	fmt.Println(secret)
}

// faviconGIF is a 1x1 transparent GIF served as a dummy favicon so browsers
// hitting the consent/login pages don't log a 404 for /favicon.ico.
var faviconGIF, _ = base64.StdEncoding.DecodeString(
	"R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7")

func favicon(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(faviconGIF)
}

// mustLoadLocation resolves the --tz flag to a *time.Location. Empty means the
// host's local time. An unknown zone is fatal (better to fail at startup than to
// silently compute read-window days in the wrong calendar).
func mustLoadLocation(name string) *time.Location {
	if name == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		log.Fatalf("invalid --tz %q: %v", name, err)
	}
	return loc
}

// logging wraps the handler to log each request's method, path, and status. The
// wrapper preserves streaming (Flush) and the unwrap chain so the MCP SSE
// endpoint keeps working.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s -> %d", r.Method, r.URL.RequestURI(), sw.status)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
