package validate

import (
	"encoding/json"
	"fmt"
	"time"

	"open-lifelog.org/node/internal/olf"
)

// checkInvariants enforces the per-type MUST semantic invariants from
// spec/types.md that JSON Schema cannot express (relational comparisons between
// date-time fields). Types with no cross-field invariants (weight, meal) and
// unknown/extension types fall through as valid — schema validation already ran.
//
// SHOULD-level rules (e.g. sleep stages falling within the session interval) are
// deliberately not enforced here; only MUST-level invariants are.
func checkInvariants(r olf.Record) error {
	switch r.Type {
	case "sleep":
		return sleepInvariants(r)
	case "steps":
		return stepsInvariants(r)
	default:
		return nil
	}
}

// intervalEnd is the shape shared by interval payloads (steps, sleep) that carry
// an ended_at alongside the envelope's occurred_at (the interval start).
type intervalEnd struct {
	EndedAt string `json:"ended_at"`
}

// stepsInvariants: ended_at MUST be >= occurred_at (spec/types.md steps).
func stepsInvariants(r olf.Record) error {
	var p intervalEnd
	if err := json.Unmarshal(r.Payload, &p); err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	return notBefore("ended_at", p.EndedAt, "occurred_at", r.OccurredAt)
}

type sleepPayload struct {
	EndedAt string       `json:"ended_at"`
	Stages  []sleepStage `json:"stages"`
}

type sleepStage struct {
	Stage     string `json:"stage"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at"`
}

// sleepInvariants enforces the sleep MUST invariants (spec/types.md):
//   - ended_at >= occurred_at,
//   - every stage's ended_at >= started_at,
//   - stages are ordered chronologically by started_at.
//
// Stages MAY overlap and need not cover the session, so no coverage/overlap
// check is made.
func sleepInvariants(r olf.Record) error {
	var p sleepPayload
	if err := json.Unmarshal(r.Payload, &p); err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	if err := notBefore("ended_at", p.EndedAt, "occurred_at", r.OccurredAt); err != nil {
		return err
	}
	var prev time.Time
	for i, s := range p.Stages {
		if err := notBefore(
			fmt.Sprintf("stages[%d].ended_at", i), s.EndedAt,
			fmt.Sprintf("stages[%d].started_at", i), s.StartedAt,
		); err != nil {
			return err
		}
		start, err := olf.ParseInstant(s.StartedAt)
		if err != nil {
			return fmt.Errorf("stages[%d].started_at: %w", i, err)
		}
		if i > 0 && start.Before(prev) {
			return fmt.Errorf("stages must be ordered by started_at: stages[%d] starts before stages[%d]", i, i-1)
		}
		prev = start
	}
	return nil
}

// notBefore returns an error unless the instant at laterField is at or after the
// instant at earlierField. Both are compared as true instants (offset-independent).
func notBefore(laterField, later, earlierField, earlier string) error {
	lt, err := olf.ParseInstant(later)
	if err != nil {
		return fmt.Errorf("%s: %w", laterField, err)
	}
	et, err := olf.ParseInstant(earlier)
	if err != nil {
		return fmt.Errorf("%s: %w", earlierField, err)
	}
	if lt.Before(et) {
		return fmt.Errorf("%s (%s) must be >= %s (%s)", laterField, later, earlierField, earlier)
	}
	return nil
}
