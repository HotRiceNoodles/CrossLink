package handler

// r63 probe defect regression: a semantic-cache HIT must produce exactly one
// durable ledger row (cache_hit=true) per served request. The 40-probe run
// showed 40 identical 200s where only the populating MISS was billed — the
// HIT path aborted the chain before any usage submit. The biller here is the
// HIT-side half of the fix; the chain-order half lives in
// middleware/cache_billing_chain_test.go.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
)

func TestCacheHitBiller_OneDurableRowPerHit(t *testing.T) {
	db := newUsageDB(t)
	bill := NewCacheHitBiller(db.svc)

	cached := &service.CacheEntry{
		Model:       "mock-r63",
		InputTokens: 12,
		OutputTokens: 6,
		InputPrice:  3,
		OutputPrice: 12,
		Currency:    "CNY",
	}

	const hits = 3
	for i := 0; i < hits; i++ {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request, _ = http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		c.Set("request_id", fmt.Sprintf("00000000-0000-0000-0000-%012d", i))
		c.Set("api_key", &model.APIKey{ID: 990655, TeamID: ptrTeam(990654)})
		c.Set("org_id", int64(990653))
		c.Set("cache_hit_latency_ms", int64(2))
		bill(c, cached)
	}

	// The submit path is asynchronous (worker goroutine when no spool/pool
	// are armed in tests); poll the DB with a deadline.
	deadline := time.Now().Add(5 * time.Second)
	var rows int64
	for time.Now().Before(deadline) {
		db.gorm.Raw("SELECT count(*) FROM usage_logs").Scan(&rows)
		if rows == hits {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rows != hits {
		t.Fatalf("usage rows = %d, want %d (one durable row per HIT)", rows, hits)
	}

	type row struct {
		EventID      string
		RequestID    string
		CacheHit     bool
		APIKeyID     int64
		TeamID       int64
		OrgID        int64
		InputTokens  int
		OutputTokens int
		ModelUsed    string
		Currency     string
	}
	var got []row
	if err := db.gorm.Raw("SELECT event_id, request_id, cache_hit, api_key_id, team_id, org_id, input_tokens, output_tokens, model_used, currency FROM usage_logs").Scan(&got).Error; err != nil {
		t.Fatalf("scan: %v", err)
	}
	seenEvent := map[string]bool{}
	for _, r := range got {
		if r.EventID == "" || seenEvent[r.EventID] {
			t.Fatalf("event_id %q empty or duplicated — each HIT must be its own billing event", r.EventID)
		}
		seenEvent[r.EventID] = true
		if r.RequestID == "" {
			t.Fatal("request_id empty — the HIT row must carry the internal billing id")
		}
		if !r.CacheHit {
			t.Fatalf("row %q: cache_hit=false, want true", r.EventID)
		}
		if r.APIKeyID != 990655 || r.TeamID != 990654 || r.OrgID != 990653 {
			t.Fatalf("row %q identity = %d/%d/%d", r.EventID, r.APIKeyID, r.TeamID, r.OrgID)
		}
		if r.InputTokens != 12 || r.OutputTokens != 6 || r.ModelUsed != "mock-r63" || r.Currency != "CNY" {
			t.Fatalf("row %q usage = %d/%d model=%q currency=%q", r.EventID, r.InputTokens, r.OutputTokens, r.ModelUsed, r.Currency)
		}
	}
}

// The biller signature must stay compatible with the middleware wiring.
func TestCacheHitBiller_Interface(t *testing.T) {
	var _ middleware.CacheHitBiller = NewCacheHitBiller(nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	NewCacheHitBiller(nil)(c, nil) // nil entry: no-op, no panic
}

func ptrTeam(v int64) *int64 { return &v }
