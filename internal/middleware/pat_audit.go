package middleware

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
)

// AuditLogger is the consumer-side interface of service.AuditService.
type AuditLogger interface {
	Log(entry *model.AuditLog)
}

// PATAudit logs every PAT-authenticated request to the audit trail with
// via:"pat" so agent activity is distinguishable from human admin sessions.
// Per design 4.3: no request body and no query string — only method/path/
// status and PAT identity. Must be mounted after PATAuthMiddleware in the
// chain so pat_id is in the context when c.Next() returns.
//
// CAUTION: pass a bare nil (not a nil *service.AuditService) when the audit
// service is absent — a nil *T stuffed into an interface is not == nil and
// would panic on Log. Callers should pass nil explicitly (see app.go).
func PATAudit(auditSvc AuditLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		if auditSvc == nil {
			return
		}
		patID, _ := c.Get("pat_id")
		id, ok := patID.(int64)
		if !ok || id == 0 {
			// PATAuthMiddleware did not run — nothing to attribute.
			return
		}

		status := c.Writer.Status()
		auditStatus := "success"
		if status >= 400 {
			auditStatus = "failure"
		}
		userID, _ := c.Get("user_id")
		username, _ := c.Get("username")
		uid, _ := userID.(int64)

		auditSvc.Log(&model.AuditLog{
			UserID:   uid,
			Username: usernameToString(username),
			Action:   "pat:request",
			ResourceType: "pat",
			ResourceID:   strconv.FormatInt(id, 10),
			Detail: service.AuditDetail(map[string]any{
				"via":    "pat",
				"method": c.Request.Method,
				"path":   c.FullPath(),
				"status": status,
			}),
			IPAddress: c.ClientIP(),
			UserAgent: c.Request.UserAgent(),
			Status:    auditStatus,
			CreatedAt: time.Now().UTC(),
		})
	}
}

func usernameToString(v any) string {
	s, _ := v.(string)
	return s
}
