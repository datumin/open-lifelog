// Package mcpserver is the MCP surface over the core. Like the HTTP REST API it
// is a thin translation layer (runtime design §6): per-type read/write tools are
// generated from the schema registry (no hardcoded type list) and call the same
// core (query / write). Tool annotations mark read-only vs destructive, and each
// tool records the scope PEP will require once enforcement lands.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"open-lifelog.org/node/internal/links"
	"open-lifelog.org/node/internal/olf"
	"open-lifelog.org/node/internal/pep"
	"open-lifelog.org/node/internal/query"
	"open-lifelog.org/node/internal/validate"
	"open-lifelog.org/node/internal/wire"
	"open-lifelog.org/node/internal/write"
)

// currentOLFVersion is the version the node stamps on records written through
// this surface when the caller does not specify one. It tracks the current spec
// version (single source: the spec / schemas).
const currentOLFVersion = "1.0"

// linkCtxKey is how the surrounding HTTP handler tells getServer which link the
// request is for; the all-tools Handler() leaves it unset, per-link handlers set it.
type linkCtxKey struct{}

// WithLink stores the per-link surface bound on the request context.
func WithLink(ctx context.Context, l *links.Link) context.Context {
	return context.WithValue(ctx, linkCtxKey{}, l)
}

func linkFromCtx(ctx context.Context) *links.Link {
	l, _ := ctx.Value(linkCtxKey{}).(*links.Link)
	return l
}

// Server builds per-(type|link) MCP servers exposing OLF tools.
type Server struct {
	q          *query.Engine
	w          *write.Service
	grants     *pep.Store
	types      []string
	payloadRaw map[string]json.RawMessage
	scopes     map[string]string // tool name -> required scope (across all types)
}

// New constructs the MCP server builder. It registers nothing up front; each
// HTTP request gets its own mcp.Server populated with exactly the tools allowed
// by the link bound to the request (or every type, for the all-tools handler).
// Every tool call is enforced against the grant ledger via the OAuth identity.
func New(q *query.Engine, w *write.Service, v *validate.Validator, grants *pep.Store) *Server {
	s := &Server{
		q:          q,
		w:          w,
		grants:     grants,
		types:      v.PayloadTypes(),
		payloadRaw: map[string]json.RawMessage{},
		scopes:     map[string]string{},
	}
	for _, typ := range s.types {
		ps, _ := v.RawPayloadSchema(typ)
		s.payloadRaw[typ] = ps
		// Pre-compute scope strings so RequiredScope() works without building a server.
		s.scopes[typ+"_record"] = "lifelog:write:" + typ
		s.scopes[typ+"_update"] = "lifelog:write:" + typ
		s.scopes[typ+"_delete"] = "lifelog:write:" + typ
		s.scopes[typ+"_get"] = "lifelog:read:" + typ
		s.scopes[typ+"_list"] = "lifelog:read:" + typ
	}
	return s
}

// Handler returns the streamable HTTP handler for the un-scoped MCP endpoint at
// /mcp — every type's tools are exposed. Per-link endpoints use LinkHandler.
//
// DisableLocalhostProtection turns off the SDK's DNS-rebinding guard, which
// otherwise 403s any request whose Host header is non-localhost — including
// every request that reaches us through a tunnel or reverse proxy. That guard is
// meant for unauthenticated local servers; here the endpoint is already gated by
// the OAuth bearer middleware, so an attacker without a token gets 401 regardless.
func (s *Server) Handler() http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server { return s.buildServer(linkFromCtx(r.Context())) },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true},
	)
}

// LinkSurface is the minimal interface the surrounding handler uses to scope
// tools — anything that reports allowed (op, type) pairs and has a stable id.
type LinkSurface interface {
	Allows(op, typ string) bool
}

// buildServer creates a fresh mcp.Server populated with only the (type, op)
// tools the link allows. A nil link means "all".
func (s *Server) buildServer(l *links.Link) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "open-lifelog", Version: "0.0.1"}, nil)
	b := &builder{srv: srv, parent: s, link: l}
	for _, typ := range s.types {
		b.addType(typ, s.payloadRaw[typ])
	}
	return srv
}

// builder is a transient helper used while populating one mcp.Server. It carries
// the link scope so per-link tool registration stays declarative.
type builder struct {
	srv    *mcp.Server
	parent *Server
	link   *links.Link
}

func (b *builder) linkAllows(op, typ string) bool {
	return b.link == nil || b.link.Allows(op, typ)
}

// RequiredScope returns the scope a tool needs (e.g. "lifelog:write:weight").
func (s *Server) RequiredScope(tool string) (string, bool) {
	sc, ok := s.scopes[tool]
	return sc, ok
}

func (b *builder) addType(typ string, payloadSchema json.RawMessage) {
	readOnly := true
	writeable := false
	destructive := true
	p := b.parent

	if b.linkAllows("write", typ) {
		b.tool(typ+"_record", "Record a new "+typ+" observation.",
			recordSchema(typ, payloadSchema),
			&mcp.ToolAnnotations{ReadOnlyHint: writeable, DestructiveHint: ptr(false), Title: "Record " + typ},
			p.handleRecord(typ))
		b.tool(typ+"_update", "Replace an existing "+typ+" record (by id).",
			updateSchema(typ, payloadSchema),
			&mcp.ToolAnnotations{ReadOnlyHint: writeable, DestructiveHint: ptr(destructive), IdempotentHint: true, Title: "Update " + typ},
			p.handleUpdate(typ))
		b.tool(typ+"_delete", "Delete a "+typ+" record (by id).",
			idSchema("the id of the "+typ+" record to delete"),
			&mcp.ToolAnnotations{ReadOnlyHint: writeable, DestructiveHint: ptr(destructive), IdempotentHint: true, Title: "Delete " + typ},
			p.handleDelete(typ))
	}
	if b.linkAllows("read", typ) {
		b.tool(typ+"_get", "Get a single "+typ+" record by id.",
			idSchema("the id of the "+typ+" record to fetch"),
			&mcp.ToolAnnotations{ReadOnlyHint: readOnly, Title: "Get " + typ},
			p.handleGet(typ))
		b.tool(typ+"_list", "List "+typ+" records within an occurred_at range.",
			listSchema(),
			&mcp.ToolAnnotations{ReadOnlyHint: readOnly, Title: "List " + typ},
			p.handleList(typ))
	}
}

func (b *builder) tool(name, desc string, in any, ann *mcp.ToolAnnotations, h mcp.ToolHandler) {
	b.srv.AddTool(&mcp.Tool{
		Name:        name,
		Description: desc,
		InputSchema: in,
		Annotations: ann,
	}, h)
}

// --- PEP enforcement ---

// principal returns the authenticated client id attached by the OAuth bearer
// middleware (req.Extra.TokenInfo). Empty means no identity — fail closed.
func principal(req *mcp.CallToolRequest) string {
	if req.Extra != nil && req.Extra.TokenInfo != nil {
		return req.Extra.TokenInfo.UserID
	}
	return ""
}

// enforce checks the grant ledger for (operation, typ). It returns a non-nil
// deny result if the call is not allowed, plus the read window to apply.
func (s *Server) enforce(req *mcp.CallToolRequest, operation, typ string) (*mcp.CallToolResult, pep.Window) {
	client := principal(req)
	if client == "" {
		return errCodeResult(wire.CodeUnauthenticated, "unauthenticated"), pep.Window{}
	}
	dec, err := s.grants.Authorize(client, operation, typ)
	if err != nil {
		return errCodeResult(wire.CodeInternal, err.Error()), pep.Window{}
	}
	if !dec.Allowed {
		return errCodeResult(wire.CodeForbidden, fmt.Sprintf("access denied: no active grant for %s on %q", operation, typ)), pep.Window{}
	}
	return nil, dec.Window
}

// --- handlers ---

type recordArgs struct {
	OccurredAt string          `json:"occurred_at"`
	OLFVersion string          `json:"olf_version"`
	TZ         string          `json:"tz"`
	Source     string          `json:"source"`
	Payload    json.RawMessage `json:"payload"`
}

type updateArgs struct {
	ID string `json:"id"`
	recordArgs
}

type idArgs struct {
	ID string `json:"id"`
}

type listArgs struct {
	OccurredFrom string `json:"occurred_from"`
	OccurredTo   string `json:"occurred_to"`
}

func (s *Server) handleRecord(typ string) mcp.ToolHandler {
	return func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deny, _ := s.enforce(req, "write", typ); deny != nil {
			return deny, nil
		}
		var a recordArgs
		if err := json.Unmarshal(req.Params.Arguments, &a); err != nil {
			return errCodeResult(wire.CodeBadRequest, err.Error()), nil
		}
		rec, err := s.w.Record(write.RecordInput{
			Type: typ, OLFVersion: version(a.OLFVersion),
			OccurredAt: a.OccurredAt, TZ: a.TZ, Source: a.Source, Payload: a.Payload,
		})
		if err != nil {
			return writeErr(err), nil
		}
		return envelopeResult(rec, s.writeMeta(req, rec)), nil
	}
}

func (s *Server) handleUpdate(typ string) mcp.ToolHandler {
	return func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deny, _ := s.enforce(req, "write", typ); deny != nil {
			return deny, nil
		}
		var a updateArgs
		if err := json.Unmarshal(req.Params.Arguments, &a); err != nil {
			return errCodeResult(wire.CodeBadRequest, err.Error()), nil
		}
		rec, err := s.w.Update(write.UpdateInput{
			Type: typ, ID: a.ID, OLFVersion: version(a.OLFVersion),
			OccurredAt: a.OccurredAt, TZ: a.TZ, Source: a.Source, Payload: a.Payload,
		})
		if err != nil {
			return writeErr(err), nil
		}
		return envelopeResult(rec, s.writeMeta(req, rec)), nil
	}
}

func (s *Server) handleDelete(typ string) mcp.ToolHandler {
	return func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deny, _ := s.enforce(req, "write", typ); deny != nil {
			return deny, nil
		}
		var a idArgs
		if err := json.Unmarshal(req.Params.Arguments, &a); err != nil {
			return errCodeResult(wire.CodeBadRequest, err.Error()), nil
		}
		if err := s.w.Delete(typ, a.ID); err != nil {
			return writeErr(err), nil
		}
		return textResult("deleted "+typ+" "+a.ID, map[string]any{"deleted": true, "id": a.ID}), nil
	}
}

func (s *Server) handleGet(typ string) mcp.ToolHandler {
	return func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		deny, win := s.enforce(req, "read", typ)
		if deny != nil {
			return deny, nil
		}
		var a idArgs
		if err := json.Unmarshal(req.Params.Arguments, &a); err != nil {
			return errCodeResult(wire.CodeBadRequest, err.Error()), nil
		}
		rec, found, err := s.q.Get(typ, a.ID)
		if err != nil {
			return errCodeResult(wire.CodeInternal, err.Error()), nil
		}
		if !found {
			return errResultBody(wire.ErrorBody{Code: wire.CodeNotFound, Message: "record not found"}), nil
		}
		// A record that exists but falls outside the grant window is reported as
		// out_of_read_scope (existence disclosed; single-owner node), carrying the
		// granted window and the record's occurred_at so the client can explain it.
		if inst, perr := olf.ParseInstant(rec.OccurredAt); perr != nil || !win.Contains(inst) {
			return errResultBody(wire.ErrorBody{
				Code:              wire.CodeOutOfReadScope,
				Message:           "this record exists but is outside your granted read window",
				GrantedReadWindow: &wire.Range{From: wire.TimePtr(win.From), To: wire.TimePtr(win.To)},
				RecordOccurredAt:  wire.StrPtr(rec.OccurredAt),
			}), nil
		}
		return envelopeResult(rec, wire.OpMeta{Warnings: []wire.Warning{}}), nil
	}
}

func (s *Server) handleList(typ string) mcp.ToolHandler {
	return func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		deny, win := s.enforce(req, "read", typ)
		if deny != nil {
			return deny, nil
		}
		var a listArgs
		if err := json.Unmarshal(req.Params.Arguments, &a); err != nil {
			return errCodeResult(wire.CodeBadRequest, err.Error()), nil
		}
		from, err := optionalInstant(a.OccurredFrom)
		if err != nil {
			return errCodeResult(wire.CodeBadRequest, err.Error()), nil
		}
		to, err := optionalInstant(a.OccurredTo)
		if err != nil {
			return errCodeResult(wire.CodeBadRequest, err.Error()), nil
		}
		// Constrain the requested range to the grant window, recording whether the
		// window clipped it so the client can tell "0 results = not visible" from
		// "0 results = nothing happened".
		effFrom, effTo, clipped := pep.IntersectRange(from, to, win)
		recs, err := s.q.List(typ, effFrom, effTo)
		if err != nil {
			return errCodeResult(wire.CodeInternal, err.Error()), nil
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
		return envelopeResult(recs, meta), nil
	}
}

// --- helpers ---

func version(v string) string {
	if v == "" {
		return currentOLFVersion
	}
	return v
}

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

// envelopeResult returns a successful tool result whose structured content AND
// text are the {data, meta} envelope — so an LLM client sees the meta (e.g. a
// clipped range or a written-outside-read-window warning), not just the data.
func envelopeResult(data, meta any) *mcp.CallToolResult {
	env := wire.Envelope{Data: data, Meta: meta}
	body, _ := json.Marshal(env)
	return textResult(string(body), env)
}

func textResult(text string, structured any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text}},
		StructuredContent: structured,
	}
}

// errResultBody is a tool error carrying a stable machine code (and, for an
// out-of-read-scope read, the granted window + the record's occurred_at) in the
// structured content, so a client can branch on it.
func errResultBody(body wire.ErrorBody) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError:           true,
		Content:           []mcp.Content{&mcp.TextContent{Text: body.Message}},
		StructuredContent: wire.ErrorEnvelope{Error: body},
	}
}

// errCodeResult is a tool error with a specific stable code (so MCP codes match
// the REST surface for the same logical failure).
func errCodeResult(code, msg string) *mcp.CallToolResult {
	return errResultBody(wire.ErrorBody{Code: code, Message: msg})
}

// writeErr maps a core write error to a coded tool error: a missing target is
// not_found (matching REST), anything else is a validation/bad-request failure.
func writeErr(err error) *mcp.CallToolResult {
	if errors.Is(err, write.ErrNotFound) {
		return errCodeResult(wire.CodeNotFound, "record not found")
	}
	return errCodeResult(wire.CodeBadRequest, err.Error())
}

// writeMeta warns when a written record's occurred_at is outside the caller's
// read window (a rw client can write the past, but a narrower read window means
// it just created a record it can never read back).
func (s *Server) writeMeta(req *mcp.CallToolRequest, rec olf.Record) wire.OpMeta {
	warnings := []wire.Warning{}
	if client := principal(req); client != "" {
		if rw, err := s.grants.ReadWindowFor(client); err == nil {
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

func ptr[T any](v T) *T { return &v }
