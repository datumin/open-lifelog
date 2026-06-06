// Package write is the core write usecase: the single path through which every
// surface (MCP write tools, the HTTP write REST API, CLI import) persists a
// record. It validates, mints the id and recorded_at, then appends — no surface
// duplicates this logic (runtime design §5). The store and query layers are
// deliberately separate; this package owns only the act of writing.
package write

import (
	"encoding/json"
	"errors"
	"time"

	"open-lifelog.org/node/internal/olf"
	"open-lifelog.org/node/internal/store"
	"open-lifelog.org/node/internal/validate"
)

// ErrNotFound is returned by Update and Delete when no record of the given type
// has the given id.
var ErrNotFound = errors.New("record not found")

// Service records OLF records into the store after validating them.
type Service struct {
	store     *store.FSStore
	validator *validate.Validator
	now       func() time.Time // injectable for deterministic ids/recorded_at
}

// New builds a write Service. now may be nil, in which case time.Now is used.
func New(s *store.FSStore, v *validate.Validator, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: s, validator: v, now: now}
}

// RecordInput is what a surface supplies to record a new observation. The
// envelope fields the node owns — id and recorded_at — are not part of the
// input; the node mints them.
type RecordInput struct {
	Type       string
	OLFVersion string
	OccurredAt string
	TZ         string
	Source     string
	Payload    json.RawMessage
}

// Record validates the input, mints a time-ordered id and recorded_at, appends
// the record, and returns it. The record is persisted only if validation passes;
// a validation error leaves the store untouched.
func (s *Service) Record(in RecordInput) (olf.Record, error) {
	now := s.now()
	id, err := olf.NewUUIDv7(now)
	if err != nil {
		return olf.Record{}, err
	}
	r := olf.Record{
		ID:         id,
		Type:       in.Type,
		OLFVersion: in.OLFVersion,
		OccurredAt: in.OccurredAt,
		RecordedAt: now.Format(time.RFC3339Nano),
		TZ:         in.TZ,
		Source:     in.Source,
		Payload:    in.Payload,
	}
	if err := s.validator.Validate(r); err != nil {
		return olf.Record{}, err
	}
	if err := s.store.Append(r); err != nil {
		return olf.Record{}, err
	}
	return r, nil
}

// UpdateInput is the full desired state of an existing record, identified by
// Type+ID. As with Record, the node owns recorded_at (it is re-minted to the
// edit time); the id is preserved.
type UpdateInput struct {
	Type       string
	ID         string
	OLFVersion string
	OccurredAt string
	TZ         string
	Source     string
	Payload    json.RawMessage
}

// Update validates the new state and rewrites the existing record in place
// (spec/on-disk.md edit model: the type's file is rewritten, so the id still
// appears exactly once). It returns ErrNotFound if the id does not exist; on a
// validation error the store is left untouched.
func (s *Service) Update(in UpdateInput) (olf.Record, error) {
	r := olf.Record{
		ID:         in.ID,
		Type:       in.Type,
		OLFVersion: in.OLFVersion,
		OccurredAt: in.OccurredAt,
		RecordedAt: s.now().Format(time.RFC3339Nano),
		TZ:         in.TZ,
		Source:     in.Source,
		Payload:    in.Payload,
	}
	if err := s.validator.Validate(r); err != nil {
		return olf.Record{}, err
	}
	found, err := s.store.ReplaceByID(in.Type, in.ID, &r)
	if err != nil {
		return olf.Record{}, err
	}
	if !found {
		return olf.Record{}, ErrNotFound
	}
	return r, nil
}

// Delete removes the record of typ with the given id by rewriting the type's
// file without it (no tombstone). It returns ErrNotFound if the id does not exist.
func (s *Service) Delete(typ, id string) error {
	found, err := s.store.ReplaceByID(typ, id, nil)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	return nil
}
