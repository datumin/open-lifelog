// Package restapi is the authenticated REST surface over the core. Like the MCP
// surface (internal/mcpserver) it is a thin translation layer (runtime design
// §6): it carries no business logic, only request/response translation, and it
// enforces the same three-stage narrowing on every call —
//
//  1. capability bound: the {capability} in /api/{capability}/… caps what this
//     URL can ever reach (the link, parsed statelessly, à la MCP);
//  2. PEP: the owner's grant ledger decides what the authenticated client may do
//     right now (immediate revocation);
//  3. read window: an allowed read is constrained to the grant's occurred_at
//     window, exactly as the MCP read tools do.
//
// Bearer validation and audience binding live one layer out, in the oauth
// package's RequireToken middleware, which also attaches the client identity to
// the request context (auth.TokenInfoFromContext). An empty (nil) link means the
// un-scoped /api endpoint, which offers every type subject only to PEP.
package restapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"open-lifelog.org/node/internal/links"
	"open-lifelog.org/node/internal/olf"
	"open-lifelog.org/node/internal/pep"
	"open-lifelog.org/node/internal/query"
	"open-lifelog.org/node/internal/wire"
	"open-lifelog.org/node/internal/write"
)

// currentOLFVersion is stamped on records written through this surface when the
// caller does not supply one (mirrors mcpserver; single source is the spec).
const currentOLFVersion = "1.0"

// linkCtxKey is how the surrounding handler tells a request which capability it
// is scoped to. The un-scoped /api endpoint leaves it unset (nil link).
type linkCtxKey struct{}

// WithLink stores the per-link surface bound on the request context. The
// surrounding router (cmd/olf) sets it after parsing the {capability} segment.
func WithLink(ctx context.Context, l *links.Link) context.Context {
	return context.WithValue(ctx, linkCtxKey{}, l)
}

func linkFromCtx(ctx context.Context) *links.Link {
	l, _ := ctx.Value(linkCtxKey{}).(*links.Link)
	return l
}

// API wires the REST routes to the core read/write services and the grant ledger.
type API struct {
	q      *query.Engine
	w      *write.Service
	grants *pep.Store
	known  map[string]bool // the types the node serves; bounds read & write
}

func New(q *query.Engine, w *write.Service, grants *pep.Store, types []string) *API {
	known := make(map[string]bool, len(types))
	for _, t := range types {
		known[t] = true
	}
	return &API{q: q, w: w, grants: grants, known: known}
}

// JSONErrors normalizes the REST surface's error responses to the node's JSON
// envelope {"error": "..."}. The handlers here already emit JSON, but two pieces
// of the chain do not: the OAuth bearer middleware (the MCP go-sdk) and the
// router's capability guard both report failures via http.Error, which writes
// text/plain. Wrapping the whole /api chain in this rewrites any error response
// (status >= 400) whose body is not already JSON, so every REST error — 401
// included — shares one shape. Success responses and already-JSON bodies pass
// through byte-for-byte, and headers the inner handler set (e.g.
// WWW-Authenticate on a 401) are preserved.
func JSONErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&jsonErrorWriter{ResponseWriter: w}, r)
	})
}

// jsonErrorWriter is the ResponseWriter JSONErrors installs. It decides at
// WriteHeader time whether to rewrite the body: only for an error status whose
// Content-Type is not already JSON.
type jsonErrorWriter struct {
	http.ResponseWriter
	rewrite     bool
	wroteHeader bool
	done        bool
	status      int
}

func (w *jsonErrorWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	if code >= 400 {
		if ct := w.Header().Get("Content-Type"); ct == "" || !strings.HasPrefix(ct, "application/json") {
			w.rewrite = true
			w.Header().Set("Content-Type", "application/json")
			w.Header().Del("Content-Length") // body length changes
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *jsonErrorWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if !w.rewrite {
		return w.ResponseWriter.Write(b)
	}
	// The plain-text error sources (http.Error) write the message in a single
	// call; emit it as {"error": "..."} and swallow any trailing writes so the
	// JSON stays well-formed. Report len(b) so the caller sees a full write.
	if w.done {
		return len(b), nil
	}
	w.done = true
	body, _ := json.Marshal(wire.ErrorEnvelope{Error: wire.ErrorBody{
		Code:    codeForStatus(w.status),
		Message: strings.TrimSpace(string(b)),
	}})
	if _, err := w.ResponseWriter.Write(body); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (w *jsonErrorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// recordBody is the writable part of a record; the node owns id and recorded_at,
// so callers never supply them.
type recordBody struct {
	OLFVersion string          `json:"olf_version"`
	OccurredAt string          `json:"occurred_at"`
	TZ         string          `json:"tz,omitempty"`
	Source     string          `json:"source"`
	Payload    json.RawMessage `json:"payload"`
}

// --- enforcement ---

// principal returns the authenticated client id the oauth bearer middleware
// attached to the request context. Empty means no identity — fail closed.
func principal(r *http.Request) string {
	if ti := auth.TokenInfoFromContext(r.Context()); ti != nil {
		return ti.UserID
	}
	return ""
}

// authorize runs the three-stage check for (op, typ): identity, the capability
// bound carried by the URL, and the grant ledger. On any denial it writes the
// response itself and returns ok=false. For an allowed read it returns the
// grant's occurred_at window to apply to the query.
func (a *API) authorize(w http.ResponseWriter, r *http.Request, op, typ string) (pep.Window, bool) {
	client := principal(r)
	if client == "" {
		writeError(w, http.StatusUnauthorized, "unauthenticated")
		return pep.Window{}, false
	}
	// Stage 1: capability upper bound. The {capability} in the URL is an
	// absolute ceiling the grant ledger cannot exceed — a token for /api/meal:r
	// can never touch weight even if the client also holds a weight grant.
	if l := linkFromCtx(r.Context()); l != nil && !l.Allows(op, typ) {
		writeError(w, http.StatusForbidden, "capability does not permit "+op+" on "+typ)
		return pep.Window{}, false
	}
	// The type must be one the node actually serves. The write path enforces
	// this through the schema validator; reads have no such gate, so enforce it
	// here — otherwise a wildcard capability (*:r) would pass an arbitrary type
	// string (including a path-traversal-shaped one) straight to the store. This
	// keeps reads consistent with writes and is the surface-level half of the
	// store's own ErrInvalidType guard.
	if !a.known[typ] {
		writeError(w, http.StatusNotFound, "unknown type "+typ)
		return pep.Window{}, false
	}
	// Stage 2: PEP. The owner's live ledger decides; revocation is immediate.
	dec, err := a.grants.Authorize(client, op, typ)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return pep.Window{}, false
	}
	if !dec.Allowed {
		writeError(w, http.StatusForbidden, "access denied: no active grant for "+op+" on "+typ)
		return pep.Window{}, false
	}
	return dec.Window, true
}

// --- handlers ---

// List handles GET …/query/{type}: list a type's records within an occurred_at
// range, intersected with the grant's read window.
func (a *API) List(w http.ResponseWriter, r *http.Request) {
	typ := r.PathValue("type")
	win, ok := a.authorize(w, r, "read", typ)
	if !ok {
		return
	}
	from, err := optionalInstant(r.URL.Query().Get("from"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid from: "+err.Error())
		return
	}
	to, err := optionalInstant(r.URL.Query().Get("to"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid to: "+err.Error())
		return
	}
	// Constrain the requested range to the grant window, and record whether the
	// window clipped it so the response can say so explicitly (0 results because
	// not-visible, not because nothing happened).
	effFrom, effTo, clipped := pep.IntersectRange(from, to, win)
	recs, err := a.q.List(typ, effFrom, effTo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if recs == nil {
		recs = []olf.Record{}
	}
	meta := wire.ListMeta{
		RequestedRange: wire.MakeRange(from, to),
		EffectiveRange: wire.MakeRange(effFrom, effTo),
		Clipped:        clipped,
		Warnings:       []wire.Warning{},
	}
	if clipped {
		meta.Warnings = append(meta.Warnings, wire.Warning{
			Code:    wire.CodeRangeClippedByScope,
			Message: "results were limited to your granted read window; records outside it exist but are not visible",
		})
	}
	writeJSON(w, http.StatusOK, wire.Envelope{Data: recs, Meta: meta})
}

// Get handles GET …/query/{type}/{id}. A non-existent id is 404 not_found; a
// record that exists but falls outside the grant's read window is 403
// out_of_read_scope (carrying the granted window and the record's occurred_at).
// This single-owner node deliberately discloses the existence of unreadable
// records — knowing "there is older data you can't see" is more useful here than
// hiding it (the consent window is the access control, not obscurity).
func (a *API) Get(w http.ResponseWriter, r *http.Request) {
	typ := r.PathValue("type")
	win, ok := a.authorize(w, r, "read", typ)
	if !ok {
		return
	}
	rec, found, err := a.q.Get(typ, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "record not found")
		return
	}
	if inst, perr := olf.ParseInstant(rec.OccurredAt); perr != nil || !win.Contains(inst) {
		writeErrorBody(w, http.StatusForbidden, wire.ErrorBody{
			Code:              wire.CodeOutOfReadScope,
			Message:           "this record exists but is outside your granted read window",
			GrantedReadWindow: &wire.Range{From: wire.TimePtr(win.From), To: wire.TimePtr(win.To)},
			RecordOccurredAt:  wire.StrPtr(rec.OccurredAt),
		})
		return
	}
	writeJSON(w, http.StatusOK, wire.Envelope{Data: rec, Meta: wire.OpMeta{Warnings: []wire.Warning{}}})
}

// Create handles POST …/records/{type}.
func (a *API) Create(w http.ResponseWriter, r *http.Request) {
	typ := r.PathValue("type")
	if _, ok := a.authorize(w, r, "write", typ); !ok {
		return
	}
	body, ok := decodeBody(w, r)
	if !ok {
		return
	}
	rec, err := a.w.Record(write.RecordInput{
		Type:       typ,
		OLFVersion: version(body.OLFVersion),
		OccurredAt: body.OccurredAt,
		TZ:         body.TZ,
		Source:     body.Source,
		Payload:    body.Payload,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, wire.Envelope{Data: rec, Meta: a.writeMeta(r, rec)})
}

// Update handles PUT …/records/{type}/{id}.
func (a *API) Update(w http.ResponseWriter, r *http.Request) {
	typ := r.PathValue("type")
	if _, ok := a.authorize(w, r, "write", typ); !ok {
		return
	}
	body, ok := decodeBody(w, r)
	if !ok {
		return
	}
	rec, err := a.w.Update(write.UpdateInput{
		Type:       typ,
		ID:         r.PathValue("id"),
		OLFVersion: version(body.OLFVersion),
		OccurredAt: body.OccurredAt,
		TZ:         body.TZ,
		Source:     body.Source,
		Payload:    body.Payload,
	})
	if errors.Is(err, write.ErrNotFound) {
		writeError(w, http.StatusNotFound, "record not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, wire.Envelope{Data: rec, Meta: a.writeMeta(r, rec)})
}

// writeMeta builds the meta for a write response, warning when the record landed
// OUTSIDE the caller's read window — a `rw` client can write the past, but if its
// read window is narrower it has just created a record it can never read back.
func (a *API) writeMeta(r *http.Request, rec olf.Record) wire.OpMeta {
	warnings := []wire.Warning{}
	if client := principal(r); client != "" {
		if rw, err := a.grants.ReadWindowFor(client); err == nil {
			if inst, perr := olf.ParseInstant(rec.OccurredAt); perr == nil && !rw.Contains(inst) {
				warnings = append(warnings, wire.Warning{
					Code:    wire.CodeWrittenOutsideReadWindow,
					Message: "saved, but its occurred_at is outside your read window — you will not be able to read this record back",
				})
			}
		}
	}
	return wire.OpMeta{Warnings: warnings}
}

// Delete handles DELETE …/records/{type}/{id}.
func (a *API) Delete(w http.ResponseWriter, r *http.Request) {
	typ := r.PathValue("type")
	if _, ok := a.authorize(w, r, "write", typ); !ok {
		return
	}
	err := a.w.Delete(typ, r.PathValue("id"))
	if errors.Is(err, write.ErrNotFound) {
		writeError(w, http.StatusNotFound, "record not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

func version(v string) string {
	if v == "" {
		return currentOLFVersion
	}
	return v
}

// optionalInstant parses an empty string as "unbounded" (nil) or an
// offset-bearing timestamp as a true instant.
func optionalInstant(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := olf.ParseInstant(s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func decodeBody(w http.ResponseWriter, r *http.Request) (recordBody, bool) {
	var body recordBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return recordBody{}, false
	}
	return body, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// codeForStatus maps an HTTP status to the stable machine `code` carried in the
// error envelope, so a client can branch on the code instead of the status text.
func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return wire.CodeBadRequest
	case http.StatusUnauthorized:
		return wire.CodeUnauthenticated
	case http.StatusForbidden:
		return wire.CodeForbidden
	case http.StatusNotFound:
		return wire.CodeNotFound
	default:
		return wire.CodeInternal
	}
}

// writeError emits the standard error envelope {"error":{"code","message"}} with
// the code derived from the status.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeErrorBody(w, status, wire.ErrorBody{Code: codeForStatus(status), Message: msg})
}

// writeErrorBody emits an error envelope with a caller-supplied body (used for the
// out-of-read-scope read, which carries the granted window and the record's
// occurred_at).
func writeErrorBody(w http.ResponseWriter, status int, body wire.ErrorBody) {
	writeJSON(w, status, wire.ErrorEnvelope{Error: body})
}
