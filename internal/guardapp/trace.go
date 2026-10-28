package guardapp

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
)

// TraceStore 是请求/响应配对的内存存储：请求向检测完成时存下检测文本，
// 响应向提交时取回作为 CheckRequest.Prompt（上下文引擎按网关同语义消费）。
//
// 原文红线（设计 DD-5）：检测执行内存与 TTL 配对窗口是原文仅存的两处，
// 到期即弃；容量上限触发 LRU 淘汰（镜像流量可能永远等不到响应）。
type TraceStore struct {
	mu         sync.Mutex
	lru        *list.List // front=最近使用；element.Value=*traceItem
	items      map[string]*list.Element
	ttl        time.Duration
	maxEntries int
	now        func() time.Time // 可注入时钟

	evictedTTL atomic.Int64
	evictedLRU atomic.Int64
}

type traceItem struct {
	key       string
	prompt    string
	expiresAt time.Time
}

func NewTraceStore(ttl time.Duration, maxEntries int) *TraceStore {
	return &TraceStore{
		lru:        list.New(),
		items:      make(map[string]*list.Element),
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        time.Now,
	}
}

func traceKey(orgID int64, traceID string) string {
	return fmt.Sprintf("%d|%s", orgID, traceID)
}

// SavePrompt 写入/刷新配对条目并移到 LRU 头部；超容量时淘汰尾部。
func (s *TraceStore) SavePrompt(orgID int64, traceID, prompt string) {
	key := traceKey(orgID, traceID)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		it := el.Value.(*traceItem)
		it.prompt = prompt
		it.expiresAt = now.Add(s.ttl)
		s.lru.MoveToFront(el)
		return
	}
	s.items[key] = s.lru.PushFront(&traceItem{key: key, prompt: prompt, expiresAt: now.Add(s.ttl)})
	for s.lru.Len() > s.maxEntries {
		back := s.lru.Back()
		if back == nil {
			break
		}
		s.lru.Remove(back)
		delete(s.items, back.Value.(*traceItem).key)
		s.evictedLRU.Add(1)
	}
}

// TakePrompt 取回并删除配对条目（一次性：幂等层保证同一 trace 不会重复
// 提交响应）。条目过期按 TTL 淘汰计数，视为未命中。
func (s *TraceStore) TakePrompt(orgID int64, traceID string) (string, bool) {
	key := traceKey(orgID, traceID)
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.items[key]
	if !ok {
		return "", false
	}
	it := el.Value.(*traceItem)
	s.lru.Remove(el)
	delete(s.items, key)
	if s.now().After(it.expiresAt) {
		s.evictedTTL.Add(1)
		return "", false
	}
	return it.prompt, true
}

// Evicted 返回两类淘汰计数（TTL 过期 / LRU 容量），供指标暴露。
func (s *TraceStore) Evicted() (ttl, lru int64) {
	return s.evictedTTL.Load(), s.evictedLRU.Load()
}

// verdict 汇总档位：高档覆盖低档（pending < pass < flagged < would_block）。
// error 是执行失败而非内容判定：只覆盖 pending，不覆盖已有裁决。
var verdictRank = map[string]int{model.GuardVerdictPending: 0, model.GuardVerdictPass: 1, model.GuardVerdictFlagged: 2, model.GuardVerdictWouldBlock: 3}

// VerdictFromCheck 把一次引擎裁决映射为 trace verdict。旁路语义：block 的
// 影子判定记 would_block；review/mask/log 命中记 flagged；无命中记 pass。
func VerdictFromCheck(res *guardrail.CheckResponse, checkErr error) string {
	if checkErr != nil {
		return model.GuardVerdictError
	}
	if res == nil {
		return model.GuardVerdictPass
	}
	if res.Blocked {
		return model.GuardVerdictWouldBlock
	}
	if res.Action != "" {
		return model.GuardVerdictFlagged
	}
	return model.GuardVerdictPass
}

func mergeVerdict(tr *model.GuardTrace, v string) {
	if v == model.GuardVerdictError {
		if tr.Verdict == model.GuardVerdictPending {
			tr.Verdict = v
		}
		return
	}
	if verdictRank[v] > verdictRank[tr.Verdict] {
		tr.Verdict = v
	}
}

// TraceRecorder 维护 guard_traces 元数据（状态/方向完成/裁决汇总）并联动
// 内存配对存储。方向完成与裁决汇总是 last-write-wins（元数据仅观测用，
// 事件本身在 guard_events 有独立留存）。
//
// mu 把读-改-写串行化：请求/响应两路持久化在检测池并发执行，无锁时
// 后写者的全行 Save 会把先写者刚落的方向时间戳覆盖回 NULL（丢更新，
// trace 永久 open）。节点间仍为 last-write-wins（同 trace 跨节点并发
// 属罕见窗口，元数据观测容错）。
type TraceRecorder struct {
	mu    sync.Mutex
	repo  *repository.GuardTraceRepo
	store *TraceStore
}

func NewTraceRecorder(repo *repository.GuardTraceRepo, store *TraceStore) *TraceRecorder {
	return &TraceRecorder{repo: repo, store: store}
}

// TakePrompt 取回请求向配对 prompt（take-and-delete）——管线在响应向检测
// 前调用，作为 CheckRequest.Prompt 进入上下文引擎（网关 stream_wrapper
// 同语义）。
func (r *TraceRecorder) TakePrompt(orgID int64, traceID string) (string, bool) {
	return r.store.TakePrompt(orgID, traceID)
}

// Status 读取 trace 当前元数据（API 响应附录用；落库异步，可能滞后）。
func (r *TraceRecorder) Status(ctx context.Context, orgID int64, traceID string) (*model.GuardTrace, error) {
	return r.repo.FindByOrgTrace(ctx, orgID, traceID)
}

// RecordRequest 请求向检测完成：存 prompt 供响应向配对 + 落 trace 元数据。
func (r *TraceRecorder) RecordRequest(ctx context.Context, orgID int64, traceID, prompt, verdict string) error {
	r.store.SavePrompt(orgID, traceID, prompt)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.apply(ctx, orgID, traceID, func(tr *model.GuardTrace) {
		now := time.Now()
		tr.RequestAt = &now
		mergeVerdict(tr, verdict)
		if tr.ResponseAt != nil {
			tr.Status = model.GuardTraceComplete
		}
	})
}

// RecordResponse 响应向检测完成：取回配对 prompt（供 CheckRequest.Prompt，
// 调用方先取再检）+ 落 trace 元数据。
func (r *TraceRecorder) RecordResponse(ctx context.Context, orgID int64, traceID, verdict string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.apply(ctx, orgID, traceID, func(tr *model.GuardTrace) {
		now := time.Now()
		tr.ResponseAt = &now
		mergeVerdict(tr, verdict)
		if tr.RequestAt != nil {
			tr.Status = model.GuardTraceComplete
		}
	})
}

// apply 读-改-写 upsert；并发首建撞唯一键时重读一次再改。
func (r *TraceRecorder) apply(ctx context.Context, orgID int64, traceID string, mutate func(*model.GuardTrace)) error {
	tr, err := r.repo.FindByOrgTrace(ctx, orgID, traceID)
	if err != nil {
		tr = &model.GuardTrace{TraceID: traceID, OrgID: orgID, Status: model.GuardTraceOpen, Verdict: model.GuardVerdictPending}
		if err := r.repo.Create(ctx, tr); err != nil {
			// 并发首建：另一路已插入同 (org, trace) 行，重读继续。
			tr2, rerr := r.repo.FindByOrgTrace(ctx, orgID, traceID)
			if rerr != nil {
				return err
			}
			tr = tr2
		}
	}
	mutate(tr)
	return r.repo.Save(ctx, tr)
}
