// Package query implements the typed read contract over the store: list a
// type's records within an occurred_at range and fetch one by id. The
// occurred_at range is evaluated by true instant (offset/timezone-independent),
// which is the conformance-critical part (runtime design §4.2). The query
// engine is deliberately a plain scan — no analytical engine is required for
// the v1 read requirements.
package query

import (
	"time"

	"open-lifelog.org/node/internal/olf"
	"open-lifelog.org/node/internal/store"
)

type Engine struct{ store *store.FSStore }

func New(s *store.FSStore) *Engine { return &Engine{store: s} }

// List returns records of typ whose occurred_at instant is within [from, to]
// (inclusive). A nil bound is unbounded on that side.
func (e *Engine) List(typ string, from, to *time.Time) ([]olf.Record, error) {
	var out []olf.Record
	err := e.store.Scan(typ, func(r olf.Record) error {
		inst, err := r.OccurredInstant()
		if err != nil {
			return err
		}
		if from != nil && inst.Before(*from) {
			return nil
		}
		if to != nil && inst.After(*to) {
			return nil
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

// Get returns the record of typ with the given id. When the id appears more
// than once (an update appended a newer record), the latest write wins.
func (e *Engine) Get(typ, id string) (rec olf.Record, found bool, err error) {
	err = e.store.Scan(typ, func(r olf.Record) error {
		if r.ID == id {
			rec, found = r, true
		}
		return nil
	})
	return rec, found, err
}
