package incident

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when an incident ID is unknown.
var ErrNotFound = errors.New("incident not found")

// FileStore persists one JSON file per incident under DataDir.
//
// It is intentionally simple: the MVP needs durable, inspectable incident
// records, not a database. Because incident IDs are derived from the source
// key, the directory listing is the index; no separate index file is needed.
type FileStore struct {
	dir string
	mu  sync.Mutex
}

// NewFileStore creates the directory if needed.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return &FileStore{dir: dir}, nil
}

// Upsert loads the incident for key (if any), lets mutate produce the new
// state, and writes it atomically. created reports whether the incident was
// new. The whole sequence runs under the store lock, which makes concurrent
// redeliveries of the same alert safe.
func (s *FileStore) Upsert(key string, mutate func(existing *Incident) (*Incident, error)) (inc *Incident, created bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := IDForKey(key)
	existing, err := s.readLocked(id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	created = existing == nil

	inc, err = mutate(existing)
	if err != nil {
		return nil, false, err
	}
	if inc == nil {
		return nil, false, errors.New("mutate returned nil incident")
	}
	if inc.IncidentID != id {
		return nil, false, fmt.Errorf("incident id mismatch: %s != %s", inc.IncidentID, id)
	}
	if err := s.writeLocked(inc); err != nil {
		return nil, false, err
	}
	return inc, created, nil
}

// ErrNoChange can be returned by an Update mutation to skip the write.
var ErrNoChange = errors.New("no change")

// Update loads an existing incident, applies mutate and saves the result
// under the store lock. If mutate returns ErrNoChange the incident is
// returned unchanged and nothing is written.
func (s *FileStore) Update(id string, mutate func(inc *Incident) error) (*Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	inc, err := s.readLocked(id)
	if err != nil {
		return nil, err
	}
	if err := mutate(inc); err != nil {
		if errors.Is(err, ErrNoChange) {
			return inc, nil
		}
		return nil, err
	}
	if err := s.writeLocked(inc); err != nil {
		return nil, err
	}
	return inc, nil
}

// Correlate assigns a correlation group to incident id, under the store lock
// so concurrent webhooks cannot create two primaries for one group.
//
// An eligible incident joins the earliest eligible primary with the same
// CorrelationKey whose StartsAt is within window of its own; otherwise it
// becomes a primary itself. Ineligible incidents (e.g. smoke rules) are
// always their own primary and never become a group's primary, so a smoke
// alert cannot swallow a real scenario incident.
func (s *FileStore) Correlate(id string, window time.Duration, eligible func(*Incident) bool) (*Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	inc, err := s.readLocked(id)
	if err != nil {
		return nil, err
	}
	if inc.CorrelationID != "" {
		return inc, nil
	}
	key := inc.CorrelationKey()
	if key == "" || window <= 0 || (eligible != nil && !eligible(inc)) {
		inc.CorrelationID = inc.IncidentID
		return inc, s.writeLocked(inc)
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list data dir: %w", err)
	}
	var primary *Incident
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "inc-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		c, err := s.readLocked(strings.TrimSuffix(name, ".json"))
		if err != nil || c.IncidentID == inc.IncidentID || c.IsGroupMember() || c.CorrelationID == "" {
			continue
		}
		if eligible != nil && !eligible(c) {
			continue
		}
		if c.CorrelationKey() != key {
			continue
		}
		d := inc.StartsAt.Sub(c.StartsAt)
		if d < -window || d > window {
			continue
		}
		if primary == nil || c.StartsAt.Before(primary.StartsAt) {
			primary = c
		}
	}

	if primary == nil {
		inc.CorrelationID = inc.IncidentID
		return inc, s.writeLocked(inc)
	}
	inc.CorrelationID = primary.IncidentID
	primary.CorrelatedIncidents = append(primary.CorrelatedIncidents, inc.IncidentID)
	if err := s.writeLocked(primary); err != nil {
		return nil, err
	}
	return inc, s.writeLocked(inc)
}

// Members returns the incidents whose CorrelationID is primaryID, excluding
// the primary itself.
func (s *FileStore) Members(primaryID string) ([]*Incident, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var out []*Incident
	for _, i := range all {
		if i.CorrelationID == primaryID && i.IncidentID != primaryID {
			out = append(out, i)
		}
	}
	return out, nil
}

// Save writes an incident unconditionally.
func (s *FileStore) Save(inc *Incident) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked(inc)
}

// Get returns one incident by ID.
func (s *FileStore) Get(id string) (*Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked(id)
}

// List returns all incidents, newest first by StartsAt.
func (s *FileStore) List() ([]*Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list data dir: %w", err)
	}
	out := make([]*Incident, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || !strings.HasPrefix(name, "inc-") {
			continue
		}
		inc, err := s.readLocked(strings.TrimSuffix(name, ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartsAt.After(out[j].StartsAt) })
	return out, nil
}

func (s *FileStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

func (s *FileStore) readLocked(id string) (*Incident, error) {
	if !validID(id) {
		return nil, ErrNotFound
	}
	raw, err := os.ReadFile(s.path(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read incident %s: %w", id, err)
	}
	var inc Incident
	if err := json.Unmarshal(raw, &inc); err != nil {
		return nil, fmt.Errorf("decode incident %s: %w", id, err)
	}
	return &inc, nil
}

// writeLocked writes to a temp file then renames so a crash never leaves a
// half-written incident behind.
func (s *FileStore) writeLocked(inc *Incident) error {
	if !validID(inc.IncidentID) {
		return fmt.Errorf("invalid incident id %q", inc.IncidentID)
	}
	raw, err := json.MarshalIndent(inc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode incident: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.path(inc.IncidentID)); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

// validID guards against path traversal through the HTTP path parameter.
func validID(id string) bool {
	if !strings.HasPrefix(id, "inc-") || len(id) != len("inc-")+12 {
		return false
	}
	for _, r := range id[len("inc-"):] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}
