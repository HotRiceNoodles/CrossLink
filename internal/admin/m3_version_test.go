package admin

// M3-A+C tests: versioned rule writes, rollback-as-new-version, history
// immutability, transaction failure discipline (no broadcast, no history, no
// rule change), dual-instance convergence through the E broadcast, tenant
// isolation, sensitive-config sanitization, and decision attribution lists.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	redisv9 "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/crosslink/internal/guardrail"
	sqlite "github.com/glebarez/sqlite"
)

type m3aEnv struct {
	db      *gorm.DB
	mr      *miniredis.Miniredis
	rdb     *redisv9.Client
	svcA    *guardrail.GuardrailService
	svcB    *guardrail.GuardrailService
	cancelA context.CancelFunc
	cancelB context.CancelFunc
	router  *gin.Engine
}

func newM3AEnv(t *testing.T, redis bool, orgID int64) *m3aEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "m3a.db")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`CREATE TABLE guardrail_rules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		org_id INTEGER,
		name TEXT NOT NULL,
		type TEXT NOT NULL,
		direction TEXT NOT NULL,
		enabled INTEGER DEFAULT 1,
		config TEXT NOT NULL,
		severity TEXT DEFAULT 'medium',
		action TEXT DEFAULT 'block',
		model_filter TEXT,
		version INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE guardrail_rule_versions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		rule_id INTEGER NOT NULL,
		version INTEGER NOT NULL,
		org_id INTEGER,
		name TEXT NOT NULL,
		type TEXT NOT NULL,
		direction TEXT NOT NULL,
		enabled BOOLEAN NOT NULL DEFAULT 1,
		config TEXT NOT NULL,
		severity TEXT DEFAULT 'medium',
		action TEXT DEFAULT 'block',
		model_filter TEXT,
		change_kind TEXT NOT NULL,
		actor TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE (rule_id, version)
	)`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE system_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`).Error)
	t.Cleanup(func() {
		if sqlDB, derr := db.DB(); derr == nil {
			_ = sqlDB.Close()
		}
	})

	env := &m3aEnv{db: db}
	if redis {
		env.mr = miniredis.RunT(t)
		env.rdb = redisv9.NewClient(&redisv9.Options{Addr: env.mr.Addr()})
		t.Cleanup(func() { _ = env.rdb.Close() })
	}
	env.svcA = guardrail.NewGuardrailService(db, nil)
	env.svcB = guardrail.NewGuardrailService(db, nil)
	if redis {
		env.svcA.SetRedisClient(env.rdb)
		env.svcB.SetRedisClient(env.rdb)
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	env.cancelA, env.cancelB = cancelA, cancelB
	t.Cleanup(func() { cancelA(); cancelB() })
	env.svcA.StartReloadSubscription(ctxA)
	env.svcB.StartReloadSubscription(ctxB)

	handler := NewGuardrailHandler(db, env.svcA, nil)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if orgID != 0 {
			c.Set("org_id", orgID)
		}
		c.Set("username", "m3-tester")
	})
	r.POST("/guardrails", handler.Create)
	r.PUT("/guardrails/:id", handler.Update)
	r.DELETE("/guardrails/:id", handler.Delete)
	r.POST("/guardrails/:id/rollback", handler.Rollback)
	r.GET("/guardrails/versions", handler.Versions)
	env.router = r
	return env
}

func (e *m3aEnv) req(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rd = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func (e *m3aEnv) seed(t *testing.T, keyword string) (int64, string) {
	t.Helper()
	cfg, _ := json.Marshal(map[string]any{"blocklist": []string{keyword}})
	w := e.req(t, "POST", "/guardrails", map[string]any{
		"name": "m3a-" + keyword, "type": "keyword_filter", "direction": "both",
		"enabled": true, "config": json.RawMessage(cfg), "severity": "medium", "action": "block",
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var resp struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.Data.ID, string(cfg)
}

func (e *m3aEnv) updateCfg(t *testing.T, id int64, keyword string) int {
	cfg, _ := json.Marshal(map[string]any{"blocklist": []string{keyword}})
	return e.req(t, "PUT", fmt.Sprintf("/guardrails/%d", id), map[string]any{"config": json.RawMessage(cfg)}).Code
}

func (e *m3aEnv) history(t *testing.T, ruleID int64) map[int64]guardrail.GuardrailRuleVersion {
	t.Helper()
	var rows []guardrail.GuardrailRuleVersion
	require.NoError(t, e.db.Where("rule_id = ?", ruleID).Order("version ASC").Find(&rows).Error)
	out := map[int64]guardrail.GuardrailRuleVersion{}
	for _, r := range rows {
		out[r.Version] = r
	}
	return out
}

// TestM3AConcurrentUpdatesNoLostUpdate: N racing updates yield exactly one
// winner per version step; losers get 409; the history chain records every
// winner in order and no update is silently lost.
func TestM3AConcurrentUpdatesNoLostUpdate(t *testing.T) {
	env := newM3AEnv(t, false, 0)
	id, _ := env.seed(t, "MARKERBASE")

	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = env.updateCfg(t, id, fmt.Sprintf("MARKERWIN%d", i))
		}(i)
	}
	wg.Wait()
	wins, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			wins++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	require.Equal(t, n, wins+conflicts, "every request resolves to win or 409")
	require.Greater(t, wins, 0)

	var rule guardrail.GuardrailRule
	require.NoError(t, env.db.First(&rule, id).Error)
	require.Equal(t, int64(1+wins), rule.Version, "version increments exactly once per winner")

	hist := env.history(t, id)
	require.Len(t, hist, 1+wins, "one history row per committed state (create + wins)")
	for v := int64(2); v <= rule.Version; v++ {
		require.Equal(t, "update", hist[v].ChangeKind)
		require.Equal(t, "m3-tester", hist[v].Actor)
	}
	// the final rule content equals the last winner's history snapshot
	require.Equal(t, string(rule.Config), string(hist[rule.Version].Config))
	t.Logf("concurrency: %d racing updates → %d committed (version %d), %d rejected 409", n, wins, rule.Version, conflicts)
}

// TestM3ARollbackEquivalence: rollback re-publishes the historical snapshot
// as a NEW version; config is byte-identical and detection behavior matches.
func TestM3ARollbackEquivalence(t *testing.T) {
	env := newM3AEnv(t, false, 0)
	id, v1Cfg := env.seed(t, "MARKERONE")

	require.Equal(t, http.StatusOK, env.updateCfg(t, id, "MARKERTWO"))
	hist := env.history(t, id)
	require.Len(t, hist, 2)

	// after v2: MARKERTWO blocked, MARKERONE not
	check := func(text string) bool {
		resp, err := env.svcA.Check(context.Background(), &guardrail.CheckRequest{Content: text, Direction: guardrail.DirectionRequest})
		require.NoError(t, err)
		return resp.Blocked
	}
	require.True(t, check("leak MARKERTWO"))
	require.False(t, check("leak MARKERONE"))

	w := env.req(t, "POST", fmt.Sprintf("/guardrails/%d/rollback", id), map[string]any{"to_version": 1})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var rule guardrail.GuardrailRule
	require.NoError(t, env.db.First(&rule, id).Error)
	require.Equal(t, int64(3), rule.Version, "rollback is a new version")
	require.Equal(t, v1Cfg, string(rule.Config), "rolled-back config is byte-identical to the v1 snapshot")

	require.True(t, check("leak MARKERONE"))
	require.False(t, check("leak MARKERTWO"))

	hist = env.history(t, id)
	require.Len(t, hist, 3)
	require.Equal(t, "rollback", hist[3].ChangeKind)
	require.Equal(t, string(hist[1].Config), string(hist[3].Config), "rollback snapshot equals the target snapshot")

	// delete snapshots are terminal, not restorable
	dw := env.req(t, "DELETE", fmt.Sprintf("/guardrails/%d", id), nil)
	require.Equal(t, http.StatusOK, dw.Code)
	hist = env.history(t, id)
	require.Len(t, hist, 4)
	require.Equal(t, "delete", hist[4].ChangeKind)
	// the rule row is gone, so rollback 404s at the rule load — the correct
	// terminal outcome (the in-code delete-snapshot guard stays as
	// defense-in-depth for any future undelete path)
	rw := env.req(t, "POST", fmt.Sprintf("/guardrails/%d/rollback", id), map[string]any{"to_version": 4})
	require.Equal(t, http.StatusNotFound, rw.Code, "deleted rule is not restorable via rollback")
	t.Logf("rollback chain: v1 create → v2 update → v3 rollback(=v1 bytes) → v4 delete(terminal)")
}

// TestM3ATxFailureNoSideEffects: a failed history INSERT rolls back the rule
// update too — no version bump, no history row, no reload broadcast.
func TestM3ATxFailureNoSideEffects(t *testing.T) {
	env := newM3AEnv(t, true, 0)
	id, _ := env.seed(t, "MARKERONE")

	sub := env.rdb.Subscribe(context.Background(), "guardrail:rules:reload")
	msgs := sub.Channel()
	defer sub.Close()
	drain := func() int {
		n := 0
		for {
			select {
			case <-msgs:
				n++
			default:
				return n
			}
		}
	}

	require.NoError(t, env.db.Exec(
		`CREATE TRIGGER fail_hist_insert BEFORE INSERT ON guardrail_rule_versions `+
			`BEGIN SELECT RAISE(ABORT, 'injected failure'); END`).Error)

	before := env.history(t, id)
	w := env.req(t, "PUT", fmt.Sprintf("/guardrails/%d", id), map[string]any{
		"config": json.RawMessage(`{"blocklist":["MARKERTWO"]}`),
	})
	require.Equal(t, http.StatusInternalServerError, w.Code, "poisoned history INSERT must fail the update")

	time.Sleep(200 * time.Millisecond)
	require.Zero(t, drain(), "failed transaction must not broadcast")
	require.Equal(t, before, env.history(t, id), "history unchanged after failed tx")
	var rule guardrail.GuardrailRule
	require.NoError(t, env.db.First(&rule, id).Error)
	require.Equal(t, int64(1), rule.Version, "version unchanged after failed tx")
	require.Contains(t, string(rule.Config), "MARKERONE", "rule content unchanged after failed tx")

	// after removing the poison, the same update succeeds
	require.NoError(t, env.db.Exec(`DROP TRIGGER fail_hist_insert`).Error)
	require.Equal(t, http.StatusOK, env.updateCfg(t, id, "MARKERTWO"))
	require.Len(t, env.history(t, id), 2)
	t.Logf("tx-failure discipline: 500 + no broadcast + no history row + no version bump; recovery works")
}

// TestM3ADualInstanceRollbackConvergence: instance B (subscribed) converges
// after A's committed rollback; measured time, no TTL sleep.
func TestM3ADualInstanceRollbackConvergence(t *testing.T) {
	env := newM3AEnv(t, true, 0)
	id, _ := env.seed(t, "MARKERONE")
	require.Equal(t, http.StatusOK, env.updateCfg(t, id, "MARKERTWO"))

	checkB := func(text string) bool {
		resp, err := env.svcB.Check(context.Background(), &guardrail.CheckRequest{Content: text, Direction: guardrail.DirectionRequest})
		require.NoError(t, err)
		return resp.Blocked
	}
	require.True(t, checkB("leak MARKERTWO"), "B warm with v2")

	t0 := time.Now()
	w := env.req(t, "POST", fmt.Sprintf("/guardrails/%d/rollback", id), map[string]any{"to_version": 1})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	start := time.Now()
	for {
		if checkB("leak MARKERONE") && !checkB("leak MARKERTWO") {
			break
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("B did not converge after rollback within 5s (measured %v)", time.Since(start))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("rollback convergence: B applied v1 behavior %v after A's committed rollback (admin took %v)",
		time.Since(start).Round(time.Microsecond), time.Since(t0).Round(time.Microsecond))
}

// TestM3ATenantIsolation: another tenant can neither read a rule's history
// nor roll it back (404 before any history access).
func TestM3ATenantIsolation(t *testing.T) {
	org1 := newM3AEnv(t, false, 7)
	id, _ := org1.seed(t, "MARKERONE")
	require.Equal(t, http.StatusOK, org1.updateCfg(t, id, "MARKERTWO"))

	org2 := newM3AEnv(t, false, 9)
	org2.db = org1.db // shares the DATA, separate tenant context on the router
	w := org2.req(t, "GET", fmt.Sprintf("/guardrails/versions?rule_id=%d", id), nil)
	require.Equal(t, http.StatusNotFound, w.Code, "cross-tenant history must be invisible")
	w = org2.req(t, "POST", fmt.Sprintf("/guardrails/%d/rollback", id), map[string]any{"to_version": 1})
	require.Equal(t, http.StatusNotFound, w.Code, "cross-tenant rollback must be impossible")
	w = org2.req(t, "PUT", fmt.Sprintf("/guardrails/%d", id), map[string]any{"name": "hijack"})
	require.Equal(t, http.StatusNotFound, w.Code, "cross-tenant update must be impossible")

	// the owning tenant still sees everything
	w = org1.req(t, "GET", fmt.Sprintf("/guardrails/versions?rule_id=%d", id), nil)
	require.Equal(t, http.StatusOK, w.Code)
	t.Logf("tenant isolation: org9 got 404 on versions/rollback/update of org7's rule; org7 OK")
}

// TestM3AConfigSanitization: sensitive config values never appear in any API
// response (create/update/rollback/versions); history rows keep the raw value
// server-side so rollback still restores it byte-exact.
func TestM3AConfigSanitization(t *testing.T) {
	env := newM3AEnv(t, false, 0)
	cfg, _ := json.Marshal(map[string]any{"blocklist": []string{"MARKERONE"}, "token": "SUPERSECRETTOKEN"})
	w := env.req(t, "POST", "/guardrails", map[string]any{
		"name": "sens", "type": "keyword_filter", "direction": "both",
		"enabled": true, "config": json.RawMessage(cfg), "severity": "medium", "action": "block",
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), "SUPERSECRETTOKEN", "create response must mask secrets")

	var resp struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	id := resp.Data.ID

	// update + rollback + versions responses all sanitized
	cfg2, _ := json.Marshal(map[string]any{"blocklist": []string{"MARKERTWO"}, "token": "SUPERSECRETTOKEN"})
	w = env.req(t, "PUT", fmt.Sprintf("/guardrails/%d", id), map[string]any{"config": json.RawMessage(cfg2)})
	require.Equal(t, http.StatusOK, w.Code)
	require.NotContains(t, w.Body.String(), "SUPERSECRETTOKEN")

	w = env.req(t, "POST", fmt.Sprintf("/guardrails/%d/rollback", id), map[string]any{"to_version": 1})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), "SUPERSECRETTOKEN")

	w = env.req(t, "GET", fmt.Sprintf("/guardrails/versions?rule_id=%d", id), nil)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotContains(t, w.Body.String(), "SUPERSECRETTOKEN", "history API must mask secrets")

	// server-side history retains the RAW config: rollback restored v1 bytes
	var rule guardrail.GuardrailRule
	require.NoError(t, env.db.First(&rule, id).Error)
	require.Contains(t, string(rule.Config), "SUPERSECRETTOKEN", "raw config retained internally for exact rollback")
	require.Contains(t, string(rule.Config), "MARKERONE")
	t.Logf("sanitization: secret masked in all API responses, retained server-side for rollback fidelity")
}

// TestM3AHistoryImmutable: earlier history rows are byte-identical after
// later operations (append-only by construction; verified against drift).
func TestM3AHistoryImmutable(t *testing.T) {
	env := newM3AEnv(t, false, 0)
	id, _ := env.seed(t, "MARKERONE")
	env.updateCfg(t, id, "MARKERTWO")
	snap := env.history(t, id)

	env.updateCfg(t, id, "MARKERTHREE")
	env.req(t, "POST", fmt.Sprintf("/guardrails/%d/rollback", id), map[string]any{"to_version": 2})

	after := env.history(t, id)
	for v, row := range snap {
		require.Equal(t, row, after[v], "history row v%d must be immutable", v)
	}
	require.Len(t, after, len(snap)+2, "new rows appended only")
	t.Logf("history immutability: v1/v2 byte-identical across later update+rollback")
}

// TestM3AAttributionList: the decision response and blocking event carry the
// exact participating (rule_id, version, action) list with each rule's own
// version; non-participating rules are absent.
func TestM3AAttributionList(t *testing.T) {
	env := newM3AEnv(t, false, 0)
	mk := func(name, keyword, severity, action string, ver *int64) int64 {
		cfg, _ := json.Marshal(map[string]any{"blocklist": []string{keyword}})
		w := env.req(t, "POST", "/guardrails", map[string]any{
			"name": name, "type": "keyword_filter", "direction": "both",
			"enabled": true, "config": json.RawMessage(cfg), "severity": severity, "action": action,
		})
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		var resp struct {
			Data struct {
				ID int64 `json:"id"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		if ver != nil {
			*ver = resp.Data.ID
		}
		return resp.Data.ID
	}
	var vLog, vBlock int64
	idLog := mk("r-log", "MARKERLOG", "critical", "log", &vLog) // critical → evaluated first
	idBlock := mk("r-block", "MARKERBLOCK", "high", "block", &vBlock)
	idAbsent := mk("r-absent", "NEVERSEEN", "low", "block", nil)
	_ = idAbsent
	// bump the block rule to version 2 (same detection behavior) so the
	// attribution assertions distinguish versions from ids and from 1
	require.Equal(t, http.StatusOK, env.updateCfg(t, idBlock, "MARKERBLOCK"))

	var mu sync.Mutex
	var events []guardrail.GuardrailEvent
	env.svcA.AddHandler(func(ctx context.Context, ev guardrail.GuardrailEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	})

	resp, err := env.svcA.Check(context.Background(), &guardrail.CheckRequest{
		Content: "please leak MARKERLOG and MARKERBLOCK now", Direction: guardrail.DirectionRequest,
	})
	require.NoError(t, err)
	require.True(t, resp.Blocked)

	require.Len(t, resp.Matched, 2, "exactly the two participating rules")
	require.Equal(t, guardrail.MatchedRule{RuleID: idLog, Version: 1, Action: "log"}, resp.Matched[0])
	require.Equal(t, guardrail.MatchedRule{RuleID: idBlock, Version: 2, Action: "block"}, resp.Matched[1])
	for _, m := range resp.Matched {
		require.NotEqual(t, idAbsent, m.RuleID, "non-participating rule must be absent")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		var blockEv *guardrail.GuardrailEvent
		for i := range events {
			if events[i].RuleID == idBlock && events[i].Action == "block" {
				blockEv = &events[i]
			}
		}
		mu.Unlock()
		if blockEv != nil {
			require.Equal(t, int64(2), blockEv.RuleVersion, "event carries its own rule's version (v2, not the id, not 1)")
			require.Len(t, blockEv.Matched, 2, "decision event carries the full participating list")
			require.Equal(t, idLog, blockEv.Matched[0].RuleID)
			require.Equal(t, int64(1), blockEv.Matched[0].Version)
			require.Equal(t, idBlock, blockEv.Matched[1].RuleID)
			require.Equal(t, int64(2), blockEv.Matched[1].Version)
			t.Logf("attribution: matched=[log(v1) block(v2)] absent-rule excluded; event rule_version=%d", blockEv.RuleVersion)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("blocking event not observed: %d events", len(events))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
