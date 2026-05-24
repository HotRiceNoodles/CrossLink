package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"gorm.io/datatypes"
)

const auditChannelSize = 1024
const auditBatchSize = 50
const auditFlushInterval = 2 * time.Second

type AuditService struct {
	repo    *repository.AuditLogRepo
	ch      chan *model.AuditLog
	done    chan struct{}
	retryWg sync.WaitGroup
}

func NewAuditService(repo *repository.AuditLogRepo, ctx context.Context) *AuditService {
	s := &AuditService{
		repo: repo,
		ch:   make(chan *model.AuditLog, auditChannelSize),
		done: make(chan struct{}),
	}
	go s.run(ctx)
	return s
}

func (s *AuditService) Wait() {
	<-s.done
}

var auditEventsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "cl_audit_events_total",
}, []string{"action", "resource_type"})

var auditDroppedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "cl_audit_events_dropped_total",
}, []string{"action", "resource_type"})

func (s *AuditService) Log(entry *model.AuditLog) {
	select {
	case s.ch <- entry:
		auditEventsTotal.WithLabelValues(entry.Action, entry.ResourceType).Inc()
	default:
		auditDroppedTotal.WithLabelValues(entry.Action, entry.ResourceType).Inc()
		slog.Warn("audit log channel full, dropping entry", "action", entry.Action, "resource_type", entry.ResourceType)
	}
}

func (s *AuditService) logFromContext(c *gin.Context, action, resourceType, resourceID, resourceName string, detail datatypes.JSON, status string) {
	userID, _ := c.Get("user_id")
	username, _ := c.Get("username")
	entry := &model.AuditLog{
		UserID:       auditInt64(userID),
		Username:     toString(username),
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		ResourceName: resourceName,
		Detail:       detail,
		IPAddress:    c.ClientIP(),
		UserAgent:    c.Request.UserAgent(),
		Status:       status,
		CreatedAt:    time.Now().UTC(),
	}
	s.Log(entry)
}

func (s *AuditService) LogFromContext(c *gin.Context, action, resourceType, resourceID, resourceName string, detail datatypes.JSON) {
	s.logFromContext(c, action, resourceType, resourceID, resourceName, detail, "success")
}

func (s *AuditService) LogFailure(c *gin.Context, action, resourceType, resourceID, resourceName string, detail datatypes.JSON) {
	s.logFromContext(c, action, resourceType, resourceID, resourceName, detail, "failure")
}

func AuditDetail(v any) datatypes.JSON {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return datatypes.JSON(b)
}

func (s *AuditService) run(ctx context.Context) {
	batch := make([]*model.AuditLog, 0, auditBatchSize)
	ticker := time.NewTicker(auditFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case entry := <-s.ch:
			batch = append(batch, entry)
			if len(batch) >= auditBatchSize {
				s.flush(batch, ctx)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				s.flush(batch, ctx)
				batch = batch[:0]
			}
		case <-ctx.Done():
			for len(s.ch) > 0 {
				batch = append(batch, <-s.ch)
				if len(batch) >= auditBatchSize {
					s.flush(batch, ctx)
					batch = batch[:0]
				}
			}
			if len(batch) > 0 {
				s.flush(batch, ctx)
			}
			s.retryWg.Wait()
			close(s.done)
			return
		}
	}
}

func (s *AuditService) flush(batch []*model.AuditLog, lifecycle context.Context) {
	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.repo.CreateBatch(flushCtx, batch); err != nil {
		retry := make([]*model.AuditLog, len(batch))
		copy(retry, batch)
		s.retryWg.Add(1)
		go func() {
			defer s.retryWg.Done()
			select {
			case <-time.After(time.Second):
			case <-lifecycle.Done():
				for _, e := range retry {
					auditDroppedTotal.WithLabelValues(e.Action, e.ResourceType).Inc()
				}
				slog.Warn("audit retry cancelled on shutdown, dropping entries", "count", len(retry))
				return
			}
			retryCtx, retryCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer retryCancel()
			if err := s.repo.CreateBatch(retryCtx, retry); err != nil {
				slog.Error("audit flush retry failed", "error", err, "count", len(retry))
				for _, e := range retry {
					auditDroppedTotal.WithLabelValues(e.Action, e.ResourceType).Inc()
					slog.Error("dropped audit entry", "action", e.Action, "resource_type", e.ResourceType, "resource_id", e.ResourceID, "user_id", e.UserID)
				}
			}
		}()
	}
}

func auditInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}
