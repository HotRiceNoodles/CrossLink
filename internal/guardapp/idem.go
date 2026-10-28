package guardapp

import (
	"container/list"
	"fmt"
	"sync"
	"time"
)

// idemStore 是 ingest 幂等去重存储：键 (org_id, trace_id, direction, seq)。
// 重复回放（客户超时重试、镜像端点重复投递）不得产生重复事件/trace——
// 设计 DD「幂等：(trace_id, direction, seq) 去重」。
//
// M1 为节点内 TTL+LRU（与 TraceStore 同模式）：镜像流量通常回放同一节点，
// 跨节点重复属罕见窗口。Redis 后端（跨节点一致）留 M2。
type idemStore struct {
	mu    sync.Mutex
	lru   *list.List // front=最近；element.Value=*idemItem
	items map[string]*list.Element
	ttl   time.Duration
	cap   int
	now   func() time.Time
}

type idemItem struct {
	key       string
	expiresAt time.Time
}

func newIdemStore(ttl time.Duration, cap int) *idemStore {
	return &idemStore{
		lru:   list.New(),
		items: make(map[string]*list.Element),
		ttl:   ttl,
		cap:   cap,
		now:   time.Now,
	}
}

// idemKey 幂等键。seq 缺省 0——非流式 M1 客户通常不带 seq，同 trace 同向
// 二次提交即重复。
func idemKey(orgID int64, traceID, direction string, seq int64) string {
	return fmt.Sprintf("%d|%s|%s|%d", orgID, traceID, direction, seq)
}

// Seen 记录并报告是否首次出现（首次=true 并留痕；重复=false）。
func (s *idemStore) Seen(orgID int64, traceID, direction string, seq int64) bool {
	key := idemKey(orgID, traceID, direction, seq)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		it := el.Value.(*idemItem)
		if now.After(it.expiresAt) {
			// 过期条目视为未见：刷新留痕
			it.expiresAt = now.Add(s.ttl)
			s.lru.MoveToFront(el)
			return true
		}
		return false
	}
	s.items[key] = s.lru.PushFront(&idemItem{key: key, expiresAt: now.Add(s.ttl)})
	for s.lru.Len() > s.cap {
		back := s.lru.Back()
		if back == nil {
			break
		}
		s.lru.Remove(back)
		delete(s.items, back.Value.(*idemItem).key)
	}
	return true
}
