package mcpserver

import (
	"encoding/json"
)

// The tool input schemas are JSON Schema objects advertised to the client (a
// hint for the LLM). The authoritative validation still happens in the core via
// internal/validate when the handler builds and writes the record — these
// schemas are not the enforcement point.

func occurredAtProp() map[string]any {
	return map[string]any{
		"type":        "string",
		"format":      "date-time",
		"description": "when it happened — offset-bearing RFC 3339 (e.g. 2026-05-28T07:05:00+09:00)",
	}
}

func envelopeProps() map[string]any {
	return map[string]any{
		"occurred_at": occurredAtProp(),
		"olf_version": map[string]any{"type": "string", "description": "optional; defaults to the node's current version"},
		"tz":          map[string]any{"type": "string", "description": "optional IANA time zone, e.g. Asia/Tokyo"},
		"source":      map[string]any{"type": "string", "description": "client identifier slug that recorded this"},
	}
}

// splitPayload prepares the type's payload schema for embedding as the "payload"
// property of a tool's input schema. It strips the document-root-only keywords
// ($schema, $id) and lifts $defs out so the caller can place them at the tool
// schema's root — otherwise internal "$ref": "#/$defs/..." (used by meal.items
// and sleep.stages) would dangle, hiding those nested fields from the client.
func splitPayload(payloadSchema json.RawMessage) (payload map[string]any, defs any) {
	var ps map[string]any
	if err := json.Unmarshal(payloadSchema, &ps); err != nil {
		return map[string]any{"type": "object"}, nil
	}
	delete(ps, "$schema")
	delete(ps, "$id")
	defs = ps["$defs"]
	delete(ps, "$defs")
	return ps, defs
}

func recordSchema(_ string, payloadSchema json.RawMessage) map[string]any {
	payload, defs := splitPayload(payloadSchema)
	props := envelopeProps()
	props["payload"] = payload
	s := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"occurred_at", "source", "payload"},
		"properties":           props,
	}
	if defs != nil {
		// Hoisted to the root so "#/$defs/..." references resolve.
		s["$defs"] = defs
	}
	return s
}

func updateSchema(typ string, payloadSchema json.RawMessage) map[string]any {
	s := recordSchema(typ, payloadSchema)
	props := s["properties"].(map[string]any)
	props["id"] = map[string]any{"type": "string", "description": "id of the record to replace"}
	s["required"] = []string{"id", "occurred_at", "source", "payload"}
	return s
}

func idSchema(desc string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"id"},
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "description": desc},
		},
	}
}

func listSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"occurred_from": map[string]any{"type": "string", "format": "date-time", "description": "range start (inclusive), offset-bearing RFC 3339; omit for unbounded"},
			"occurred_to":   map[string]any{"type": "string", "format": "date-time", "description": "range end (inclusive), offset-bearing RFC 3339; omit for unbounded"},
		},
	}
}
