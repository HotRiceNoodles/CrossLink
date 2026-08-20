package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
)

type patUsageAggFunc func(ctx context.Context, since time.Time) ([]DailyAgg, error)

func (f patUsageAggFunc) DailySummary(ctx context.Context, since time.Time) ([]DailyAgg, error) {
	return f(ctx, since)
}

type patKeyListerFunc func(ctx context.Context, orgID int64) ([]model.APIKey, error)

func (f patKeyListerFunc) List(ctx context.Context, orgID int64) ([]model.APIKey, error) {
	return f(ctx, orgID)
}

type patUsageSummerFunc func(ctx context.Context, keyIDs []int64) (map[int64]UsageAgg, error)

func (f patUsageSummerFunc) TodayByKey(ctx context.Context, keyIDs []int64) (map[int64]UsageAgg, error) {
	return f(ctx, keyIDs)
}

func newPATKeysHandler(lister PATKeyLister, summer UsageSummer) *PATReadHandler {
	return NewPATReadHandler(PATReadDeps{
		KeyLister:   lister,
		UsageSummer: summer,
		UsageAgg: patUsageAggFunc(func(_ context.Context, _ time.Time) ([]DailyAgg, error) {
			return nil, nil
		}),
	})
}

func callUsage(t *testing.T, query string, agg usageAggregator) *httptest.ResponseRecorder {
	t.Helper()
	c, w := newTestContext(t, http.MethodGet, "/usage/summary"+query, nil)
	setAdminContext(c, 1, 1, "admin")
	NewPATReadHandler(PATReadDeps{UsageAgg: agg}).Usage(c)
	return w
}

type patBudgetSpentFunc func(ctx context.Context, scope, targetID, period string) float64

func (f patBudgetSpentFunc) GetCurrentSpent(ctx context.Context, scope, targetID, period string) float64 {
	return f(ctx, scope, targetID, period)
}

type patHealthFunc func() []provider.ProviderHealthSnapshot

func (f patHealthFunc) Snapshot() []provider.ProviderHealthSnapshot { return f() }

func TestPATReadBudgets_Success(t *testing.T) {
	lister := patKeyListerFunc(func(_ context.Context, _ int64) ([]model.APIKey, error) {
		return []model.APIKey{
			{ID: 1, Name: "budgeted", MaxBudget: 10, BudgetPeriod: "monthly"},
			{ID: 2, Name: "no-budget", MaxBudget: 0, BudgetPeriod: "monthly"},
		}, nil
	})
	spent := patBudgetSpentFunc(func(_ context.Context, scope, targetID, period string) float64 {
		if scope != "key" || period != "monthly" || targetID != "1" {
			t.Errorf("GetCurrentSpent(%q, %q, %q)", scope, targetID, period)
		}
		return 9.5
	})

	c, w := newTestContext(t, http.MethodGet, "/budgets/status", nil)
	setAdminContext(c, 1, 1, "admin")
	NewPATReadHandler(PATReadDeps{KeyLister: lister, BudgetSpent: spent}).Budgets(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	decodeResponse(t, w, &resp)
	if len(resp.Data) != 1 {
		t.Fatalf("data length = %d, want 1 (MaxBudget=0 skipped)", len(resp.Data))
	}
	wantFields := map[string]bool{"name": true, "limit": true, "spent": true, "percentage": true, "status": true}
	item := resp.Data[0]
	if len(item) != 5 {
		t.Errorf("item has %d fields (%v), want exactly 5", len(item), item)
	}
	for k := range item {
		if !wantFields[k] {
			t.Errorf("unexpected field %q", k)
		}
	}
	if item["status"] != "warning" || item["percentage"].(float64) != 95 {
		t.Errorf("item = %v, want warning/95", item)
	}
}

func TestPATReadBudgets_StatusThresholds(t *testing.T) {
	lister := patKeyListerFunc(func(_ context.Context, _ int64) ([]model.APIKey, error) {
		return []model.APIKey{
			{ID: 1, Name: "over", MaxBudget: 10, BudgetPeriod: "monthly"},
			{ID: 2, Name: "fine", MaxBudget: 10, BudgetPeriod: "monthly"},
		}, nil
	})
	spent := patBudgetSpentFunc(func(_ context.Context, _, id, _ string) float64 {
		if id == "1" {
			return 10
		}
		return 1
	})

	c, w := newTestContext(t, http.MethodGet, "/budgets/status", nil)
	setAdminContext(c, 1, 1, "admin")
	NewPATReadHandler(PATReadDeps{KeyLister: lister, BudgetSpent: spent}).Budgets(c)

	var resp struct {
		Data []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"data"`
	}
	decodeResponse(t, w, &resp)
	if resp.Data[0].Status != "exceeded" {
		t.Errorf("over-budget status = %q, want exceeded", resp.Data[0].Status)
	}
	if resp.Data[1].Status != "ok" {
		t.Errorf("under-budget status = %q, want ok", resp.Data[1].Status)
	}
}

func TestPATReadBudgets_Empty(t *testing.T) {
	lister := patKeyListerFunc(func(_ context.Context, _ int64) ([]model.APIKey, error) {
		return nil, nil
	})
	spent := patBudgetSpentFunc(func(_ context.Context, _, _, _ string) float64 { return 0 })

	c, w := newTestContext(t, http.MethodGet, "/budgets/status", nil)
	setAdminContext(c, 1, 1, "admin")
	NewPATReadHandler(PATReadDeps{KeyLister: lister, BudgetSpent: spent}).Budgets(c)

	if w.Body.String() == `{"data":null}` {
		t.Errorf("data is null, want []")
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	decodeResponse(t, w, &resp)
	if resp.Data == nil || len(resp.Data) != 0 {
		t.Errorf("data = %v, want empty non-nil array", resp.Data)
	}
}

func TestPATReadBudgets_ListerError(t *testing.T) {
	lister := patKeyListerFunc(func(_ context.Context, _ int64) ([]model.APIKey, error) {
		return nil, errors.New("db down")
	})
	spent := patBudgetSpentFunc(func(_ context.Context, _, _, _ string) float64 { return 0 })

	c, w := newTestContext(t, http.MethodGet, "/budgets/status", nil)
	setAdminContext(c, 1, 1, "admin")
	NewPATReadHandler(PATReadDeps{KeyLister: lister, BudgetSpent: spent}).Budgets(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestPATReadHealth_Success(t *testing.T) {
	health := patHealthFunc(func() []provider.ProviderHealthSnapshot {
		return []provider.ProviderHealthSnapshot{
			{Provider: "openai", Model: "", State: "closed"},
			{Provider: "openai", Model: "gpt-4", State: "open"},
		}
	})

	c, w := newTestContext(t, http.MethodGet, "/health", nil)
	setAdminContext(c, 1, 1, "admin")
	NewPATReadHandler(PATReadDeps{Health: health, Version: "v1.2.3"}).Health(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Version   string           `json:"version"`
			Providers []map[string]any `json:"providers"`
		} `json:"data"`
	}
	decodeResponse(t, w, &resp)
	if resp.Data.Version != "v1.2.3" {
		t.Errorf("version = %q, want v1.2.3", resp.Data.Version)
	}
	if len(resp.Data.Providers) != 2 {
		t.Fatalf("providers length = %d, want 2", len(resp.Data.Providers))
	}
	wantFields := map[string]bool{"provider": true, "model": true, "circuit": true}
	for i, p := range resp.Data.Providers {
		if len(p) != 3 {
			t.Errorf("provider %d has %d fields (%v), want exactly 3", i, len(p), p)
		}
		for k := range p {
			if !wantFields[k] {
				t.Errorf("provider %d has unexpected field %q", i, k)
			}
		}
	}
	if resp.Data.Providers[1]["circuit"] != "open" {
		t.Errorf("circuit = %v, want open", resp.Data.Providers[1]["circuit"])
	}
}

func TestPATReadHealth_Empty(t *testing.T) {
	health := patHealthFunc(func() []provider.ProviderHealthSnapshot { return nil })

	c, w := newTestContext(t, http.MethodGet, "/health", nil)
	setAdminContext(c, 1, 1, "admin")
	NewPATReadHandler(PATReadDeps{Health: health, Version: "dev"}).Health(c)

	if strings.Contains(w.Body.String(), `"providers":null`) {
		t.Errorf("providers is null, want []: %s", w.Body.String())
	}
	var resp struct {
		Data struct {
			Providers []map[string]any `json:"providers"`
		} `json:"data"`
	}
	decodeResponse(t, w, &resp)
	if resp.Data.Providers == nil || len(resp.Data.Providers) != 0 {
		t.Errorf("providers = %v, want empty non-nil array", resp.Data.Providers)
	}
}

func TestPATReadKeys_Success(t *testing.T) {
	lister := patKeyListerFunc(func(_ context.Context, orgID int64) ([]model.APIKey, error) {
		if orgID != 0 {
			t.Errorf("orgID = %d, want 0", orgID)
		}
		return []model.APIKey{
			{ID: 1, Name: "k1", KeyPrefix: "sk-ab", Status: 1, KeyHash: "secret"},
			{ID: 2, Name: "k2", KeyPrefix: "sk-cd", Status: 1, KeyHash: "secret2"},
		}, nil
	})
	summer := patUsageSummerFunc(func(_ context.Context, keyIDs []int64) (map[int64]UsageAgg, error) {
		if len(keyIDs) != 2 {
			t.Errorf("keyIDs = %v, want 2 ids", keyIDs)
		}
		return map[int64]UsageAgg{1: {Requests: 5, Tokens: 100, Cost: 0.25}}, nil
	})

	c, w := newTestContext(t, http.MethodGet, "/keys", nil)
	setAdminContext(c, 1, 1, "admin")
	newPATKeysHandler(lister, summer).Keys(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	decodeResponse(t, w, &resp)
	if len(resp.Data) != 2 {
		t.Fatalf("data length = %d, want 2", len(resp.Data))
	}

	wantFields := map[string]bool{"id": true, "name": true, "status": true, "expires_at": true, "today_requests": true, "today_tokens": true, "today_cost": true}
	for i, item := range resp.Data {
		if len(item) != 7 {
			t.Errorf("item %d has %d fields (%v), want exactly 7", i, len(item), item)
		}
		for k := range item {
			if !wantFields[k] {
				t.Errorf("item %d has unexpected field %q", i, k)
			}
		}
	}
	if resp.Data[1]["today_requests"].(float64) != 0 {
		t.Errorf("key without usage today_requests = %v, want 0", resp.Data[1]["today_requests"])
	}
}

func TestPATReadKeys_NoKeyPrefixLeak(t *testing.T) {
	lister := patKeyListerFunc(func(_ context.Context, _ int64) ([]model.APIKey, error) {
		return []model.APIKey{{ID: 1, Name: "k1", KeyPrefix: "sk-leak", KeyHash: "hash"}}, nil
	})
	summer := patUsageSummerFunc(func(_ context.Context, _ []int64) (map[int64]UsageAgg, error) {
		return map[int64]UsageAgg{}, nil
	})

	c, w := newTestContext(t, http.MethodGet, "/keys", nil)
	setAdminContext(c, 1, 1, "admin")
	newPATKeysHandler(lister, summer).Keys(c)

	var resp map[string]any
	decodeResponse(t, w, &resp)
	body := w.Body.String()
	for _, forbidden := range []string{"sk-leak", "key_prefix", "key_hash"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("response leaks %q: %s", forbidden, body)
		}
	}
}

func TestPATReadKeys_EmptyList(t *testing.T) {
	lister := patKeyListerFunc(func(_ context.Context, _ int64) ([]model.APIKey, error) {
		return nil, nil
	})
	summer := patUsageSummerFunc(func(_ context.Context, keyIDs []int64) (map[int64]UsageAgg, error) {
		if len(keyIDs) != 0 {
			t.Errorf("keyIDs = %v, want empty", keyIDs)
		}
		return map[int64]UsageAgg{}, nil
	})

	c, w := newTestContext(t, http.MethodGet, "/keys", nil)
	setAdminContext(c, 1, 1, "admin")
	newPATKeysHandler(lister, summer).Keys(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if w.Body.String() == `{"data":null}` {
		t.Errorf("data is null, want []")
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	decodeResponse(t, w, &resp)
	if resp.Data == nil || len(resp.Data) != 0 {
		t.Errorf("data = %v, want empty non-nil array", resp.Data)
	}
}

func TestPATReadKeys_ListerError(t *testing.T) {
	lister := patKeyListerFunc(func(_ context.Context, _ int64) ([]model.APIKey, error) {
		return nil, errors.New("db down")
	})
	summer := patUsageSummerFunc(func(_ context.Context, _ []int64) (map[int64]UsageAgg, error) {
		return map[int64]UsageAgg{}, nil
	})

	c, w := newTestContext(t, http.MethodGet, "/keys", nil)
	setAdminContext(c, 1, 1, "admin")
	newPATKeysHandler(lister, summer).Keys(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestPATReadUsage_Success(t *testing.T) {
	agg := patUsageAggFunc(func(_ context.Context, since time.Time) ([]DailyAgg, error) {
		if time.Since(since) < 6*24*time.Hour || time.Since(since) > 8*24*time.Hour {
			t.Errorf("since = %v, want ~now-7d", since)
		}
		return []DailyAgg{
			{Date: "2026-08-19", Requests: 100, Tokens: 5000, Cost: 1.25},
			{Date: "2026-08-20", Requests: 40, Tokens: 2000, Cost: 0.5},
		}, nil
	})

	w := callUsage(t, "", agg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data struct {
			Days  []map[string]any `json:"days"`
			Total map[string]any   `json:"total"`
		} `json:"data"`
	}
	decodeResponse(t, w, &resp)
	if len(resp.Data.Days) != 2 {
		t.Fatalf("days length = %d, want 2", len(resp.Data.Days))
	}
	wantFields := map[string]bool{"date": true, "requests": true, "tokens": true, "cost": true}
	for i, d := range resp.Data.Days {
		if len(d) != 4 {
			t.Errorf("day %d has %d fields (%v), want exactly 4", i, len(d), d)
		}
		for k := range d {
			if !wantFields[k] {
				t.Errorf("day %d has unexpected field %q", i, k)
			}
		}
	}
	if resp.Data.Days[0]["date"] != "2026-08-19" || resp.Data.Days[0]["requests"].(float64) != 100 {
		t.Errorf("day 0 = %v", resp.Data.Days[0])
	}
	if resp.Data.Total["requests"].(float64) != 140 || resp.Data.Total["tokens"].(float64) != 7000 || resp.Data.Total["cost"].(float64) != 1.75 {
		t.Errorf("total = %v, want accumulated values", resp.Data.Total)
	}
}

func TestPATReadUsage_DaysParamFallback(t *testing.T) {
	for _, q := range []string{"?days=-1", "?days=abc", "?days=200"} {
		var gotSince time.Time
		agg := patUsageAggFunc(func(_ context.Context, since time.Time) ([]DailyAgg, error) {
			gotSince = since
			return nil, nil
		})
		w := callUsage(t, q, agg)
		if w.Code != http.StatusOK {
			t.Fatalf("query %q: status = %d", q, w.Code)
		}
		age := time.Since(gotSince)
		if age < 6*24*time.Hour || age > 8*24*time.Hour {
			t.Errorf("query %q: since age = %v, want ~7d", q, age)
		}
	}
}

func TestPATReadUsage_AggregatorError(t *testing.T) {
	agg := patUsageAggFunc(func(_ context.Context, _ time.Time) ([]DailyAgg, error) {
		return nil, errors.New("db down")
	})
	w := callUsage(t, "", agg)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestPATReadUsage_EmptyDays(t *testing.T) {
	agg := patUsageAggFunc(func(_ context.Context, _ time.Time) ([]DailyAgg, error) {
		return nil, nil
	})
	w := callUsage(t, "", agg)
	if w.Body.String() == `{"data":{"days":null,` || strings.Contains(w.Body.String(), `"days":null`) {
		t.Errorf("days is null, want []: %s", w.Body.String())
	}
	var resp struct {
		Data struct {
			Days  []map[string]any `json:"days"`
			Total map[string]any   `json:"total"`
		} `json:"data"`
	}
	decodeResponse(t, w, &resp)
	if resp.Data.Days == nil || len(resp.Data.Days) != 0 {
		t.Errorf("days = %v, want empty non-nil array", resp.Data.Days)
	}
	if resp.Data.Total["requests"].(float64) != 0 || resp.Data.Total["tokens"].(float64) != 0 || resp.Data.Total["cost"].(float64) != 0 {
		t.Errorf("total = %v, want all zeros", resp.Data.Total)
	}
}
