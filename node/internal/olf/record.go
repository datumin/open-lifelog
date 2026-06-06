// Package olf defines the OLF record (common envelope + typed payload) and the
// small primitives the runtime needs around it: id minting and the occurred_at
// instant semantics. See docs/design/olf-runtime-design and the format spec
// under ../spec.
package olf

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"
)

// Record is a single OLF record. The envelope fields are common to all types;
// payload is type-specific and validated against the type's JSON Schema.
type Record struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	OLFVersion string          `json:"olf_version"`
	OccurredAt string          `json:"occurred_at"`
	RecordedAt string          `json:"recorded_at"`
	TZ         string          `json:"tz,omitempty"`
	Source     string          `json:"source"`
	Payload    json.RawMessage `json:"payload"`
}

// OccurredInstant parses OccurredAt as a true instant (see ParseInstant).
func (r Record) OccurredInstant() (time.Time, error) { return ParseInstant(r.OccurredAt) }

// ParseInstant parses an offset-bearing RFC 3339 timestamp into an instant.
//
// Comparison is offset/timezone-independent: two timestamps that denote the
// same instant are equal regardless of their textual offset. The input MUST
// carry an offset (Z or ±hh:mm); offset-less ("naive") timestamps are rejected
// rather than guessed. See runtime design §4.2 — this is conformance-critical.
func ParseInstant(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("occurred_at must be an offset-bearing RFC 3339 timestamp: %w", err)
	}
	return t, nil
}

// NewUUIDv7 returns a UUIDv7 string for the given time. The leading 48 bits are
// a Unix-millisecond timestamp, so ids sort by creation time and appends land
// at the tail. The writer (this node) mints ids; an external service never does.
func NewUUIDv7(now time.Time) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	ms := uint64(now.UnixMilli())
	b[0], b[1], b[2] = byte(ms>>40), byte(ms>>32), byte(ms>>24)
	b[3], b[4], b[5] = byte(ms>>16), byte(ms>>8), byte(ms)
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
