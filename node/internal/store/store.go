// Package store is the append-only, local-filesystem JSONL store for OLF
// records. The lifelog (these JSONL files) is the canonical data; the store
// holds no derived state. Records are partitioned by type and the local calendar
// date of occurred_at: <root>/<type>/<YYYY>/<MM>/<YYYY-MM-DD>.jsonl
// (spec/on-disk.md).
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"open-lifelog.org/node/internal/olf"
)

// ErrInvalidType is returned when a type name is not a safe single path segment
// and therefore must never be turned into a file path (it could escape the
// root). Callers are expected to pass only validated OLF type names; this is a
// last-line guard so a bad type can never traverse the filesystem.
var ErrInvalidType = errors.New("invalid type")

// FSStore appends records to, and scans records from, the local filesystem.
type FSStore struct {
	root string
	mu   sync.Mutex // serialize appends: single-writer per process
}

func NewFSStore(root string) *FSStore { return &FSStore{root: root} }

// safeType reports whether typ maps to a single directory directly under root —
// i.e. it has no path separator and is not "", ".", or ".." — so that the
// derived path cannot escape the root via traversal.
func safeType(typ string) bool {
	if typ == "" || typ == "." || typ == ".." {
		return false
	}
	return !strings.ContainsRune(typ, '/') && !strings.ContainsRune(typ, filepath.Separator)
}

// dayOf returns the local calendar date (YYYY-MM-DD) of an offset-bearing
// occurred_at. The date comes directly from the textual date portion — because
// occurred_at carries an offset, its date portion already is the local
// wall-clock date, so no time-zone math is needed (spec/on-disk.md).
func dayOf(occurredAt string) (string, error) {
	if len(occurredAt) < 10 {
		return "", fmt.Errorf("occurred_at %q has no date portion", occurredAt)
	}
	date := occurredAt[:10]
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return "", fmt.Errorf("occurred_at %q has an invalid date portion: %w", occurredAt, err)
	}
	return date, nil
}

// pathForDay maps (type, YYYY-MM-DD) to its partition file. The caller must have
// validated typ with safeType and date with dayOf.
func (s *FSStore) pathForDay(typ, date string) string {
	return filepath.Join(s.root, typ, date[0:4], date[5:7], date+".jsonl")
}

// typeDir is the root of a type's partitions: <root>/<type>.
func (s *FSStore) typeDir(typ string) string { return filepath.Join(s.root, typ) }

// dayFiles returns the type's partition files in chronological order. A missing
// type dir yields no files. The YYYY/MM/YYYY-MM-DD names are zero-padded, so a
// lexical sort is chronological.
func (s *FSStore) dayFiles(typ string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(s.typeDir(typ), "*", "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

// Append writes one record as a JSON line to its day partition and fsyncs,
// giving read-after-write durability.
func (s *FSStore) Append(r olf.Record) error {
	if !safeType(r.Type) {
		return ErrInvalidType
	}
	day, err := dayOf(r.OccurredAt)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	p := s.pathForDay(r.Type, day)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// ReplaceByID rewrites the partition(s) holding the record with the given id so
// that it is replaced by rep (an edit) or removed entirely (a delete, when rep
// is nil). It returns found=false and leaves the store untouched if no record
// has that id.
//
// This is the canonical edit/delete model (spec/on-disk.md): a day file is
// rewritten so a given id appears at most once and no tombstones accumulate. An
// edit that changes occurred_at to a different day moves the record to the new
// day partition. Each file rewrite is atomic (temp file + rename); when a move
// touches two files the target is written before the source is cleared, so a
// concurrent lock-free Scan may transiently see the record twice (collapsed by
// the reader's latest-wins) but never zero times.
func (s *FSStore) ReplaceByID(typ, id string, rep *olf.Record) (found bool, err error) {
	if !safeType(typ) {
		return false, ErrInvalidType
	}
	var targetDay string
	if rep != nil {
		targetDay, err = dayOf(rep.OccurredAt)
		if err != nil {
			return false, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	files, err := s.dayFiles(typ)
	if err != nil {
		return false, err
	}

	// Read each partition, dropping every occurrence of id (a corrupt/imported
	// log could hold duplicates, possibly across days). Track which files change.
	kept := map[string][]olf.Record{}
	affected := map[string]bool{}
	for _, f := range files {
		recs, err := readRecords(f)
		if err != nil {
			return false, err
		}
		out := recs[:0]
		for _, r := range recs {
			if r.ID == id {
				found = true
				affected[f] = true
				continue
			}
			out = append(out, r)
		}
		kept[f] = out
	}
	if !found {
		return false, nil // no match: leave the store untouched
	}

	// Insert the replacement into its target day, then rewrite the target before
	// clearing the (possibly different) source files.
	if rep != nil {
		target := s.pathForDay(typ, targetDay)
		if _, seen := kept[target]; !seen {
			kept[target] = nil
		}
		kept[target] = append(kept[target], *rep)
		affected[target] = true
		if err := s.writeOrPrune(target, kept[target]); err != nil {
			return found, err
		}
		delete(affected, target)
	}
	for f := range affected {
		if err := s.writeOrPrune(f, kept[f]); err != nil {
			return found, err
		}
	}
	return true, nil
}

// writeOrPrune atomically replaces p with recs, or removes p (and prunes its now
// empty parent dirs up to the type dir) when recs is empty, keeping the
// canonical tree free of empty partitions.
func (s *FSStore) writeOrPrune(p string, recs []olf.Record) error {
	if len(recs) == 0 {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return s.pruneEmptyDirs(filepath.Dir(p))
	}
	return writeAll(p, recs)
}

// pruneEmptyDirs removes empty directories from dir upward, stopping at (and not
// removing) the store root. A non-empty dir or the root ends the walk.
func (s *FSStore) pruneEmptyDirs(dir string) error {
	root := filepath.Clean(s.root)
	for {
		d := filepath.Clean(dir)
		if d == root || !strings.HasPrefix(d, root+string(filepath.Separator)) {
			return nil
		}
		if err := os.Remove(d); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			// Directory not empty (or other error): stop pruning, not an error.
			return nil
		}
		dir = filepath.Dir(d)
	}
}

// readRecords reads all records from one partition file in append order. A
// missing file yields no records.
func readRecords(p string) ([]olf.Record, error) {
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []olf.Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		var r olf.Record
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// writeAll atomically replaces the file at p with the given records, one JSON
// line each: write a sibling temp file, fsync it, then rename over p.
func writeAll(p string, recs []olf.Record) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".rewrite-*.jsonl")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed; cleanup on any error path

	w := bufio.NewWriter(tmp)
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			tmp.Close()
			return err
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// Scan calls fn for each record of typ, in chronological partition order (then
// append order within a partition), stopping early if fn returns an error. A
// missing log is treated as "no records". An unsafe type (one that could escape
// the root) is rejected outright. ReplaceByID routes its reads through the same
// partition files, so it is guarded too.
func (s *FSStore) Scan(typ string, fn func(olf.Record) error) error {
	if !safeType(typ) {
		return ErrInvalidType
	}
	files, err := s.dayFiles(typ)
	if err != nil {
		return err
	}
	for _, p := range files {
		recs, err := readRecords(p)
		if err != nil {
			return err
		}
		for _, r := range recs {
			if err := fn(r); err != nil {
				return err
			}
		}
	}
	return nil
}
