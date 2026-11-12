package debug

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestStore_GetBySeqWithDuplicateRequestIDs reproduces the bug where multiple
// distinct requests share the same client X-Request-ID (Entry.ID). Entries must
// still be individually retrievable by their store-assigned Seq — Get must NOT
// collapse them onto one.
func TestStore_GetBySeqWithDuplicateRequestIDs(t *testing.T) {
	s := NewStore(100, 1024)

	const sharedReqID = "5ebe4c6c9049e95b"
	bodies := []string{"req-A", "req-B", "req-C", "req-D"}
	for _, b := range bodies {
		s.Add(&Entry{ID: sharedReqID, ReqBody: []byte(b), RespBody: []byte("resp-" + b)})
	}

	listed := s.List()
	assert.Len(t, listed, len(bodies), "all entries stored")

	// Each entry got a unique Seq despite identical request_id.
	seen := map[int64]bool{}
	for _, e := range listed {
		assert.False(t, seen[e.Seq], "Seq must be unique across entries")
		seen[e.Seq] = true
		assert.Equal(t, sharedReqID, e.ID, "request_id is display-only and may repeat")
	}

	// Every Seq retrieves the correct, distinct body — no collapse.
	for _, e := range listed {
		got := s.Get(e.Seq)
		if assert.NotNil(t, got) {
			assert.Equal(t, e.ReqBody, got.ReqBody, "Get(Seq) must return this entry's own body")
			assert.Equal(t, e.RespBody, got.RespBody)
		}
	}

	// Unknown Seq returns nil.
	assert.Nil(t, s.Get(99999))
}

/* 采样策略钳制（0-100 外的收入边界）+ 只记失败开关（2026-11-12 调试数据安全） */
func TestSamplingPolicyClamp(t *testing.T) {
	s := NewStore(10, 1024)
	s.SetSamplingRate(-5)
	if s.SamplingRate() != 0 {
		t.Fatalf("negative rate must clamp to 0, got %d", s.SamplingRate())
	}
	s.SetSamplingRate(150)
	if s.SamplingRate() != 100 {
		t.Fatalf("oversized rate must clamp to 100, got %d", s.SamplingRate())
	}
	s.SetOnlyFailed(true)
	if !s.OnlyFailed() {
		t.Fatal("only-failed flag must round-trip")
	}
}
