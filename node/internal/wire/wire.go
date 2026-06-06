// Package wire defines the JSON response envelopes shared by the REST and MCP
// surfaces, so a record read or write returns the SAME machine-readable shape on
// both. The envelope exists to make implicit behavior explicit: a list can report
// that its range was clipped by the read window (0 results = not visible, not
// "nothing happened"), and a write can warn that the record landed outside the
// caller's read window (saved but unreadable). Errors carry a stable `code` plus,
// for an out-of-window read, the granted window and the record's occurred_at.
package wire

import "time"

// Range is an occurred_at interval; a nil bound is unbounded on that side.
type Range struct {
	From *string `json:"from"`
	To   *string `json:"to"`
}

// Warning is a non-fatal advisory with a stable machine code.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Stable warning / error codes (the contract clients branch on).
const (
	CodeRangeClippedByScope     = "range_clipped_by_scope"
	CodeWrittenOutsideReadWindow = "written_outside_read_window"
	CodeNotFound                = "not_found"
	CodeOutOfReadScope          = "out_of_read_scope"
	CodeBadRequest              = "bad_request"
	CodeUnauthenticated         = "unauthenticated"
	CodeForbidden               = "forbidden"
	CodeInternal                = "internal"
)

// Envelope wraps a successful read/write result with its metadata.
type Envelope struct {
	Data any `json:"data"`
	Meta any `json:"meta"`
}

// ListMeta accompanies a list result: what the caller asked for, what was
// actually served after the read window, and whether the window clipped it.
type ListMeta struct {
	RequestedRange Range     `json:"requested_range"`
	EffectiveRange Range     `json:"effective_range"`
	Clipped        bool      `json:"clipped"`
	Warnings       []Warning `json:"warnings"`
}

// OpMeta accompanies a single-record read or a write: just any warnings.
type OpMeta struct {
	Warnings []Warning `json:"warnings"`
}

// ErrorBody is the value under the top-level "error" key. GrantedReadWindow and
// RecordOccurredAt are present only for an out-of-read-scope read.
type ErrorBody struct {
	Code              string  `json:"code"`
	Message           string  `json:"message"`
	GrantedReadWindow *Range  `json:"granted_read_window,omitempty"`
	RecordOccurredAt  *string `json:"record_occurred_at,omitempty"`
}

// ErrorEnvelope is the top-level error response shape, on both surfaces.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// TimePtr formats an optional instant as an RFC 3339 (nanosecond) string, or nil.
func TimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

// MakeRange builds a Range from optional instants.
func MakeRange(from, to *time.Time) Range {
	return Range{From: TimePtr(from), To: TimePtr(to)}
}

// StrPtr returns &s, or nil for the empty string.
func StrPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
