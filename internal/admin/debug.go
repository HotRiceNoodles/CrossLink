package admin

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/debug"
	"github.com/crosslink/internal/service"
)

type DebugHandler struct {
	store    *debug.Store
	auditSvc *service.AuditService
}

func NewDebugHandler(store *debug.Store, auditSvc *service.AuditService) *DebugHandler {
	return &DebugHandler{store: store, auditSvc: auditSvc}
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
	Seq           int64               `json:"seq"`
	ID            string              `json:"id"`
	Timestamp     string              `json:"timestamp"`
	Duration      int64               `json:"duration_ms"`
	Method        string              `json:"method"`
	Path          string              `json:"path"`
	Stream        bool                `json:"stream"`
	Truncated     bool                `json:"truncated"`
	ReqHeaders    map[string][]string `json:"req_headers"`
	ReqBody       string              `json:"req_body"`
	RespStatus    int                 `json:"resp_status"`
	RespHeaders   map[string][]string `json:"resp_headers"`
	RespBody      string              `json:"resp_body"`
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
