package admin

/* ProviderHealth —— 运行时熔断视图（2026-11-10）：
   熔断是 HealthTracker 的进程内存态（不落库、不改 provider/model 的 status 字段、
   到期自动半开恢复），控制台此前完全不可见。本文件把活跃电路（open/half_open）
   透出给「容灾与重试」页，并提供管理员强制恢复——等价于对该电路执行进程内重建，
   替代"等冷却/重启进程"两种极端。closed 电路不返回（无运营意义）。 */

import (
	"net/http"
	"strings"
	"time"

	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
)

// SetHealthTracker injects the runtime health tracker. Called from app.go after
// construction (same pattern as SetFixtureStore).
func (h *ProviderHandler) SetHealthTracker(t *provider.HealthTracker) { h.health = t }

// providerCircuitDTO 是对外字段白名单：只有运行时电路事实，绝不含供应商连接
// 信息（base_url/密钥等，纪律同 pat_read 的 patHealthProviderDTO）。
type providerCircuitDTO struct {
	Provider string `json:"provider"`
	Model    string `json:"model"` // "" = 账号级电路（整供应商），非空 = 模型级
	State    string `json:"state"` // open | half_open
	Kind     string `json:"kind"`  // persistent | transient
	// OpenUntil 冷却截止（RFC3339）；half_open 时已过期但仍列出（等待探测结论）
	OpenUntil *time.Time `json:"open_until,omitempty"`
	// RecoverInSeconds 半开预计自动恢复剩余秒数；half_open=0（已可被试探）
	RecoverInSeconds int64  `json:"recover_in_seconds"`
	OpenCount        int    `json:"open_count"` // 连续打开次数（指数升级因子）
	ErrorType        string `json:"error_type,omitempty"`
	ErrorMessage     string `json:"error_message,omitempty"`
}

// Circuits GET /admin/api/providers/health —— 当前活跃熔断（只读快照）。
// 多实例部署时熔断态每进程一份：响应标注 server_now 供前端无偏倒计时，
// 消费方应向各网关实例分别取数或按实例标注（单实例部署如实直读）。
func (h *ProviderHandler) Circuits(c *gin.Context) {
	if h.health == nil {
		errorResp(c, http.StatusServiceUnavailable, "health_tracker_unavailable", "health tracker not wired")
		return
	}
	now := time.Now()
	rows := make([]providerCircuitDTO, 0)
	for _, s := range h.health.Snapshot() {
		if s.State == provider.CircuitClosed.String() {
			continue // 只透出活跃电路；closed 是常态不产生噪音
		}
		row := providerCircuitDTO{
			Provider:     s.Provider,
			Model:        s.Model,
			State:        s.State,
			Kind:         s.Kind,
			OpenCount:    s.OpenCount,
			ErrorType:    s.LastErrorType,
			ErrorMessage: truncateStr(s.LastErrorMessage, 160),
		}
		if !s.Until.IsZero() {
			t := s.Until
			row.OpenUntil = &t
			if d := t.Sub(now); d > 0 {
				row.RecoverInSeconds = int64(d.Seconds()) + 1 // 向上取整，展示不早于实际
			}
		}
		rows = append(rows, row)
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"server_now": now.UTC(),
		"circuits":   rows,
	}})
}

// ResetCircuit POST /admin/api/providers/health/reset —— 管理员强制闭合电路。
// 绕过 C1 竞态保护是有意的：C1 防的是"自动成功误擦刚设置的持久熔断"，
// 手动恢复是显式运维意图（记审计）。绕过不等价于无保护——下一次真实失败
// 会立刻重新打开电路，保护语义完整。
func (h *ProviderHandler) ResetCircuit(c *gin.Context) {
	if h.health == nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "provider:update", "provider_circuit", "", "", service.AuditDetail(map[string]any{"reason": "health_tracker_unavailable"}))
		}
		errorResp(c, http.StatusServiceUnavailable, "health_tracker_unavailable", "health tracker not wired")
		return
	}
	var input struct {
		Provider string `json:"provider" binding:"required"`
		Model    string `json:"model"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "provider:update", "provider_circuit", input.Provider, input.Provider, service.AuditDetail(map[string]any{"reason": "invalid_request"}))
		}
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	input.Provider = strings.TrimSpace(input.Provider)
	if input.Provider == "" {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "provider:update", "provider_circuit", input.Provider, input.Provider, service.AuditDetail(map[string]any{"reason": "invalid_request"}))
		}
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "provider must not be empty")
		return
	}
	h.health.ForceClear(input.Provider, input.Model)
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "provider:update", "provider_circuit", input.Provider, input.Provider,
			service.AuditDetail(map[string]any{"action": "circuit_reset", "provider": input.Provider, "model": input.Model}))
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"reset": true, "provider": input.Provider, "model": input.Model}})
}
