// Package links parses capability URLs that scope the MCP endpoint. The URL
// itself encodes what's allowed — there is no stored link record — so the owner
// just hands an app a URL like /mcp/meal:w to give "write meal only" and
// /mcp/meal:rw,sleep:r for finer-grained sharing. Inspired by Discord's bot
// invite ?permissions=8: the URL is the capability, the server stays stateless.
//
// Grammar (a single path segment after /mcp/):
//
//	capability := perm ("," perm)*
//	perm       := type ":" ops
//	type       := an OLF type name, e.g. "meal", "x.com.acme.mood", or "*"
//	ops        := one or two of [r w], any order: "r", "w", "rw", "wr"
//
// Examples:
//
//	meal:w          – meal write only
//	meal:rw,sleep:r – meal read+write, sleep read
//	*:r             – read all types
package links

import (
	"errors"
	"regexp"
	"sort"
	"strings"
)

// ErrInvalidCapability is returned by Parse when the capability string is malformed.
var ErrInvalidCapability = errors.New("invalid capability")

// Link is the parsed capability — the set of (type, op) pairs the URL grants.
type Link struct {
	Capability string   // canonical (sorted, deduped) capability string
	Types      []string // types covered, may be "*"
	Ops        []string // "read" and/or "write"

	perms map[string]bool // "<op>:<type>" → present
}

// Allows reports whether op×typ is permitted by this capability.
func (l Link) Allows(op, typ string) bool {
	if l.perms[op+":"+typ] || l.perms[op+":*"] {
		return true
	}
	return false
}

// Scopes returns the OAuth-style "lifelog:<op>:<type>" strings this link covers.
func (l Link) Scopes() []string {
	out := make([]string, 0, len(l.perms))
	for p := range l.perms {
		op, typ, _ := strings.Cut(p, ":")
		out = append(out, "lifelog:"+op+":"+typ)
	}
	sort.Strings(out)
	return out
}

// String returns the canonical capability string (the URL segment).
func (l Link) String() string { return l.Capability }

// typeRE matches an OLF type name or the wildcard "*".
//   - standard names: lowercase + digits + underscore, starting with a letter
//   - reverse-DNS extensions: x.<reverse-dns>.<name> (per spec/type-system.md)
var typeRE = regexp.MustCompile(`^([a-z][a-z0-9_]*|x(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?){2,}|\*)$`)

// Parse turns a capability string (the path segment after /mcp/) into a Link.
// knownTypes is the set the runtime can serve; capabilities for unknown types
// are rejected, with "*" expanded to every known type.
func Parse(capability string, knownTypes []string) (Link, error) {
	if capability == "" {
		return Link{}, ErrInvalidCapability
	}
	known := map[string]bool{}
	for _, t := range knownTypes {
		known[t] = true
	}
	perms := map[string]bool{} // "<op>:<type>"
	types := map[string]bool{}
	ops := map[string]bool{}

	for _, p := range strings.Split(capability, ",") {
		typ, opStr, ok := strings.Cut(p, ":")
		if !ok || typ == "" || opStr == "" || !typeRE.MatchString(typ) {
			return Link{}, ErrInvalidCapability
		}
		if typ != "*" && !known[typ] {
			return Link{}, ErrInvalidCapability
		}
		segOps, err := parseOps(opStr)
		if err != nil {
			return Link{}, err
		}
		expand := []string{typ}
		if typ == "*" {
			expand = knownTypes
		}
		for _, t := range expand {
			types[t] = true
			for _, op := range segOps {
				ops[op] = true
				perms[op+":"+t] = true
			}
		}
		// "*" itself stays in types as well, so Allows can short-circuit.
		if typ == "*" {
			types["*"] = true
			for _, op := range segOps {
				perms[op+":*"] = true
			}
		}
	}

	l := Link{perms: perms}
	for t := range types {
		l.Types = append(l.Types, t)
	}
	for o := range ops {
		l.Ops = append(l.Ops, o)
	}
	sort.Strings(l.Types)
	sort.Strings(l.Ops)
	l.Capability = canonicalize(perms)
	return l, nil
}

func parseOps(s string) ([]string, error) {
	switch s {
	case "r":
		return []string{"read"}, nil
	case "w":
		return []string{"write"}, nil
	case "rw", "wr":
		return []string{"read", "write"}, nil
	default:
		return nil, ErrInvalidCapability
	}
}

// canonicalize produces a stable string form: types alphabetised, each with its
// merged ops compacted to "r" / "w" / "rw". A "*" segment is preferred over the
// expanded list when present (since the same URL was originally written with *).
func canonicalize(perms map[string]bool) string {
	// Group ops by type.
	byType := map[string][]string{}
	for p := range perms {
		op, typ, _ := strings.Cut(p, ":")
		byType[typ] = append(byType[typ], op)
	}
	// If "*" is present, suppress the individual types it covers.
	if starOps, ok := byType["*"]; ok {
		starSet := map[string]bool{}
		for _, o := range starOps {
			starSet[o] = true
		}
		for t, ops := range byType {
			if t == "*" {
				continue
			}
			remaining := ops[:0]
			for _, o := range ops {
				if !starSet[o] {
					remaining = append(remaining, o)
				}
			}
			if len(remaining) == 0 {
				delete(byType, t)
			} else {
				byType[t] = remaining
			}
		}
	}
	types := make([]string, 0, len(byType))
	for t := range byType {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool {
		// "*" sorts first so the wildcard segment is leading and visible.
		if types[i] == "*" {
			return true
		}
		if types[j] == "*" {
			return false
		}
		return types[i] < types[j]
	})
	parts := make([]string, 0, len(types))
	for _, t := range types {
		ops := byType[t]
		sort.Strings(ops)
		short := ""
		for _, o := range ops {
			if o == "read" {
				short += "r"
			} else if o == "write" {
				short += "w"
			}
		}
		parts = append(parts, t+":"+short)
	}
	return strings.Join(parts, ",")
}
