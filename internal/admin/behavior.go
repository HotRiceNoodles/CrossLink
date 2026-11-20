package admin

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/crosslink/internal/behavior"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
)

// BehaviorHandler 是行为基线异常台账的管理面（规划冻结 v4.0 §5.9/§5.6；
// D4：台账 CRUD 属 Community 机制，路由以 behavior:view/manage 动作位定档 Pro）。
//
// confirm/dismiss 为终态裁决：只接受 open 事件（终态不可重开/改判，D16），
// 同时按 §5.6 公式把反馈写进 behavior_tolerances（永不改写基线数据，D10）。
type BehaviorHandler struct {
	db       *gorm.DB
	auditSvc *service.AuditService // nil in Community
}

func NewBehaviorHandler(db *gorm.DB, auditSvc *service.AuditService) *BehaviorHandler {
	return &BehaviorHandler{db: db, auditSvc: auditSvc}
}

var validAnomalyStatus = map[string]bool{
	behavior.AnomalyStatusOpen: true, behavior.AnomalyStatusConfirmed: true,
	behavior.AnomalyStatusDismissed: true, behavior.AnomalyStatusExpired: true,
}

// ListAnomalies GET /admin/api/behavior/anomalies
// 筛选：status / band / entity_type / entity_id / org_id；分页 page、page_size。
func (h *BehaviorHandler) ListAnomalies(c *gin.Context) {
	q := h.db.WithContext(c.Request.Context()).Model(&model.BehaviorAnomaly{})
	if s := c.Query("status"); s != "" {
		if !validAnomalyStatus[s] {
			errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "status must be one of open|confirmed|dismissed|expired")
			return
		}
		q = q.Where("status = ?", s)
	}
	if b := c.Query("band"); b != "" {
		q = q.Where("band = ?", b)
	}
	if et := c.Query("entity_type"); et != "" {
		q = q.Where("entity_type = ?", et)
	}
	if eid := parseID(c.Query("entity_id")); eid != 0 {
		q = q.Where("entity_id = ?", eid)
	}
	if oid := parseID(c.Query("org_id")); oid != 0 {
		q = q.Where("org_id = ?", oid)
	}

	page, pageSize := 1, 20
	if v, err := strconv.Atoi(c.Query("page")); err == nil && v > 0 {
		page = v
	}
	if v, err := strconv.Atoi(c.Query("page_size")); err == nil && v > 0 {
		pageSize = v
	}
	if pageSize > 100 {
		pageSize = 100
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		internalErr(c, err, "count anomalies failed")
		return
	}
	var rows []model.BehaviorAnomaly
	if err := q.Order("last_hit_at DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&rows).Error; err != nil {
		internalErr(c, err, "list anomalies failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": total, "page": page, "page_size": pageSize})
}

// GetAnomaly GET /admin/api/behavior/anomalies/:id
func (h *BehaviorHandler) GetAnomaly(c *gin.Context) {
	id := parseID(c.Param("id"))
	if id == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return
	}
	var ev model.BehaviorAnomaly
	if err := h.db.WithContext(c.Request.Context()).First(&ev, id).Error; err != nil {
		errorResp(c, http.StatusNotFound, ErrNotFound, "not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": ev})
}

// Confirm POST /admin/api/behavior/anomalies/:id/confirm —— 判定为真异常（终态）。
func (h *BehaviorHandler) Confirm(c *gin.Context) { h.decide(c, behavior.AnomalyStatusConfirmed) }

// Dismiss POST /admin/api/behavior/anomalies/:id/dismiss —— 判定为误报（终态）。
func (h *BehaviorHandler) Dismiss(c *gin.Context) { h.decide(c, behavior.AnomalyStatusDismissed) }

func (h *BehaviorHandler) decide(c *gin.Context, decision string) {
	id := parseID(c.Param("id"))
	if id == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return
	}
	ctx := c.Request.Context()
	var ev model.BehaviorAnomaly
	if err := h.db.WithContext(ctx).First(&ev, id).Error; err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "behavior:"+decision, "behavior_anomaly", c.Param("id"), "", service.AuditDetail(map[string]any{"reason": "not_found"}))
		}
		errorResp(c, http.StatusNotFound, ErrNotFound, "not found")
		return
	}
	if ev.Status != behavior.AnomalyStatusOpen {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "behavior:"+decision, "behavior_anomaly", c.Param("id"), "", service.AuditDetail(map[string]any{"reason": "terminal_state", "current_status": ev.Status}))
		}
		// D16：confirmed/dismissed 为终态，不重开、不改判。
		errorResp(c, http.StatusConflict, ErrConflict, "event already "+ev.Status+" (terminal; a recurrence opens a new case)")
		return
	}

	decidedBy := ""
	if v, ok := c.Get("username"); ok {
		if s, _ := v.(string); s != "" {
			decidedBy = s
		}
	}
	now := time.Now().UTC()
	updates := map[string]any{
		"status":        decision,
		"decided_by":    decidedBy,
		"closed_reason": decision,
		"updated_at":    now,
	}
	if err := h.db.WithContext(ctx).Model(&model.BehaviorAnomaly{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "behavior:"+decision, "behavior_anomaly", c.Param("id"), "", service.AuditDetail(map[string]any{"reason": "internal_error", "error": err.Error()}))
		}
		internalErr(c, err, "decide anomaly failed")
		return
	}

	// 反馈闭环（§5.6/D10/D14）：只作用于事件命中过的维度，永不改写基线数据。
	// 反馈失败不回滚裁决（终态已落），但在响应里显式透出（不静默吞错）。
	dims := behavior.FiredDimensions("")
	if ev.Dimensions != nil {
		dims = behavior.FiredDimensions(*ev.Dimensions)
	}
	feedbackWarning := ""
	if len(dims) > 0 {
		if err := behavior.ApplyToleranceFeedback(ctx, h.db, ev.EntityType, ev.EntityID, decision, dims, now); err != nil {
			feedbackWarning = err.Error()
			if h.auditSvc != nil {
				h.auditSvc.LogFailure(c, "behavior:"+decision, "behavior_anomaly", c.Param("id"), "", service.AuditDetail(map[string]any{"reason": "tolerance_feedback_failed", "error": err.Error()}))
			}
		}
	}

	// M3 反馈闭环另一半（§5.6/D10）：confirmed 额外产出**行为指纹候选**，进入
	// 人在环复核（approve/retire 端点见下）——审批不自动生效（observe-only）。
	fingerprintNote := ""
	if decision == behavior.AnomalyStatusConfirmed {
		if cand := behavior.BuildFingerprintCandidate(&ev); cand != nil {
			if err := behavior.UpsertFingerprintCandidate(ctx, h.db, cand); err != nil {
				fingerprintNote = "fingerprint candidate write failed: " + err.Error()
				if h.auditSvc != nil {
					h.auditSvc.LogFailure(c, "behavior:"+decision, "behavior_anomaly", c.Param("id"), "", service.AuditDetail(map[string]any{"reason": "fingerprint_candidate_failed", "error": err.Error()}))
				}
			} else {
				fingerprintNote = cand.DimensionSet
			}
		}
	}

	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "behavior:"+decision, "behavior_anomaly", c.Param("id"), ev.Band, service.AuditDetail(map[string]any{
			"entity_type": ev.EntityType, "entity_id": ev.EntityID,
			"band": ev.Band, "hit_count": ev.HitCount, "dimensions": dims,
			"fingerprint_candidate": fingerprintNote,
		}))
	}
	resp := gin.H{"data": gin.H{"id": id, "status": decision, "decided_by": decidedBy}}
	if feedbackWarning != "" {
		resp["tolerance_warning"] = feedbackWarning
	}
	if fingerprintNote != "" {
		resp["fingerprint"] = fingerprintNote
	}
	c.JSON(http.StatusOK, resp)
}

// ListFingerprints GET /admin/api/behavior/fingerprints —— 候选/已批/已退清单。
func (h *BehaviorHandler) ListFingerprints(c *gin.Context) {
	q := h.db.WithContext(c.Request.Context()).Model(&model.BehaviorFingerprint{})
	if s := c.Query("status"); s != "" {
		switch s {
		case "candidate", "approved", "retired":
		default:
			errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "status must be one of candidate|approved|retired")
			return
		}
		q = q.Where("status = ?", s)
	}
	page, pageSize := 1, 20
	if v, err := strconv.Atoi(c.Query("page")); err == nil && v > 0 {
		page = v
	}
	if v, err := strconv.Atoi(c.Query("page_size")); err == nil && v > 0 {
		pageSize = v
	}
	if pageSize > 100 {
		pageSize = 100
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		internalErr(c, err, "count fingerprints failed")
		return
	}
	var rows []model.BehaviorFingerprint
	if err := q.Order("updated_at DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&rows).Error; err != nil {
		internalErr(c, err, "list fingerprints failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": total, "page": page, "page_size": pageSize})
}

// decideFingerprint 是 approve/retire 的共用体：仅 candidate 可判（终态不可改）。
func (h *BehaviorHandler) decideFingerprint(c *gin.Context, status string) {
	id := parseID(c.Param("id"))
	if id == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return
	}
	ctx := c.Request.Context()
	var fp model.BehaviorFingerprint
	if err := h.db.WithContext(ctx).First(&fp, id).Error; err != nil {
		errorResp(c, http.StatusNotFound, ErrNotFound, "not found")
		return
	}
	if fp.Status != "candidate" {
		// 终态不可改判（与台账裁决同纪律：复核结论优先，再现走新候选计数）。
		errorResp(c, http.StatusConflict, ErrConflict, "fingerprint already "+fp.Status+" (terminal)")
		return
	}
	decidedBy := ""
	if v, ok := c.Get("username"); ok {
		if s, _ := v.(string); s != "" {
			decidedBy = s
		}
	}
	if err := h.db.WithContext(ctx).Model(&model.BehaviorFingerprint{}).Where("id = ?", id).
		Updates(map[string]any{"status": status, "decided_by": decidedBy, "updated_at": time.Now().UTC()}).Error; err != nil {
		internalErr(c, err, "decide fingerprint failed")
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "behavior:fingerprint_"+status, "behavior_fingerprint", c.Param("id"), fp.DimensionSet, service.AuditDetail(map[string]any{
			"entity_type": fp.EntityType, "entity_id": fp.EntityID, "hit_count": fp.HitCount,
		}))
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": id, "status": status, "decided_by": decidedBy}})
}

// ApproveFingerprint POST /admin/api/behavior/fingerprints/:id/approve
// —— 记为已知异常模式；**不自动生效**（不触发任何处置，D2 observe-only）。
func (h *BehaviorHandler) ApproveFingerprint(c *gin.Context) { h.decideFingerprint(c, "approved") }

// RetireFingerprint POST /admin/api/behavior/fingerprints/:id/retire —— 误报模式退役。
func (h *BehaviorHandler) RetireFingerprint(c *gin.Context) { h.decideFingerprint(c, "retired") }

// Coverage GET /admin/api/behavior/coverage —— 基线覆盖率如实呈现（G0 门槛 5：
// 不允许静默无保护）。近 7 天有活动的实体 × 指标，标注是否已有基线。
func (h *BehaviorHandler) Coverage(c *gin.Context) {
	since := time.Now().UTC().Add(-7 * 24 * time.Hour)
	type coverRow struct {
		EntityType      string `json:"entity_type"`
		EntityID        int64  `json:"entity_id"`
		RollupMetrics   int    `json:"rollup_metrics"`
		BaselineMetrics int    `json:"baseline_metrics"`
		Protected       bool   `json:"protected"`
	}
	var rows []coverRow
	err := h.db.WithContext(c.Request.Context()).Raw(`
SELECT r.entity_type AS entity_type, r.entity_id AS entity_id,
       COUNT(DISTINCT r.metric) AS rollup_metrics,
       COUNT(DISTINCT b.metric) AS baseline_metrics
FROM (
    SELECT DISTINCT entity_type, entity_id, metric
    FROM behavior_rollups
    WHERE granularity = 'hour' AND bucket_ts >= ?
      AND metric NOT LIKE 'model:%' AND metric NOT LIKE 'grule:%'
) r
LEFT JOIN behavior_baselines b
  ON b.entity_type = r.entity_type AND b.entity_id = r.entity_id AND b.metric = r.metric
GROUP BY r.entity_type, r.entity_id
ORDER BY r.entity_type, r.entity_id`, since).Scan(&rows).Error
	if err != nil {
		internalErr(c, err, "coverage query failed")
		return
	}
	for i := range rows {
		rows[i].Protected = rows[i].BaselineMetrics >= rows[i].RollupMetrics
	}
	c.JSON(http.StatusOK, gin.H{"data": rows})
}
