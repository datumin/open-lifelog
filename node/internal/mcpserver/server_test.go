package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"open-lifelog.org/node/internal/links"
	"open-lifelog.org/node/internal/meta"
	"open-lifelog.org/node/internal/pep"
	"open-lifelog.org/node/internal/query"
	"open-lifelog.org/node/internal/store"
	"open-lifelog.org/node/internal/validate"
	"open-lifelog.org/node/internal/wire"
	"open-lifelog.org/node/internal/write"
)

func newGrants(t *testing.T) *pep.Store {
	t.Helper()
	ms, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { ms.Close() })
	return pep.New(ms)
}

// connectInMemory wires an in-memory client for tool-listing tests (no auth).
func connectInMemory(t *testing.T) *mcp.ClientSession {
	t.Helper()
	v, err := validate.New()
	if err != nil {
		t.Fatalf("validate.New: %v", err)
	}
	st := store.NewFSStore(t.TempDir())
	srv := New(query.New(st), write.New(st, v, nil), v, newGrants(t))

	ctx := context.Background()
	serverTr, clientTr := mcp.NewInMemoryTransports()
	ss, err := srv.buildServer(nil).Connect(ctx, serverTr, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientTr, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestListToolsExposesPerTypeTools(t *testing.T) {
	cs := connectInMemory(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"weight_record", "weight_update", "weight_delete", "weight_get", "weight_list"} {
		if !names[want] {
			t.Errorf("missing tool %q", want)
		}
	}
	if len(res.Tools) == 0 || len(res.Tools)%5 != 0 {
		t.Errorf("expected a multiple of 5 tools, got %d", len(res.Tools))
	}
}

// TestMealToolSchemaExposesItemFields guards the $defs hoisting: the meal tool's
// input schema must carry $defs at its root and reference it, so the client sees
// the rich item fields (grams/kcal/macros), not just the name.
func TestMealToolSchemaExposesItemFields(t *testing.T) {
	cs := connectInMemory(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var schema string
	for _, tool := range res.Tools {
		if tool.Name == "meal_record" {
			b, _ := json.Marshal(tool.InputSchema)
			schema = string(b)
		}
	}
	if schema == "" {
		t.Fatal("meal_record tool not found")
	}
	for _, want := range []string{`"$defs"`, "mealItem", "#/$defs/mealItem", "protein_g", "kcal"} {
		if !strings.Contains(schema, want) {
			t.Errorf("meal input schema missing %q\nschema: %s", want, schema)
		}
	}
}

func TestLinkScopedServerExposesOnlyAllowedTools(t *testing.T) {
	v, _ := validate.New()
	st := store.NewFSStore(t.TempDir())
	srv := New(query.New(st), write.New(st, v, nil), v, newGrants(t))

	// Build a link-scoped server: only meal write tools should be present.
	link, err := links.Parse("meal:w", srv.types)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	mcpsrv := srv.buildServer(&link)

	ctx := context.Background()
	serverTr, clientTr := mcp.NewInMemoryTransports()
	ss, err := mcpsrv.Connect(ctx, serverTr, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientTr, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	for _, want := range []string{"meal_record", "meal_update", "meal_delete"} {
		if !got[want] {
			t.Errorf("link-scoped server missing %q", want)
		}
	}
	for _, banned := range []string{"meal_get", "meal_list", "weight_record", "weight_get", "sleep_record"} {
		if got[banned] {
			t.Errorf("link-scoped server unexpectedly exposes %q", banned)
		}
	}
}

func TestUnscopedServerExposesAllTools(t *testing.T) {
	cs := connectInMemory(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools)%5 != 0 || len(res.Tools) == 0 {
		t.Errorf("expected all-tools server to expose 5×N tools, got %d", len(res.Tools))
	}
}

func TestRequiredScopesRecorded(t *testing.T) {
	v, _ := validate.New()
	st := store.NewFSStore(t.TempDir())
	srv := New(query.New(st), write.New(st, v, nil), v, newGrants(t))
	if sc, _ := srv.RequiredScope("weight_record"); sc != "lifelog:write:weight" {
		t.Errorf("weight_record scope = %q", sc)
	}
	if sc, _ := srv.RequiredScope("weight_list"); sc != "lifelog:read:weight" {
		t.Errorf("weight_list scope = %q", sc)
	}
}

// --- PEP enforcement over the real HTTP path ---

type bearerRT struct{ rt http.RoundTripper }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.Header.Set("Authorization", "Bearer test-token")
	return b.rt.RoundTrip(r2)
}

// connectHTTP serves the MCP endpoint behind a bearer middleware that identifies
// every request as clientID (simulating a validated OAuth token), so enforcement
// runs against the grant ledger exactly as in production.
func connectHTTP(t *testing.T, clientID string, grants *pep.Store) *mcp.ClientSession {
	t.Helper()
	v, err := validate.New()
	if err != nil {
		t.Fatalf("validate.New: %v", err)
	}
	st := store.NewFSStore(t.TempDir())
	srv := New(query.New(st), write.New(st, v, nil), v, grants)

	mw := auth.RequireBearerToken(func(context.Context, string, *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{UserID: clientID, Expiration: time.Now().Add(time.Hour)}, nil
	}, nil)
	ts := httptest.NewServer(mw(srv.Handler()))
	t.Cleanup(ts.Close)

	transport := &mcp.StreamableClientTransport{
		Endpoint:   ts.URL,
		HTTPClient: &http.Client{Transport: bearerRT{http.DefaultTransport}},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func weightPayload(kg float64) map[string]any {
	return map[string]any{
		"occurred_at": "2026-05-28T07:05:00+09:00",
		"source":      "test",
		"payload":     map[string]any{"weight_kg": kg},
	}
}

func TestEnforce_AllowedWithGrant(t *testing.T) {
	grants := newGrants(t)
	if err := grants.Create(pep.Grant{ID: "gw", ClientID: "C", Operation: "write", Types: []string{"weight"}, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	cs := connectHTTP(t, "C", grants)
	res := callTool(t, cs, "weight_record", weightPayload(70.5))
	if res.IsError {
		t.Fatalf("expected allowed record, got error result")
	}
}

func TestEnforce_DeniedWithoutGrant(t *testing.T) {
	grants := newGrants(t)
	// Grant covers weight only; sleep must be denied.
	if err := grants.Create(pep.Grant{ID: "gw", ClientID: "C", Operation: "write", Types: []string{"weight"}, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	cs := connectHTTP(t, "C", grants)
	res := callTool(t, cs, "sleep_record", map[string]any{
		"occurred_at": "2026-05-28T23:00:00+09:00", "source": "test",
		"payload": map[string]any{"ended_at": "2026-05-29T07:00:00+09:00"},
	})
	if !res.IsError {
		t.Fatal("expected sleep_record to be denied (no grant)")
	}
}

func TestEnforce_ReadAfterRevoke(t *testing.T) {
	grants := newGrants(t)
	for _, g := range []pep.Grant{
		{ID: "gw", ClientID: "C", Operation: "write", Types: []string{"weight"}, Status: "active"},
		{ID: "gr", ClientID: "C", Operation: "read", Types: []string{"weight"}, Status: "active"},
	} {
		if err := grants.Create(g); err != nil {
			t.Fatal(err)
		}
	}
	cs := connectHTTP(t, "C", grants)

	if callTool(t, cs, "weight_record", weightPayload(70.5)).IsError {
		t.Fatal("record should be allowed before revoke")
	}
	if callTool(t, cs, "weight_list", map[string]any{}).IsError {
		t.Fatal("list should be allowed before revoke")
	}

	// Revoke the read grant — the very next read must be denied.
	if ok, err := grants.Revoke("gr"); err != nil || !ok {
		t.Fatalf("revoke: ok=%v err=%v", ok, err)
	}
	if !callTool(t, cs, "weight_list", map[string]any{}).IsError {
		t.Fatal("expected read-after-revoke to be denied")
	}
}

// errCode extracts the stable machine code from a tool-error result's structured
// content. MCP and REST must use the SAME codes for the same logical failure.
func errCode(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if !res.IsError {
		t.Fatalf("expected an error result, got success")
	}
	b, _ := json.Marshal(res.StructuredContent)
	var env wire.ErrorEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("decode error envelope: %v (%s)", err, b)
	}
	return env.Error.Code
}

// Tool errors carry the SAME stable codes as the REST surface, so a client can
// branch on `code` portably across surfaces (not a generic "error").
func TestErrorCodes_MatchRESTAcrossConditions(t *testing.T) {
	grants := newGrants(t)
	for _, g := range []pep.Grant{
		{ID: "gw", ClientID: "C", Operation: "write", Types: []string{"weight"}, Status: "active"},
		{ID: "gr", ClientID: "C", Operation: "read", Types: []string{"weight"}, Status: "active"},
	} {
		if err := grants.Create(g); err != nil {
			t.Fatal(err)
		}
	}
	cs := connectHTTP(t, "C", grants)

	// No grant for sleep → forbidden (not generic "error").
	c := errCode(t, callTool(t, cs, "sleep_record", map[string]any{
		"occurred_at": "2026-05-28T23:00:00+09:00", "source": "t",
		"payload": map[string]any{"ended_at": "2026-05-29T07:00:00+09:00"},
	}))
	if c != wire.CodeForbidden {
		t.Errorf("no-grant code = %q, want %q", c, wire.CodeForbidden)
	}

	// Update / delete a valid-but-non-existent record → not_found (matches REST).
	// (A malformed id is a bad_request on both surfaces — the envelope validates
	// id as a UUID — so use a real-shaped id that simply doesn't exist.)
	const missingID = "019e0000-0000-7000-8000-000000000000"
	c = errCode(t, callTool(t, cs, "weight_update", map[string]any{
		"id": missingID, "occurred_at": "2026-05-28T07:05:00+09:00", "source": "t",
		"payload": map[string]any{"weight_kg": 70},
	}))
	if c != wire.CodeNotFound {
		t.Errorf("update-missing code = %q, want %q", c, wire.CodeNotFound)
	}
	c = errCode(t, callTool(t, cs, "weight_delete", map[string]any{"id": missingID}))
	if c != wire.CodeNotFound {
		t.Errorf("delete-missing code = %q, want %q", c, wire.CodeNotFound)
	}

	// Invalid payload → bad_request.
	c = errCode(t, callTool(t, cs, "weight_record", map[string]any{
		"occurred_at": "2026-05-28T07:05:00+09:00", "source": "t",
		"payload": map[string]any{"weight_kg": -1},
	}))
	if c != wire.CodeBadRequest {
		t.Errorf("invalid-payload code = %q, want %q", c, wire.CodeBadRequest)
	}

	// Get a non-existent id → not_found.
	c = errCode(t, callTool(t, cs, "weight_get", map[string]any{"id": "nope"}))
	if c != wire.CodeNotFound {
		t.Errorf("get-missing code = %q, want %q", c, wire.CodeNotFound)
	}
}

func TestEnforce_InvalidPayloadStillRejected(t *testing.T) {
	grants := newGrants(t)
	if err := grants.Create(pep.Grant{ID: "gw", ClientID: "C", Operation: "write", Types: []string{"weight"}, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	cs := connectHTTP(t, "C", grants)
	res := callTool(t, cs, "weight_record", map[string]any{
		"occurred_at": "2026-05-28T07:05:00+09:00", "source": "test",
		"payload": map[string]any{"weight_kg": -1},
	})
	if !res.IsError {
		t.Fatal("expected invalid payload to error even with a grant")
	}
}
