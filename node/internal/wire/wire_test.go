package wire

import (
	"encoding/json"
	"testing"
	"time"
)

func TestListEnvelopeShape(t *testing.T) {
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 6, 23, 59, 59, 0, time.UTC)
	env := Envelope{
		Data: []string{},
		Meta: ListMeta{
			RequestedRange: MakeRange(&from, nil),
			EffectiveRange: MakeRange(&from, &to),
			Clipped:        true,
			Warnings:       []Warning{{Code: CodeRangeClippedByScope, Message: "clipped"}},
		},
	}
	b, _ := json.Marshal(env)
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	meta := got["meta"].(map[string]any)
	for _, k := range []string{"requested_range", "effective_range", "clipped", "warnings"} {
		if _, ok := meta[k]; !ok {
			t.Errorf("meta missing %q: %s", k, b)
		}
	}
	if meta["clipped"] != true {
		t.Errorf("clipped should be true: %s", b)
	}
	rr := meta["requested_range"].(map[string]any)
	if rr["to"] != nil {
		t.Errorf("unbounded requested.to should be null, got %v", rr["to"])
	}
}

func TestErrorEnvelopeShape(t *testing.T) {
	occ := "2026-06-05T12:00:00+09:00"
	from := time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)
	env := ErrorEnvelope{Error: ErrorBody{
		Code:              CodeOutOfReadScope,
		Message:           "exists but outside your read window",
		GrantedReadWindow: &Range{From: TimePtr(&from), To: nil},
		RecordOccurredAt:  StrPtr(occ),
	}}
	b, _ := json.Marshal(env)
	var got map[string]any
	json.Unmarshal(b, &got)
	e := got["error"].(map[string]any)
	if e["code"] != CodeOutOfReadScope || e["granted_read_window"] == nil || e["record_occurred_at"] != occ {
		t.Errorf("error body shape wrong: %s", b)
	}
	// A bare error omits the optional fields entirely.
	b2, _ := json.Marshal(ErrorEnvelope{Error: ErrorBody{Code: CodeNotFound, Message: "nope"}})
	if string(b2) == "" || contains(string(b2), "granted_read_window") {
		t.Errorf("bare error must omit window: %s", b2)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
