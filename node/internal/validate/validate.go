// Package validate enforces the OLF write-time validation contract:
//
//  1. validate the whole record against the envelope schema,
//  2. validate the payload against the type's schema for its major version, and
//  3. enforce the semantic invariants JSON Schema cannot express (cross-field
//     ordering such as ended_at >= occurred_at).
//
// See spec/type-system.md "Validating a record" and spec/types.md. Steps 1–2 are
// "necessary but not sufficient": step 3 is mandatory for the producer. The
// schemas embedded here are copies of the canonical ../schemas, synced by
// `mise run sync-schemas`; the spec and ../schemas remain the single source.
package validate

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"open-lifelog.org/node/internal/olf"
)

//go:embed schemas
var schemaFS embed.FS

// Validator holds the compiled envelope schema and the per-type payload schemas,
// keyed by "<type>/<major>" (e.g. "weight/1"). It is safe for concurrent use.
type Validator struct {
	envelope *jsonschema.Schema
	payload  map[string]*jsonschema.Schema
	// payloadRaw is the raw JSON Schema for each type's latest major, used by
	// surfaces (e.g. MCP) that need to advertise the payload shape.
	payloadRaw map[string]json.RawMessage
	// versions maps each type to the major.minor version declared in its latest
	// schema's $id (e.g. "meal" -> "1.0"). Derived once at New().
	versions map[string]string
}

// New compiles the embedded schemas. Format assertions are enabled so that
// `format: date-time` / `format: uuid` are enforced (matching the conformance
// suite), not treated as annotations.
func New() (*Validator, error) {
	c := jsonschema.NewCompiler()
	c.AssertFormat()

	files, err := fs.Glob(schemaFS, "schemas/*/*.json")
	if err != nil {
		return nil, err
	}

	v := &Validator{
		payload:    map[string]*jsonschema.Schema{},
		payloadRaw: map[string]json.RawMessage{},
		versions:   map[string]string{},
	}

	// Add every schema as a resource first (so any future cross-references
	// resolve), then compile each by the URL we registered it under.
	keyURL := map[string]string{} // "<type>/<major>" -> resource url
	latestMajor := map[string]int{}
	for _, f := range files {
		raw, err := fs.ReadFile(schemaFS, f)
		if err != nil {
			return nil, err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", f, err)
		}
		// schemas/<type>/<major>.json
		typ := path.Base(path.Dir(f))
		major := strings.TrimSuffix(path.Base(f), ".json")
		url := "olf:///" + typ + "/" + major
		if err := c.AddResource(url, doc); err != nil {
			return nil, fmt.Errorf("add %s: %w", f, err)
		}
		keyURL[typ+"/"+major] = url

		if typ != "envelope" {
			if mj, err := strconv.Atoi(major); err == nil && mj >= latestMajor[typ] {
				latestMajor[typ] = mj
				v.payloadRaw[typ] = raw
			}
		}
	}
	for key, url := range keyURL {
		sch, err := c.Compile(url)
		if err != nil {
			return nil, fmt.Errorf("compile %s: %w", key, err)
		}
		if key == "envelope/1" {
			v.envelope = sch
			continue
		}
		v.payload[key] = sch
	}
	if v.envelope == nil {
		return nil, fmt.Errorf("envelope schema not found among embedded schemas")
	}

	// Derive each type's version from its latest schema's $id
	// (e.g. ".../schemas/meal/1.0" -> "1.0"). The $id is the canonical version
	// label for the file; a missing/blank $id is a broken schema — fail closed.
	for typ, raw := range v.payloadRaw {
		var head struct {
			ID string `json:"$id"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, fmt.Errorf("parse $id for %q: %w", typ, err)
		}
		ver := path.Base(head.ID)
		if head.ID == "" || ver == "." || ver == "/" {
			return nil, fmt.Errorf("schema for %q has no usable $id version", typ)
		}
		v.versions[typ] = ver
	}
	return v, nil
}

// Validate runs the full three-step contract against a record. A returned error
// describes the first failing step; nil means the record is safe to persist.
func (v *Validator) Validate(r olf.Record) error {
	// Step 1: the whole record against the envelope schema.
	recBytes, err := json.Marshal(r)
	if err != nil {
		return err
	}
	recAny, err := decode(recBytes)
	if err != nil {
		return err
	}
	if err := v.envelope.Validate(recAny); err != nil {
		return fmt.Errorf("envelope: %w", err)
	}

	// Step 2: the payload against the type's schema for its major version.
	major, err := majorOf(r.OLFVersion)
	if err != nil {
		return err
	}
	sch := v.payload[r.Type+"/"+major]
	if sch == nil {
		return fmt.Errorf("unknown type/version: no schema for %q major %s", r.Type, major)
	}
	payloadAny, err := decode(r.Payload)
	if err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	if err := sch.Validate(payloadAny); err != nil {
		return fmt.Errorf("payload: %w", err)
	}

	// Step 3: semantic invariants the schema cannot express.
	return checkInvariants(r)
}

// PayloadTypes returns the standard type names that have a payload schema
// (envelope excluded), sorted. Surfaces use this to enumerate types without
// hardcoding the set.
func (v *Validator) PayloadTypes() []string {
	types := make([]string, 0, len(v.payloadRaw))
	for t := range v.payloadRaw {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

// TypeVersion pairs a payload type with its latest schema version (major.minor).
type TypeVersion struct {
	Type    string
	Version string
}

// SchemaVersions returns each payload type and its latest schema version,
// sorted by type. Surfaces use this to display what the node can write.
func (v *Validator) SchemaVersions() []TypeVersion {
	out := make([]TypeVersion, 0, len(v.versions))
	for typ, ver := range v.versions {
		out = append(out, TypeVersion{Type: typ, Version: ver})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// LatestVersion returns the latest schema version for typ and whether typ is
// known. Used to stamp olf_version on writes that omit it.
func (v *Validator) LatestVersion(typ string) (string, bool) {
	ver, ok := v.versions[typ]
	return ver, ok
}

// RawPayloadSchema returns the raw JSON Schema for the type's latest major, and
// whether the type is known.
func (v *Validator) RawPayloadSchema(typ string) (json.RawMessage, bool) {
	raw, ok := v.payloadRaw[typ]
	return raw, ok
}

// decode parses a JSON document into the generic value jsonschema validates
// against (numbers become json.Number so integer constraints are exact).
func decode(b []byte) (any, error) {
	return jsonschema.UnmarshalJSON(bytes.NewReader(b))
}

// majorOf returns the major component of an "M.m" olf_version. The envelope
// schema has already constrained the shape by the time invariants run, but
// majorOf is also called defensively, so it validates rather than assumes.
func majorOf(version string) (string, error) {
	major, _, ok := strings.Cut(version, ".")
	if !ok || major == "" {
		return "", fmt.Errorf("invalid olf_version %q (want major.minor)", version)
	}
	return major, nil
}
