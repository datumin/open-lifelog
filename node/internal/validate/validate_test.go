package validate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"open-lifelog.org/node/internal/olf"
)

// conformanceDir is the repo-root, language-neutral fixture set. The runtime's
// schema validation MUST agree with it (envelope fixtures are whole records;
// per-type fixtures are payloads).
const conformanceDir = "../../../conformance"

func newValidator(t *testing.T) *Validator {
	t.Helper()
	v, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

// TestConformanceFixtures cross-checks the embedded schemas against the shared
// accept/reject golden vectors: every "valid" fixture must pass and every
// "invalid" fixture must fail the relevant schema.
func TestConformanceFixtures(t *testing.T) {
	v := newValidator(t)

	types, err := os.ReadDir(conformanceDir)
	if err != nil {
		t.Fatalf("read %s: %v", conformanceDir, err)
	}

	checked := 0
	for _, td := range types {
		if !td.IsDir() {
			continue
		}
		typ := td.Name()
		schema := v.envelope
		if typ != "envelope" {
			schema = v.payload[typ+"/1"]
			if schema == nil {
				t.Fatalf("no payload schema compiled for type %q", typ)
			}
		}

		for _, validity := range []string{"valid", "invalid"} {
			dir := filepath.Join(conformanceDir, typ, validity)
			files, err := os.ReadDir(dir)
			if err != nil {
				continue // a type may have only one of the two buckets
			}
			for _, f := range files {
				if !strings.HasSuffix(f.Name(), ".json") {
					continue
				}
				name := typ + "/" + validity + "/" + f.Name()
				raw, err := os.ReadFile(filepath.Join(dir, f.Name()))
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				inst, err := decode(raw)
				if err != nil {
					t.Fatalf("%s: decode: %v", name, err)
				}
				err = schema.Validate(inst)
				switch validity {
				case "valid":
					if err != nil {
						t.Errorf("%s: expected valid, got: %v", name, err)
					}
				case "invalid":
					if err == nil {
						t.Errorf("%s: expected invalid, but it passed", name)
					}
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no conformance fixtures were checked")
	}
	t.Logf("checked %d conformance fixtures", checked)
}

// rec builds a full record with a fixed valid recorded_at/id/source so tests can
// vary only the fields under test.
func rec(typ, olfVersion, occurredAt, payload string) olf.Record {
	return olf.Record{
		ID:         "0192f3a0-7b1e-7c3d-8e4f-0a1b2c3d4e5f",
		Type:       typ,
		OLFVersion: olfVersion,
		OccurredAt: occurredAt,
		RecordedAt: "2026-05-29T07:10:00+09:00",
		Source:     "test",
		Payload:    json.RawMessage(payload),
	}
}

func TestSchemaVersions(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tv := range v.SchemaVersions() {
		got[tv.Type] = tv.Version
	}
	for _, typ := range []string{"meal", "sleep", "steps", "weight"} {
		if got[typ] != "1.0" {
			t.Errorf("SchemaVersions()[%q] = %q, want %q", typ, got[typ], "1.0")
		}
	}
	if _, ok := got["envelope"]; ok {
		t.Errorf("SchemaVersions() should not include envelope")
	}
}

func TestLatestVersion(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if ver, ok := v.LatestVersion("meal"); !ok || ver != "1.0" {
		t.Errorf("LatestVersion(meal) = %q,%v want 1.0,true", ver, ok)
	}
	if ver, ok := v.LatestVersion("nope"); ok || ver != "" {
		t.Errorf("LatestVersion(nope) = %q,%v want \"\",false", ver, ok)
	}
}

func TestVersionShapeGuard(t *testing.T) {
	good := []string{"1.0", "2.3", "10.0"}
	bad := []string{"", "meal", "1.0?v=2", "1.0#frag", "v1.0", "1", "1.0.0"}
	for _, s := range good {
		if !versionRe.MatchString(s) {
			t.Errorf("versionRe rejected good version %q", s)
		}
	}
	for _, s := range bad {
		if versionRe.MatchString(s) {
			t.Errorf("versionRe accepted bad version %q", s)
		}
	}
}

func TestValidateFullRecord_OK(t *testing.T) {
	v := newValidator(t)
	r := rec("weight", "1.0", "2026-05-28T07:05:00+09:00", `{"weight_kg":70.5}`)
	if err := v.Validate(r); err != nil {
		t.Fatalf("expected valid record, got: %v", err)
	}
}

func TestValidateUnknownType(t *testing.T) {
	v := newValidator(t)
	r := rec("x.com.acme.mood", "1.0", "2026-05-28T23:00:00+09:00", `{}`)
	err := v.Validate(r)
	if err == nil || !strings.Contains(err.Error(), "unknown type/version") {
		t.Fatalf("expected unknown-type error, got: %v", err)
	}
}

func TestValidateUnknownMajor(t *testing.T) {
	v := newValidator(t)
	r := rec("weight", "9.0", "2026-05-28T07:05:00+09:00", `{"weight_kg":70.5}`)
	if err := v.Validate(r); err == nil {
		t.Fatal("expected error for major with no schema")
	}
}

func TestValidateEnvelopeRejectsOffsetlessOccurredAt(t *testing.T) {
	v := newValidator(t)
	r := rec("weight", "1.0", "2026-05-28T07:05:00", `{"weight_kg":70.5}`) // no offset
	err := v.Validate(r)
	if err == nil || !strings.Contains(err.Error(), "envelope") {
		t.Fatalf("expected envelope/format error for offset-less occurred_at, got: %v", err)
	}
}

func TestStepsInvariant_EndedAtBeforeOccurredAt(t *testing.T) {
	v := newValidator(t)
	// occurred (interval start) after ended → invalid.
	r := rec("steps", "1.0", "2026-05-28T10:00:00+09:00",
		`{"count":100,"ended_at":"2026-05-28T09:00:00+09:00"}`)
	if err := v.Validate(r); err == nil {
		t.Fatal("expected ended_at < occurred_at to be rejected")
	}
}

func TestStepsInvariant_TrueInstantEquality(t *testing.T) {
	v := newValidator(t)
	// Same instant expressed with different offsets (00:00Z == 09:00+09:00).
	// ended_at >= occurred_at holds by equality, so this MUST pass.
	r := rec("steps", "1.0", "2026-05-28T09:00:00+09:00",
		`{"count":100,"ended_at":"2026-05-28T00:00:00Z"}`)
	if err := v.Validate(r); err != nil {
		t.Fatalf("expected equal instants (different offsets) to pass, got: %v", err)
	}
}

func TestSleepInvariant_EndedBeforeOccurred(t *testing.T) {
	v := newValidator(t)
	r := rec("sleep", "1.0", "2026-05-29T08:00:00+09:00",
		`{"ended_at":"2026-05-29T07:00:00+09:00"}`)
	if err := v.Validate(r); err == nil {
		t.Fatal("expected sleep ended_at < occurred_at to be rejected")
	}
}

func TestSleepInvariant_StageEndedBeforeStarted(t *testing.T) {
	v := newValidator(t)
	r := rec("sleep", "1.0", "2026-05-28T23:00:00+09:00", `{
		"ended_at":"2026-05-29T07:00:00+09:00",
		"stages":[{"stage":"core","started_at":"2026-05-29T01:00:00+09:00","ended_at":"2026-05-29T00:00:00+09:00"}]
	}`)
	if err := v.Validate(r); err == nil {
		t.Fatal("expected stage ended_at < started_at to be rejected")
	}
}

func TestSleepInvariant_StagesOutOfOrder(t *testing.T) {
	v := newValidator(t)
	r := rec("sleep", "1.0", "2026-05-28T23:00:00+09:00", `{
		"ended_at":"2026-05-29T07:00:00+09:00",
		"stages":[
			{"stage":"deep","started_at":"2026-05-29T02:00:00+09:00","ended_at":"2026-05-29T03:00:00+09:00"},
			{"stage":"core","started_at":"2026-05-29T01:00:00+09:00","ended_at":"2026-05-29T01:30:00+09:00"}
		]
	}`)
	if err := v.Validate(r); err == nil {
		t.Fatal("expected out-of-order stages to be rejected")
	}
}

func TestSleepInvariant_ValidOrderedOverlappingStages(t *testing.T) {
	v := newValidator(t)
	// Ordered by started_at; stages may overlap and need not cover the session.
	r := rec("sleep", "1.0", "2026-05-28T23:00:00+09:00", `{
		"ended_at":"2026-05-29T07:00:00+09:00",
		"stages":[
			{"stage":"in_bed","started_at":"2026-05-28T23:00:00+09:00","ended_at":"2026-05-29T07:00:00+09:00"},
			{"stage":"core","started_at":"2026-05-28T23:10:00+09:00","ended_at":"2026-05-29T01:00:00+09:00"}
		]
	}`)
	if err := v.Validate(r); err != nil {
		t.Fatalf("expected ordered/overlapping stages to pass, got: %v", err)
	}
}
