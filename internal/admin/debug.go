package admin

import (
	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crosslink/internal/debug"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
)

type DebugHandler struct {
	store      *debug.Store
	auditSvc   *service.AuditService
	db         *gorm.DB // 调试策略持久化（system_settings）；nil=不持久化（测试装配）
	keySvc     *service.KeyService
	port       int
	httpClient *http.Client
}

func NewDebugHandler(store *debug.Store, auditSvc *service.AuditService, keySvc *service.KeyService, port int, db *gorm.DB) *DebugHandler {
	return &DebugHandler{
		store:    store,
		auditSvc: auditSvc,
		db:       db,
		keySvc:   keySvc,
		port:     port,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse // don't follow redirects
			},
		},
	}
}

type debugEntrySummary struct {
	Seq           int64  `json:"seq"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	Duration      int64  `json:"duration_ms"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	Model         string `json:"model"`
	Status        int    `json:"status"`
	Stream        bool   `json:"stream"`
	Truncated     bool   `json:"truncated"`
	ReqBodyLen    int    `json:"req_body_len"`
	RespBodyLen   int    `json:"resp_body_len"`
	UpstreamCount int    `json:"upstream_count"`
	RetryCount    int    `json:"retry_count"`
	FallbackCount int    `json:"fallback_count"`
	LastUpstreamError string `json:"last_upstream_error,omitempty"`
}

func (h *DebugHandler) List(c *gin.Context) {
	var entries []*debug.Entry
	if orgID := GetOrgID(c); orgID != 0 {
		entries = h.store.ListByOrg(orgID)
	} else {
		entries = h.store.List()
	}
	summaries := make([]debugEntrySummary, 0, len(entries))
	for _, e := range entries {
		/* 链路诊断派生：重试/容灾次数与最后一次上游错误——HTTP 200 不代表链路无恙
		   （可能经两次重试才成功），列表层即可见异常链路 */
		retryCount, fallbackCount := 0, 0
		lastErr := ""
		for _, uc := range e.UpstreamCalls {
			if uc.IsRetry {
				retryCount++
			}
			if uc.IsFallback {
				fallbackCount++
			}
			if uc.Error != "" {
				lastErr = uc.Error
			}
		}
		summaries = append(summaries, debugEntrySummary{
			Seq:           e.Seq,
			ID:            e.ID,
			Timestamp:     e.Timestamp.Format("2006-01-02T15:04:05.000Z07:00"),
			Duration:      e.Duration.Milliseconds(),
			Method:        e.Method,
			Path:          e.Path,
			Model:         e.Model,
			Status:        e.RespStatus,
			Stream:        e.Stream,
			Truncated:     e.Truncated,
			ReqBodyLen:    len(e.ReqBody),
			RespBodyLen:   len(e.RespBody),
			UpstreamCount: len(e.UpstreamCalls),
			RetryCount:    retryCount,
			FallbackCount: fallbackCount,
			LastUpstreamError: lastErr,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": summaries})
}

type upstreamCallSummary struct {
	Seq         int                 `json:"seq"`
	Provider    string              `json:"provider"`
	Model       string              `json:"model"`
	BaseURL     string              `json:"base_url"`
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	ReqHeaders  map[string][]string `json:"req_headers"`
	ReqBody     string              `json:"req_body"`
	StatusCode  int                 `json:"status_code"`
	RespHeaders map[string][]string `json:"resp_headers"`
	RespBody    string              `json:"resp_body"`
	DurationMs  int64               `json:"duration_ms"`
	Attempt     int                 `json:"attempt"`
	IsRetry     bool                `json:"is_retry"`
	IsFallback  bool                `json:"is_fallback"`
	Error       string              `json:"error"`
}

type debugEntryDetail struct {
	Seq           int64                 `json:"seq"`
	ID            string                `json:"id"`
	Timestamp     string                `json:"timestamp"`
	Duration      int64                 `json:"duration_ms"`
	Method        string                `json:"method"`
	Path          string                `json:"path"`
	Stream        bool                  `json:"stream"`
	Truncated     bool                  `json:"truncated"`
	ReqHeaders    map[string][]string   `json:"req_headers"`
	ReqBody       string                `json:"req_body"`
	RespStatus    int                   `json:"resp_status"`
	RespHeaders   map[string][]string   `json:"resp_headers"`
	RespBody      string                `json:"resp_body"`
	UpstreamCalls []upstreamCallSummary `json:"upstream_calls"`
}

func (h *DebugHandler) Get(c *gin.Context) {
	seq, err := strconv.ParseInt(c.Param("seq"), 10, 64)
	if err != nil {
		errorResp(c, http.StatusNotFound, ErrEntryNotFound, "entry not found")
		return
	}
	entry := h.store.Get(seq)
	if entry == nil {
		errorResp(c, http.StatusNotFound, ErrEntryNotFound, "entry not found")
		return
	}

	if orgID := GetOrgID(c); orgID != 0 && entry.OrgID != orgID {
		errorResp(c, http.StatusNotFound, ErrEntryNotFound, "entry not found")
		return
	}

	// 查看原始报文入审计（调试数据安全：查看/重放/清空均可追溯）
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "debug:view", "debug", c.Param("seq"), entry.Path, nil)
	}

	reqBody := truncateBody(tryPrettyJSON(entry.ReqBody))
	respBody := string(entry.RespBody)
	if !entry.Stream {
		respBody = truncateBody(tryPrettyJSON(entry.RespBody))
	}

	// Build upstream call summaries
	var upstreamCalls []upstreamCallSummary
	for i, uc := range entry.UpstreamCalls {
		ucReqBody := truncateBody(tryPrettyJSON(uc.ReqBody))
		ucRespBody := string(uc.RespBody)
		upstreamCalls = append(upstreamCalls, upstreamCallSummary{
			Seq:         i + 1,
			Provider:    uc.Provider,
			Model:       uc.Model,
			BaseURL:     uc.BaseURL,
			Method:      uc.Method,
			Path:        uc.Path,
			ReqHeaders:  normalizeHeaders(uc.ReqHeaders),
			ReqBody:     ucReqBody,
			StatusCode:  uc.StatusCode,
			RespHeaders: normalizeHeaders(uc.RespHeaders),
			RespBody:    ucRespBody,
			DurationMs:  uc.Duration.Milliseconds(),
			Attempt:     uc.Attempt,
			IsRetry:     uc.IsRetry,
			IsFallback:  uc.IsFallback,
			Error:       uc.Error,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"data": debugEntryDetail{
			Seq:           entry.Seq,
			ID:            entry.ID,
			Timestamp:     entry.Timestamp.Format("2006-01-02T15:04:05.000Z07:00"),
			Duration:      entry.Duration.Milliseconds(),
			Method:        entry.Method,
			Path:          entry.Path,
			Stream:        entry.Stream,
			Truncated:     entry.Truncated,
			ReqHeaders:    normalizeHeaders(entry.ReqHeaders),
			ReqBody:       reqBody,
			RespStatus:    entry.RespStatus,
			RespHeaders:   normalizeHeaders(entry.RespHeaders),
			RespBody:      respBody,
			UpstreamCalls: upstreamCalls,
		},
	})
}

func (h *DebugHandler) Clear(c *gin.Context) {
	if orgID := GetOrgID(c); orgID != 0 {
		h.store.ClearByOrg(orgID)
	} else {
		h.store.Clear()
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "debug:clear", "debug", "", "", nil)
	}
	c.JSON(http.StatusOK, gin.H{"message": "cleared"})
}

const maxDebugBodySize = 10 * 1024 // 10 KB

// --- Replay ---

var replayPathWhitelist = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/messages":         true,
}

// Replay loads a historical Debug entry, merges admin-supplied overrides into
// the original request body, and re-sends it through the full gateway chain via
// an internal HTTP call to localhost. The admin provides an API key whose quota
// is consumed. See docs/plans/2026-07-17-request-replay-design.md.
func (h *DebugHandler) Replay(c *gin.Context) {
	seq, err := strconv.ParseInt(c.Param("seq"), 10, 64)
	if err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "debug:replay", "debug", c.Param("seq"), "", service.AuditDetail(map[string]any{"reason": "invalid_seq"}))
		}
		errorResp(c, http.StatusNotFound, ErrEntryNotFound, "entry not found")
		return
	}
	entry := h.store.Get(seq)
	if entry == nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "debug:replay", "debug", strconv.FormatInt(seq, 10), "", service.AuditDetail(map[string]any{"reason": "not_found"}))
		}
		errorResp(c, http.StatusNotFound, ErrEntryNotFound, "entry not found")
		return
	}
	if orgID := GetOrgID(c); orgID != 0 && entry.OrgID != orgID {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "debug:replay", "debug", strconv.FormatInt(seq, 10), entry.Path, service.AuditDetail(map[string]any{"reason": "org_scope_mismatch"}))
		}
		errorResp(c, http.StatusNotFound, ErrEntryNotFound, "entry not found")
		return
	}

	// Validate: truncated / path whitelist.
	if entry.Truncated {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "debug:replay", "debug", strconv.FormatInt(seq, 10), entry.Path, service.AuditDetail(map[string]any{"reason": "replay_truncated"}))
		}
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "replay_truncated: original body was truncated, cannot replay")
		return
	}
	if !replayPathWhitelist[entry.Path] {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "debug:replay", "debug", strconv.FormatInt(seq, 10), entry.Path, service.AuditDetail(map[string]any{"reason": "replay_path_not_allowed"}))
		}
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "replay_path_not_allowed: only /v1/chat/completions and /v1/messages are replayable")
		return
	}

	var input struct {
		Overrides map[string]any `json:"overrides"`
		KeyID     int64          `json:"key_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "debug:replay", "debug", strconv.FormatInt(seq, 10), entry.Path, service.AuditDetail(map[string]any{"reason": "invalid_request"}))
		}
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}

	// Org scoping: an org admin may only replay as keys their own org holds
	// (platform admins, orgID 0, are unrestricted). This is the enforcement
	// point for key ownership — the minted replay token carries no org, so the
	// gateway-side redeem cannot check it. Fail closed: no keySvc means an org
	// admin cannot replay at all.
	if orgID := GetOrgID(c); orgID != 0 {
		if h.keySvc == nil {
			if h.auditSvc != nil {
				h.auditSvc.LogFailure(c, "debug:replay", "debug", strconv.FormatInt(seq, 10), entry.Path, service.AuditDetail(map[string]any{"reason": "key_service_unavailable"}))
			}
			internalErr(c, errors.New("key service unavailable"), "replay key ownership check unavailable")
			return
		}
		if _, err := h.keySvc.GetByID(c.Request.Context(), orgID, input.KeyID); err != nil {
			if h.auditSvc != nil {
				h.auditSvc.LogFailure(c, "debug:replay", "debug", strconv.FormatInt(seq, 10), entry.Path, service.AuditDetail(map[string]any{"reason": "key_not_found"}))
			}
			errorResp(c, http.StatusNotFound, ErrKeyNotFound, "key not found")
			return
		}
	}

	// Parse original body and apply top-level overrides.
	var body map[string]any
	if err := json.Unmarshal(entry.ReqBody, &body); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "debug:replay", "debug", strconv.FormatInt(seq, 10), entry.Path, service.AuditDetail(map[string]any{"reason": "replay_invalid_body"}))
		}
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "replay_invalid_body: original request body is not valid JSON")
		return
	}
	for k, v := range input.Overrides {
		body[k] = v
	}
	delete(body, "x_context") // defensive: ContextAssembler already ran before Debug
	delete(body, "stream")    // replay as non-streaming (MVP doesn't proxy SSE)

	newBody, err := json.Marshal(body)
	if err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "debug:replay", "debug", strconv.FormatInt(seq, 10), entry.Path, service.AuditDetail(map[string]any{"reason": "marshal_failed", "error": err.Error()}))
		}
		internalErr(c, err, "replay marshal failed")
		return
	}

	// Internal HTTP call to self — full gateway chain.
	url := fmt.Sprintf("http://127.0.0.1:%d%s", h.port, entry.Path)
	req, err := http.NewRequestWithContext(c.Request.Context(), entry.Method, url, bytes.NewReader(newBody))
	if err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "debug:replay", "debug", strconv.FormatInt(seq, 10), entry.Path, service.AuditDetail(map[string]any{"reason": "request_creation_failed", "error": err.Error()}))
		}
		internalErr(c, err, "replay request creation failed")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// Single-use, 60s-TTL token bound to this key ID — the gateway Auth
	// middleware redeems it (see middleware/replay.go). Never send the raw
	// key ID alone: it is a small sequential integer and would authorize
	// anyone who can reach the app from a loopback source.
	replayToken := middleware.MintReplayToken(input.KeyID)
	if replayToken == "" {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "debug:replay", "debug", strconv.FormatInt(seq, 10), entry.Path, service.AuditDetail(map[string]any{"reason": "token_mint_failed"}))
		}
		internalErr(c, errors.New("replay token mint failed"), "replay token mint failed")
		return
	}
	req.Header.Set("X-Replay-Token", replayToken)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		slog.Error("replay: internal HTTP call failed", "seq", seq, "error", err)
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "debug:replay", "debug", strconv.FormatInt(seq, 10), entry.Path, service.AuditDetail(map[string]any{"reason": "replay_failed", "error": err.Error()}))
		}
		errorResp(c, http.StatusInternalServerError, "replay_failed", "internal replay request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB cap
	if err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "debug:replay", "debug", strconv.FormatInt(seq, 10), entry.Path, service.AuditDetail(map[string]any{"reason": "replay_failed", "error": err.Error()}))
		}
		errorResp(c, http.StatusInternalServerError, "replay_failed", "failed to read replay response")
		return
	}

	// Audit.
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "debug:replay", "debug", strconv.FormatInt(seq, 10), entry.Path, service.AuditDetail(map[string]any{
			"key_id":      input.KeyID,
			"resp_status": resp.StatusCode,
		}))
	}

	// Transparent passthrough: copy status + content-type + body.
	contentType := resp.Header.Get("Content-Type")
	if contentType != "" {
		c.Header("Content-Type", contentType)
	}
	c.Data(resp.StatusCode, contentType, respBody)
}

// truncateBody truncates a string to maxDebugBodySize and appends "[truncated]" if needed.
func truncateBody(s string) string {
	if len(s) <= maxDebugBodySize {
		return s
	}
	return s[:maxDebugBodySize] + "\n[truncated]"
}

func tryPrettyJSON(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	var v interface{}
	if err := json.Unmarshal(data, &v); err == nil {
		pretty, err := json.MarshalIndent(v, "", "  ")
		if err == nil {
			return string(pretty)
		}
	}
	return string(data)
}

func normalizeHeaders(h map[string][]string) map[string][]string {
	result := make(map[string][]string, len(h))
	for k, v := range h {
		result[http.CanonicalHeaderKey(k)] = v
	}
	// Sort keys for consistent display
	keys := make([]string, 0, len(result))
	for k := range result {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	_ = keys // iteration order already stable for map
	return result
}

// formatHeadersForDisplay formats headers as "Key: Value\n" string.
func formatHeadersForDisplay(h map[string][]string) string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		for _, v := range h[k] {
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// --- Debug config（调试数据安全四件套：开关/采样率/只记失败；容量与体积上限只读展示，改配置文件生效） ---

type debugConfigDTO struct {
	Enabled      bool `json:"enabled"`
	SamplingRate int  `json:"sampling_rate"` // 0-100：每百次请求记录的百分比（100=全量）
	OnlyFailed   bool `json:"only_failed"`   // true=仅记录 HTTP >= 400 的请求（降存储与隐私面）
	Capacity     int  `json:"capacity"`      // 只读：内存环形容量（条），随容量自动淘汰最旧记录
	MaxBodySize  int  `json:"max_body_size"` // 只读：单侧报文体积上限（字节）
}

// GetDebugConfig GET /admin/api/debug/config
func (h *DebugHandler) GetDebugConfig(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": debugConfigDTO{
		Enabled:      h.store.IsEnabled(),
		SamplingRate: h.store.SamplingRate(),
		OnlyFailed:   h.store.OnlyFailed(),
		Capacity:     h.store.Capacity(),
		MaxBodySize:  h.store.MaxBodySize(),
	}})
}

// UpdateDebugConfig PUT /admin/api/debug/config —— 三字段立即生效（原子，无需重启）；
// 变更入审计（采样策略变更直接影响可观测面，属安全相关配置）
func (h *DebugHandler) UpdateDebugConfig(c *gin.Context) {
	var input struct {
		Enabled      *bool `json:"enabled" binding:"required"`
		SamplingRate *int  `json:"sampling_rate" binding:"required,min=0,max=100"`
		OnlyFailed   *bool `json:"only_failed" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	before := debugConfigDTO{
		Enabled: h.store.IsEnabled(), SamplingRate: h.store.SamplingRate(), OnlyFailed: h.store.OnlyFailed(),
		Capacity: h.store.Capacity(), MaxBodySize: h.store.MaxBodySize(),
	}
	h.store.SetEnabled(*input.Enabled)
	h.store.SetSamplingRate(*input.SamplingRate)
	h.store.SetOnlyFailed(*input.OnlyFailed)
	// 持久化到 system_settings（app 启动时读回），重启不丢策略
	if h.db != nil {
	for _, kv := range []struct{ key, val string }{
		{"debug_mode", boolStr(*input.Enabled)},
		{"debug_sampling_rate", strconv.Itoa(*input.SamplingRate)},
		{"debug_only_failed", boolStr(*input.OnlyFailed)},
	} {
		if err := h.db.Save(&model.SystemSetting{Key: kv.key, Value: kv.val, UpdatedAt: time.Now().UTC()}).Error; err != nil {
			internalErr(c, err, "persist debug config failed")
			return
		}
	}
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "debug:update_config", "debug", "", "", service.AuditDetail(map[string]any{
			"before": map[string]any{"enabled": before.Enabled, "sampling_rate": before.SamplingRate, "only_failed": before.OnlyFailed},
			"after":  map[string]any{"enabled": *input.Enabled, "sampling_rate": *input.SamplingRate, "only_failed": *input.OnlyFailed},
		}))
	}
	c.JSON(http.StatusOK, gin.H{"data": debugConfigDTO{
		Enabled: h.store.IsEnabled(), SamplingRate: h.store.SamplingRate(), OnlyFailed: h.store.OnlyFailed(),
		Capacity: h.store.Capacity(), MaxBodySize: h.store.MaxBodySize(),
	}})
}

func boolStr(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
