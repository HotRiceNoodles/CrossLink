package debug

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type Entry struct {
	ID          string
	OrgID       int64
	Timestamp   time.Time
	Duration    time.Duration
	Method      string
	Path        string
	Model       string
	Stream      bool
	Truncated   bool
	ReqHeaders  http.Header
	ReqBody     []byte
	RespStatus   int
	RespHeaders  http.Header
	RespBody     []byte
	InputTokens  int
	OutputTokens int
}

type Store struct {
	mu          sync.Mutex
	entries     []*Entry
	capacity    int
	enabled     atomic.Bool
	maxBodySize int
}

func NewStore(capacity, maxBodySize int) *Store {
	return &Store{
		entries:     make([]*Entry, 0, capacity),
		capacity:    capacity,
		maxBodySize: maxBodySize,
	}
}

func (s *Store) IsEnabled() bool {
	return s.enabled.Load()
}

func (s *Store) SetEnabled(enabled bool) {
	s.enabled.Store(enabled)
}

func (s *Store) MaxBodySize() int {
	return s.maxBodySize
}

func (s *Store) Add(entry *Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.entries) >= s.capacity {
		s.entries = s.entries[1:]
	}
	s.entries = append(s.entries, entry)
}

func (s *Store) List() []*Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]*Entry, len(s.entries))
	copy(result, s.entries)

	// Return newest first
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result
}

func (s *Store) Get(id string) *Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, e := range s.entries {
		if e.ID == id {
			return e
		}
	}
	return nil
}

func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = s.entries[:0]
}

func (s *Store) ListByOrg(orgID int64) []*Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	var filtered []*Entry
	for _, e := range s.entries {
		if e.OrgID == orgID {
			filtered = append(filtered, e)
		}
	}

	result := make([]*Entry, len(filtered))
	copy(result, filtered)

	// Return newest first
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result
}

func (s *Store) ClearByOrg(orgID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := make([]*Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if e.OrgID != orgID {
			filtered = append(filtered, e)
		}
	}
	s.entries = filtered
}

func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
