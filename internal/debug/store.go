package debug

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crosslink/internal/debug/upstream"
)

type Entry struct {
	// Seq is a store-assigned, monotonically unique identity for each entry.
	// It is NOT the client request_id (Entry.ID) — request_id may legitimately
	// repeat across requests when a client reuses X-Request-ID, so it must not
	// be used as a lookup key. Seq is the only safe identity for retrieval.
	Seq         int64
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
	RespStatus  int
	RespHeaders http.Header
	RespBody    []byte
	InputTokens  int
	OutputTokens int

	UpstreamCalls []upstream.UpstreamCall // upstream HTTP call chain
}

type Store struct {
	mu          sync.Mutex
	entries     []*Entry
	capacity    int
	enabled     atomic.Bool
	maxBodySize int
	nextSeq     atomic.Int64
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

	entry.Seq = s.nextSeq.Add(1)

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

// Get returns the entry with the given store-assigned Seq. Seq is unique per
// entry, so unlike the client-controlled request_id this cannot collapse
// multiple distinct requests onto one.
func (s *Store) Get(seq int64) *Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, e := range s.entries {
		if e.Seq == seq {
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
