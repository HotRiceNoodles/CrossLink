package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/crosslink/internal/model"
)

func newBehaviorTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "behavior_admin.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.BehaviorAnomaly{}, &model.BehaviorTolerance{}, &model.BehaviorFingerprint{}, &model.BehaviorBaseline{},
		&model.BehaviorRollup{}, &model.BehaviorJobLock{}, &model.BehaviorSeenCategory{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func TestBehaviorAnomalyLedgerEndpoints(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	db := newBehaviorTestDB(t)
	now := time.Now().UTC()
	// dimensions 快照：request_rate 命中（s'=100 ≥40）→ confirm 后应写容忍度 0.9。
	dims := `[{"metric":"request_rate","class":"A","s":100,"s_prime":100,"obs":28,"p50":10,"p90":10,"x_now":100}]`
	ev := &model.BehaviorAnomaly{
		EventFingerprint: "fp-admin-test", MergeBucket: now.Unix() / 14400,
		EntityType: "key", EntityID: 7, Score: 100, ScorePeak: 100, Band: "critical",
		Dimensions: &dims, BaselineModelVer: 1,
		FirstSeenAt: now, LastHitAt: now, HitCount: 1,
		WindowStart: now, WindowEnd: now.Add(time.Hour), Status: "open",
	}
	if err := db.Create(ev).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	h := NewBehaviorHandler(db, nil, func() bool { return true })
	r := gin.New()
	r.GET("/behavior/anomalies", h.ListAnomalies)
	r.GET("/behavior/anomalies/:id", h.GetAnomaly)
	r.POST("/behavior/anomalies/:id/confirm", h.Confirm)
	r.POST("/behavior/anomalies/:id/dismiss", h.Dismiss)
	r.GET("/behavior/fingerprints", h.ListFingerprints)
	r.POST("/behavior/fingerprints/:id/approve", h.ApproveFingerprint)
	r.POST("/behavior/fingerprints/:id/retire", h.RetireFingerprint)
	r.GET("/behavior/status", h.Status)

	// 状态诊断的前置：新鲜基线快照（否则 learning_stale 判真）。
	db.Create(&model.BehaviorBaseline{
		EntityType: "key", EntityID: 7, Metric: "request_rate", BucketKey: "9",
		P50: 10, P90: 20, ObsCount: 28, WindowStart: now, WindowEnd: now,
		ModelVersion: 1, ComputedAt: now,
	})

	do := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		r.ServeHTTP(w, req)
		return w
	}

	// 状态诊断：enabled=true + 新鲜快照 → learning_stale=false；计数在位。
	wst := do("GET", "/behavior/status")
	if wst.Code != http.StatusOK {
		t.Fatalf("status: %d %s", wst.Code, wst.Body.String())
	}
	var st struct {
		Data struct {
			Enabled       bool             `json:"enabled"`
			LearningStale bool             `json:"learning_stale"`
			Counts        map[string]int64 `json:"counts"`
			Jobs          []map[string]any `json:"jobs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(wst.Body.Bytes(), &st); err != nil {
		t.Fatalf("status body: %v", err)
	}
	if !st.Data.Enabled || st.Data.LearningStale {
		t.Fatalf("status: enabled=%v stale=%v (seeded baseline exists → not stale)", st.Data.Enabled, st.Data.LearningStale)
	}
	if st.Data.Counts["anomalies_total"] < 1 {
		t.Fatalf("status counts: %+v", st.Data.Counts)
	}

	// 列表筛选 + 分页信封。
	w := do("GET", "/behavior/anomalies?status=open&band=critical")
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var listResp struct {
		Data  []model.BehaviorAnomaly `json:"data"`
		Total int64                   `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("list body: %v", err)
	}
	if listResp.Total != 1 || len(listResp.Data) != 1 || listResp.Data[0].EntityID != 7 {
		t.Fatalf("list payload: %+v", listResp)
	}
	// 非法 status → 400。
	if w := do("GET", "/behavior/anomalies?status=bogus"); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid status: %d", w.Code)
	}
	// 详情。
	if w := do("GET", "/behavior/anomalies/1"); w.Code != http.StatusOK {
		t.Fatalf("get: %d", w.Code)
	}
	if w := do("GET", "/behavior/anomalies/999"); w.Code != http.StatusNotFound {
		t.Fatalf("get missing: %d", w.Code)
	}

	// confirm → 终态 + 容忍度反馈（confirmed: t=1.0×0.9=0.9，作用于命中维度）
	// + 行为指纹候选（M3 反馈闭环：confirmed 才产出，dismiss 不产出）。
	if w := do("POST", "/behavior/anomalies/1/confirm"); w.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", w.Code, w.Body.String())
	}
	var fp model.BehaviorFingerprint
	if err := db.Where("entity_type = ? AND entity_id = ?", "key", 7).First(&fp).Error; err != nil {
		t.Fatalf("fingerprint candidate missing after confirm: %v", err)
	}
	if fp.Status != "candidate" || fp.DimensionSet != "request_rate" || fp.SourceEventID == nil || *fp.SourceEventID != ev.ID {
		t.Fatalf("candidate: %+v", fp)
	}
	if w := do("GET", "/behavior/fingerprints?status=candidate"); w.Code != http.StatusOK {
		t.Fatalf("list fingerprints: %d", w.Code)
	}
	var tol model.BehaviorTolerance
	if err := db.Where("entity_type = ? AND entity_id = ? AND metric = ?", "key", 7, "request_rate").First(&tol).Error; err != nil {
		t.Fatalf("tolerance row missing after confirm: %v", err)
	}
	if tol.Multiplier < 0.89 || tol.Multiplier > 0.91 {
		t.Fatalf("confirmed tolerance = %v, want ~0.9", tol.Multiplier)
	}
	// 终态改判 → 409（D16：confirmed/dismissed 不可重开/改判）。
	if w := do("POST", "/behavior/anomalies/1/dismiss"); w.Code != http.StatusConflict {
		t.Fatalf("re-decide terminal: %d", w.Code)
	}
	if w := do("POST", "/behavior/anomalies/1/confirm"); w.Code != http.StatusConflict {
		t.Fatalf("re-confirm terminal: %d", w.Code)
	}

	// 第二案 dismiss → 容忍度从 0.9 出发 ×1.1 = 0.99（重复反馈累计，棘轮内）。
	dims2 := dims
	ev2 := &model.BehaviorAnomaly{
		EventFingerprint: "fp-admin-test-2", MergeBucket: now.Unix()/14400 + 1,
		EntityType: "key", EntityID: 7, Score: 61, ScorePeak: 61, Band: "suspicious",
		Dimensions: &dims2, BaselineModelVer: 1,
		FirstSeenAt: now.Add(time.Hour), LastHitAt: now.Add(time.Hour), HitCount: 1,
		WindowStart: now.Add(time.Hour), WindowEnd: now.Add(2 * time.Hour), Status: "open",
	}
	if err := db.Create(ev2).Error; err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	if w := do("POST", "/behavior/anomalies/2/dismiss"); w.Code != http.StatusOK {
		t.Fatalf("dismiss: %d %s", w.Code, w.Body.String())
	}
	db.Where("entity_type = ? AND entity_id = ? AND metric = ?", "key", 7, "request_rate").First(&tol)
	if tol.Multiplier < 0.98 || tol.Multiplier > 1.00 {
		t.Fatalf("dismissed tolerance = %v, want ~0.99 (accumulates from 0.9)", tol.Multiplier)
	}
	// dismiss 不产出指纹候选（仍只有 confirm 那一条）。
	var fpCount int64
	db.Model(&model.BehaviorFingerprint{}).Count(&fpCount)
	if fpCount != 1 {
		t.Fatalf("fingerprints = %d, want 1 (dismiss must not create candidates)", fpCount)
	}

	// 指纹复核：approve → 终态；再判 409。
	if w := do("POST", "/behavior/fingerprints/1/approve"); w.Code != http.StatusOK {
		t.Fatalf("approve fingerprint: %d %s", w.Code, w.Body.String())
	}
	db.First(&fp, 1)
	if fp.Status != "approved" {
		t.Fatalf("fingerprint status = %s, want approved", fp.Status)
	}
	if w := do("POST", "/behavior/fingerprints/1/retire"); w.Code != http.StatusConflict {
		t.Fatalf("re-decide terminal fingerprint: %d", w.Code)
	}
}
