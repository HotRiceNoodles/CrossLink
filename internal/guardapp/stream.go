package guardapp

import (
	"container/list"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
)

// 结构化流式 API（设计 DD-2 ①，M3）：POST /api/v1/traces/{trace_id}/stream
// 按分片提交响应流，final 或空闲超时触发一次整段终检（MVP 不做增量水位
// ——旁路不回写，终检已满足检测语义）。分片文本只存在于 TTL 窗口内的
// 进程内存（原文红线 DD-5 的既定豁免区），终检即释放。
//
// 分片序号仅用于幂等（重复 seq 409）；乱序到达按到达序拼接（与 SSE 终检
// 同语义，排序责任在客户端）。
const (
	// streamDirection 幂等方向键：与非流式 response 提交互不挤占——同一
	// trace 两条路径都提交属客户端误用，但不应互相吞幂等位。
	streamDirection = "response_stream"
	// streamIdleTimeout 空闲终检：最后一个分片后静默即视为流结束（客户端
	// 崩溃/断连时终检不悬空）；TTL 仍是硬上限。
	streamIdleTimeout = 5 * time.Minute
	// streamField* 分片字段：content 是终检主文本，reasoning 仅在 content
	// 全空时兜底（推理链同样可检，但绝不与 content 混拼）。
	streamFieldContent   = "content"
	streamFieldReasoning = "reasoning"
)

// streamChunk 分片提交体。model/metadata/tenant 随首个分片生效（后续分片
// 的同名非空值不覆盖——流属性以首片为准）。
type streamChunk struct {
	Event    string          `json:"event"`
	Seq      int64           `json:"seq"`
	Field    string          `json:"field,omitempty"`
	Text     string          `json:"text,omitempty"`
	Model    string          `json:"model,omitempty"`
	Tenant   string          `json:"tenant,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// streamItem 一条流缓冲：按字段累积的分片文本 + 流属性 + 空闲终检定时器。
type streamItem struct {
	orgID     int64
	traceID   string
	fields    map[string]*strings.Builder
	model     string
	metadata  map[string]string
	expiresAt time.Time
	timer     *time.Timer
}

func (it *streamItem) text() string {
	if t := it.fields[streamFieldContent].String(); t != "" {
		return t
	}
	return it.fields[streamFieldReasoning].String()
}

// StreamStore 流式分片缓冲：与 TraceStore/idemStore 同款治理（TTL+LRU+
// 容量上限），淘汰计 cl_guard_ingest_dropped_total（整条流未被终检=有
// 流量未被检测，与队列打满同一观测面）。
type StreamStore struct {
	mu         sync.Mutex
	lru        *list.List // front=最近；element.Value=*streamItem
	items      map[string]*list.Element
	ttl        time.Duration
	maxEntries int
	idle       time.Duration
	now        func() time.Time

	// finalize 在锁外回调（终检递交不得持有 store 锁）。
	finalize func(req *DetectRequest)

	evictedTTL atomic.Int64
	evictedLRU atomic.Int64
}

// NewStreamStore：ttl/maxEntries 复用 guard.trace_store 治理配置（同一
// 配对窗口语义）；finalize 收空闲终检的检测请求（SubmitAsync 语义）。
func NewStreamStore(ttl time.Duration, maxEntries int, finalize func(req *DetectRequest)) *StreamStore {
	return &StreamStore{
		lru:        list.New(),
		items:      make(map[string]*list.Element),
		ttl:        ttl,
		maxEntries: maxEntries,
		idle:       streamIdleTimeout,
		now:        time.Now,
		finalize:   finalize,
	}
}

// Append 追加一个分片：建/刷新流条目（TTL 顺延）并重置空闲定时器。返回
// 当前有文本的字段数。容量淘汰在锁内完成，被淘汰流的定时器一并停止。
func (s *StreamStore) Append(orgID int64, traceID, field, text, model string, metadata map[string]string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := traceKey(orgID, traceID)
	var it *streamItem
	if el, ok := s.items[key]; ok {
		it = el.Value.(*streamItem)
	} else {
		it = &streamItem{
			orgID: orgID, traceID: traceID,
			fields: map[string]*strings.Builder{streamFieldContent: {}, streamFieldReasoning: {}},
			model:  model, metadata: metadata,
		}
		s.items[key] = s.lru.PushFront(it)
		for s.lru.Len() > s.maxEntries {
			back := s.lru.Back()
			if back == nil {
				break
			}
			victim := back.Value.(*streamItem)
			victim.timer.Stop()
			s.lru.Remove(back)
			delete(s.items, traceKey(victim.orgID, victim.traceID))
			s.evictedLRU.Add(1)
			ingestDroppedTotal.Inc()
		}
	}
	if text != "" {
		if it.fields[field] == nil {
			it.fields[field] = &strings.Builder{}
		}
		it.fields[field].WriteString(text)
	}
	if it.timer != nil {
		it.timer.Stop()
	}
	it.timer = time.AfterFunc(s.idle, func() { s.finalizeIdle(orgID, traceID) })
	it.expiresAt = s.now().Add(s.ttl)
	s.lru.MoveToFront(s.items[key])
	n := 0
	for _, b := range it.fields {
		if b.Len() > 0 {
			n++
		}
	}
	return n
}

// Finalize 主动终检（final 分片）：取走并删除条目（一次性）。返回 nil
// 表示无可终检文本（从未提交分片 / 已终检 / TTL 过期 / LRU 淘汰 / 空流）。
func (s *StreamStore) Finalize(orgID int64, traceID string) *DetectRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.takeLocked(orgID, traceID)
}

// finalizeIdle 空闲终检回调（定时器 goroutine）：条目仍在（未被 final
// 抢先/未被淘汰）才终检；递交在锁外执行。
func (s *StreamStore) finalizeIdle(orgID int64, traceID string) {
	s.mu.Lock()
	req := s.takeLocked(orgID, traceID)
	s.mu.Unlock()
	if req != nil && s.finalize != nil {
		s.finalize(req)
	}
}

// takeLocked 删除并返回终检请求；过期条目按 TTL 淘汰计数，空流/无条目
// 返回 nil。必须已持有 s.mu。
func (s *StreamStore) takeLocked(orgID int64, traceID string) *DetectRequest {
	key := traceKey(orgID, traceID)
	el, ok := s.items[key]
	if !ok {
		return nil
	}
	it := el.Value.(*streamItem)
	if it.timer != nil {
		it.timer.Stop()
	}
	s.lru.Remove(el)
	delete(s.items, key)
	if s.now().After(it.expiresAt) {
		s.evictedTTL.Add(1)
		ingestDroppedTotal.Inc()
		return nil
	}
	if it.text() == "" {
		return nil
	}
	return &DetectRequest{
		OrgID:     orgID,
		TraceID:   traceID,
		Direction: string(guardrail.DirectionResponse),
		Text:      it.text(),
		Model:     it.model,
		Metadata:  it.metadata,
	}
}

// Evicted 返回两类淘汰计数（TTL 过期 / LRU 容量），供指标暴露。
func (s *StreamStore) Evicted() (ttl, lru int64) {
	return s.evictedTTL.Load(), s.evictedLRU.Load()
}

// handleStreamChunk 分片提交：chunk 累积、final 终检（异步递交，影子判定
// ——响应体只报受理，不回同步裁决）。
func (a *App) handleStreamChunk(c *gin.Context) {
	traceID := c.Param("trace_id")
	if len(traceID) > 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_trace_id",
			"message": "trace_id exceeds 64 chars"}})
		return
	}
	limit := a.Cfg.Guard.Ingest.MaxBodyBytes
	if limit <= 0 {
		limit = 10 << 20
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, limit+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "read_error", "message": err.Error()}})
		return
	}
	if int64(len(body)) > limit {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": gin.H{"code": "payload_too_large",
			"message": "content exceeds ingest body limit"}})
		return
	}
	var p streamChunk
	if err := json.Unmarshal(body, &p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "malformed_json", "message": err.Error()}})
		return
	}
	if p.Event != "chunk" && p.Event != "final" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_event",
			"message": "event must be chunk or final"}})
		return
	}
	if p.Field == "" {
		p.Field = streamFieldContent
	}
	if p.Field != streamFieldContent && p.Field != streamFieldReasoning {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_field",
			"message": "field must be content or reasoning"}})
		return
	}
	if !a.checkTenant(c, p.Tenant) {
		return
	}
	// 幂等：重复 seq 409（结构化 API 家族语义，客户可感知重试）。
	if !a.Idem.Seen(c.GetInt64("org_id"), traceID, streamDirection, p.Seq) {
		c.JSON(http.StatusConflict, gin.H{"error": gin.H{"code": "duplicate",
			"message": "stream chunk already ingested"}})
		return
	}

	if p.Event == "chunk" {
		n := a.Streams.Append(c.GetInt64("org_id"), traceID, p.Field, p.Text, p.Model, MetadataMap(p.Metadata))
		c.JSON(http.StatusAccepted, gin.H{"accepted": true, "trace_id": traceID, "buffered": n})
		return
	}

	// final：并入本片文本后取缓冲终检（final 常是最后一段增量——直接取
	// 缓冲会丢尾段，跨 chunk/final 的关键词漏检）。无缓冲/空流记覆盖
	// 缺口事件（fire-and-forget 语义不回错误，202 如实告知）。
	if p.Text != "" {
		a.Streams.Append(c.GetInt64("org_id"), traceID, p.Field, p.Text, p.Model, MetadataMap(p.Metadata))
	}
	req := a.Streams.Finalize(c.GetInt64("org_id"), traceID)
	if req == nil {
		a.recordStreamGap(c, traceID, "stream finalized with no buffered text")
		c.JSON(http.StatusAccepted, gin.H{"accepted": false, "trace_id": traceID, "reason": "no_buffer"})
		return
	}
	if !a.Pipeline.SubmitAsync(req) {
		c.JSON(http.StatusAccepted, gin.H{"accepted": false, "trace_id": traceID, "dropped": true})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"accepted": true, "trace_id": traceID, "finalized": true})
}

// recordStreamGap 落一条覆盖缺口事件（与镜像端拒绝同观测面：有流量到达
// 但未被检测）。
func (a *App) recordStreamGap(c *gin.Context, traceID, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := a.Pipeline.events.Create(ctx, &model.GuardEvent{
		TraceID:    traceID,
		OrgID:      c.GetInt64("org_id"),
		EngineType: engineGuardService,
		Direction:  string(guardrail.DirectionResponse),
		Action:     "log",
		Severity:   "medium",
		Reason:     sanitizePreview(truncateRunes(message, reasonMaxRunes)),
		CreatedAt:  time.Now(),
	})
	if err != nil {
		slog.Warn("guard: persist stream-gap event", "error", err, "trace_id", traceID)
	}
}
