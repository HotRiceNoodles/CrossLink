package admin

import (
	"encoding/json"
	"net/http"
	"sort"
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
	ID          string `json:"id"`
	Timestamp   string `json:"timestamp"`
	Duration    int64  `json:"duration_ms"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Model       string `json:"model"`
	Status      int    `json:"status"`
	Stream      bool   `json:"stream"`
	Truncated   bool   `json:"truncated"`
	ReqBodyLen  int    `json:"req_body_len"`
	RespBodyLen int    `json:"resp_body_len"`
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
			ID:          e.ID,
			Timestamp:   e.Timestamp.Format("2006-01-02T15:04:05.000Z07:00"),
			Duration:    e.Duration.Milliseconds(),
			Method:      e.Method,
			Path:        e.Path,
			Model:       e.Model,
			Status:      e.RespStatus,
			Stream:      e.Stream,
			Truncated:   e.Truncated,
			ReqBodyLen:  len(e.ReqBody),
			RespBodyLen: len(e.RespBody),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": summaries})
}

type debugEntryDetail struct {
	ID          string              `json:"id"`
	Timestamp   string              `json:"timestamp"`
	Duration    int64               `json:"duration_ms"`
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	Stream      bool                `json:"stream"`
	Truncated   bool                `json:"truncated"`
	ReqHeaders  map[string][]string `json:"req_headers"`
	ReqBody     string              `json:"req_body"`
	RespStatus  int                 `json:"resp_status"`
	RespHeaders map[string][]string `json:"resp_headers"`
	RespBody    string              `json:"resp_body"`
}

func (h *DebugHandler) Get(c *gin.Context) {
	id := c.Param("id")
	entry := h.store.Get(id)
	if entry == nil {
		errorResp(c, http.StatusNotFound, ErrEntryNotFound, "entry not found")
		return
	}

	if orgID := GetOrgID(c); orgID != 0 && entry.OrgID != orgID {
		errorResp(c, http.StatusNotFound, ErrEntryNotFound, "entry not found")
		return
	}

	reqBody := tryPrettyJSON(entry.ReqBody)
	respBody := string(entry.RespBody)
	if !entry.Stream {
		respBody = tryPrettyJSON(entry.RespBody)
	}

	c.JSON(http.StatusOK, gin.H{
		"data": debugEntryDetail{
			ID:          entry.ID,
			Timestamp:   entry.Timestamp.Format("2006-01-02T15:04:05.000Z07:00"),
			Duration:    entry.Duration.Milliseconds(),
			Method:      entry.Method,
			Path:        entry.Path,
			Stream:      entry.Stream,
			Truncated:   entry.Truncated,
			ReqHeaders:  normalizeHeaders(entry.ReqHeaders),
			ReqBody:     reqBody,
			RespStatus:  entry.RespStatus,
			RespHeaders: normalizeHeaders(entry.RespHeaders),
			RespBody:    respBody,
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

func tryPrettyJSON(data []byte) string {
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
