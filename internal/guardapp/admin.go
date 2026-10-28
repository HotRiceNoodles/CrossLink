package guardapp

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/crosslink/internal/admin"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// 最小 Admin API（M1 Step 8）：JWT 登录、护栏规则 CRUD（复用网关
// GuardrailHandler，审计传 nil——其对 nil 全容忍）、ingest 凭证 CRUD、
// 事件查询（org 过滤强制）与基础统计。RBAC 复用 RequireAction + 动作位：
// 规则走 guardrail:*（Community 档既有位），guard 独有位 guard:token_manage /
// guard:event_view。
//
// 复用边界（sqlite 约束，见 bootstrap.go）：JWTAuthMiddleware 的请求内
// 查询全部 Select 指定列，可直接复用；网关 LoginHandler/ChangeForcedPassword
// 依赖 UserRepo 整行扫描（users 时间列 TEXT），guard 自带 Select 限定版本。
func (a *App) registerAdminRoutes(r *gin.Engine) {
	r.POST("/admin/api/auth/login", a.handleAdminLogin)

	authed := r.Group("/admin/api", admin.JWTAuthMiddleware(a.Cfg.Admin, a.DB, a.CP))
	// 强制改密放行路径与网关 JWTAuthMiddleware 白名单一致（auth.go）：
	// 未改密的种子管理员只能触达自服务端点。
	authed.POST("/auth/change-forced-password", a.handleChangeForcedPassword)
	authed.GET("/auth/permissions", a.handlePermissions)

	gr := admin.NewGuardrailHandler(a.DB, a.Guardrail, nil)
	authed.GET("/guardrails", middleware.RequireAction(a.PermCache, "guardrail:list"), gr.List)
	authed.POST("/guardrails", middleware.RequireAction(a.PermCache, "guardrail:create"), gr.Create)
	authed.PUT("/guardrails/:id", middleware.RequireAction(a.PermCache, "guardrail:update"), gr.Update)
	authed.DELETE("/guardrails/:id", middleware.RequireAction(a.PermCache, "guardrail:delete"), gr.Delete)
	authed.POST("/guardrails/:id/test", middleware.RequireAction(a.PermCache, "guardrail:test"), gr.Test)

	tok := middleware.RequireAction(a.PermCache, "guard:token_manage")
	authed.GET("/guard/tokens", tok, a.handleTokenList)
	authed.POST("/guard/tokens", tok, a.handleTokenCreate)
	authed.POST("/guard/tokens/:id/revoke", tok, a.handleTokenRevoke)
	authed.POST("/guard/tokens/:id/reset", tok, a.handleTokenReset)

	view := middleware.RequireAction(a.PermCache, "guard:event_view")
	authed.GET("/guard/events", view, a.handleEventList)
	authed.GET("/guard/stats", view, a.handleStats)
}

// guardUserRow 是登录/改密所需的最小用户投影（Select 指定列，sqlite 安全）。
type guardUserRow struct {
	ID                  int64
	Username            string
	PasswordHash        string
	RoleID              int64
	Status              int16
	ForcePasswordChange bool
	OrgID               *int64
}

func fetchGuardUser(db *gorm.DB, query string, arg any) *guardUserRow {
	var row guardUserRow
	err := db.Model(&model.User{}).
		Select("id", "username", "password_hash", "role_id", "status", "force_password_change", "org_id").
		Where(query, arg).
		Scan(&row).Error
	if err != nil || row.ID == 0 {
		return nil
	}
	return &row
}

// guardRoleName 按角色 ID 取名（roles 时间列 TEXT，只 Pluck name）。
func guardRoleName(db *gorm.DB, roleID int64) string {
	var names []string
	db.Model(&model.Role{}).Where("id = ?", roleID).Pluck("name", &names)
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// dummyHash 用于未知用户的恒时比较：登录失败不区分"用户不存在"与"密码错"。
var dummyHash = func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("timing-equalizer"), bcryptCost)
	return h
}()

// loginThrottle 进程内登录防爆破：窗口内连续失败 N 次锁 username。无 Redis
// 依赖（guard 可无 Redis 运行）；多节点各锁各的，爆破成本线性放大。
type loginThrottle struct {
	mu     sync.Mutex
	fails  map[string][]time.Time
	limit  int
	window time.Duration
}

var logins = &loginThrottle{fails: make(map[string][]time.Time), limit: 5, window: time.Minute}

func (t *loginThrottle) locked(username string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.recentLocked(username)) >= t.limit
}

// recentLocked 返回窗口内失败时刻（调用方持锁）。
func (t *loginThrottle) recentLocked(username string) []time.Time {
	now := time.Now()
	var recent []time.Time
	for _, ts := range t.fails[username] {
		if now.Sub(ts) < t.window {
			recent = append(recent, ts)
		}
	}
	t.fails[username] = recent
	return recent
}

func (t *loginThrottle) recordFailure(username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fails[username] = append(t.recentLocked(username), time.Now())
	// 上限防 username 喷射撑爆内存：超限整体丢弃（锁保护重置）。
	if len(t.fails) > 10000 {
		t.fails = make(map[string][]time.Time)
	}
}

func (t *loginThrottle) reset(username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fails, username)
}

// handleAdminLogin 用户名+密码换 JWT。种子管理员带 force_password_change，
// 登录成功但管理面被 JWT 中间件拦在改密端点（响应带标志供控制台引导）。
func (a *App) handleAdminLogin(c *gin.Context) {
	var input struct {
		Username string `json:"username" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_request", "message": err.Error()}})
		return
	}
	if logins.locked(input.Username) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"code": "too_many_attempts",
			"message": "too many failed logins, retry in a minute"}})
		return
	}

	u := fetchGuardUser(a.DB, "username = ? AND deleted_at IS NULL", input.Username)
	if u == nil {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(input.Password))
		logins.recordFailure(input.Username)
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "invalid_credentials", "message": "invalid username or password"}})
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(input.Password)); err != nil {
		logins.recordFailure(input.Username)
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "invalid_credentials", "message": "invalid username or password"}})
		return
	}
	if u.Status == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "account_disabled", "message": "account is disabled"}})
		return
	}
	logins.reset(input.Username)

	orgID := int64(0)
	if u.OrgID != nil {
		orgID = *u.OrgID
	}
	token, err := admin.GenerateToken(&model.User{ID: u.ID, Username: u.Username, RoleID: u.RoleID},
		guardRoleName(a.DB, u.RoleID), 0, orgID, "", a.Cfg.Admin, a.CP)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "token_failed", "message": err.Error()}})
		return
	}
	a.setAdminCookie(c, token)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"token": token, "expires_in": a.Cfg.Admin.TokenExpiry * 3600,
		"username": u.Username, "role_name": guardRoleName(a.DB, u.RoleID), "org_id": orgID,
		"force_password_change": u.ForcePasswordChange,
	}})
}

// handleChangeForcedPassword 首登强制改密。与网关版差异：要求当前密码——
// 种子密码来自部署配置，持有 JWT 不等于持有口令，改密必须再验一次。
func (a *App) handleChangeForcedPassword(c *gin.Context) {
	var input struct {
		CurrentPassword string `json:"current_password" binding:"required"`
		NewPassword     string `json:"new_password" binding:"required"`
		ConfirmPassword string `json:"confirm_password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_request", "message": err.Error()}})
		return
	}
	userID := admin.GetUserID(c)
	u := fetchGuardUser(a.DB, "id = ?", userID)
	if u == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "user_not_found", "message": "user not found"}})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(input.CurrentPassword)) != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "invalid_current_password", "message": "current password is incorrect"}})
		return
	}
	if input.NewPassword != input.ConfirmPassword {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_request", "message": "passwords do not match"}})
		return
	}
	if len(input.NewPassword) < 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_request", "message": "password must be at least 8 characters"}})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(input.NewPassword), bcryptCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "hash_failed", "message": "failed to hash password"}})
		return
	}
	// Updates 只触碰给定列（+updated_at），不整行回写。
	if err := a.DB.Model(&model.User{}).Where("id = ?", userID).
		Updates(map[string]any{"password_hash": string(hash), "force_password_change": false}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "update_failed", "message": "failed to update password"}})
		return
	}

	orgID := int64(0)
	if u.OrgID != nil {
		orgID = *u.OrgID
	}
	roleName := guardRoleName(a.DB, u.RoleID)
	token, err := admin.GenerateToken(&model.User{ID: u.ID, Username: u.Username, RoleID: u.RoleID},
		roleName, 0, orgID, "", a.Cfg.Admin, a.CP)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "token_failed", "message": err.Error()}})
		return
	}
	a.setAdminCookie(c, token)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"token": token, "expires_in": a.Cfg.Admin.TokenExpiry * 3600,
		"username": u.Username, "role_name": roleName, "org_id": orgID,
	}})
}

// handlePermissions 返回当前角色动作位（强制改密期可用——网关白名单同款）。
func (a *App) handlePermissions(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"permissions": a.PermCache.GetPermissions(c.GetInt64("role_id"))}})
}

// requireOrg 管理数据面的强制租户上下文：org=0（无 org 成员关系的非管理员）
// 一律 403——多租户共享部署下不存在无租户的数据访问。
func (a *App) requireOrg(c *gin.Context) (int64, bool) {
	orgID := admin.GetOrgID(c)
	if orgID == 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "no_org_context",
			"message": "user has no organization context"}})
		return 0, false
	}
	return orgID, true
}

// tokenView 是凭证列表投影：哈希永不回显（明文仅创建/重置响应出现一次）。
type tokenView struct {
	ID         int64      `json:"id"`
	OrgID      int64      `json:"org_id"`
	Name       string     `json:"name"`
	KeyPrefix  string     `json:"key_prefix"`
	HashAlgo   string     `json:"hash_algo"`
	Status     int16      `json:"status"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

func (a *App) handleTokenList(c *gin.Context) {
	orgID, ok := a.requireOrg(c)
	if !ok {
		return
	}
	toks, err := a.TokenRepo.ListByOrg(c.Request.Context(), orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "list_failed", "message": err.Error()}})
		return
	}
	out := make([]tokenView, len(toks))
	for i := range toks {
		out[i] = tokenView{
			ID: toks[i].ID, OrgID: toks[i].OrgID, Name: toks[i].Name,
			KeyPrefix: toks[i].KeyPrefix, HashAlgo: toks[i].HashAlgo, Status: toks[i].Status,
			LastUsedAt: toks[i].LastUsedAt, RevokedAt: toks[i].RevokedAt, CreatedAt: toks[i].CreatedAt,
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (a *App) handleTokenCreate(c *gin.Context) {
	orgID, ok := a.requireOrg(c)
	if !ok {
		return
	}
	var input struct {
		Name string `json:"name"`
	}
	_ = c.ShouldBindJSON(&input) // name 可选，空体也接受
	ct, err := a.Tokens.Create(c.Request.Context(), orgID, input.Name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "create_failed", "message": err.Error()}})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": ct})
}

func (a *App) handleTokenRevoke(c *gin.Context) {
	orgID, ok := a.requireOrg(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_id", "message": "invalid id"}})
		return
	}
	if err := a.Tokens.Revoke(c.Request.Context(), orgID, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "revoke_failed", "message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "revoked"})
}

func (a *App) handleTokenReset(c *gin.Context) {
	orgID, ok := a.requireOrg(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_id", "message": "invalid id"}})
		return
	}
	ct, err := a.Tokens.Reset(c.Request.Context(), orgID, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "reset_failed", "message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": ct})
}

// handleEventList 事件查询：org 强制过滤；?trace_id= 走 trace 归因查询，
// 否则按时间倒序分页（limit≤200 默认 50）。
func (a *App) handleEventList(c *gin.Context) {
	orgID, ok := a.requireOrg(c)
	if !ok {
		return
	}
	events := repository.NewGuardEventRepo(a.DB)
	if traceID := c.Query("trace_id"); traceID != "" {
		rows, err := events.ListByTrace(c.Request.Context(), orgID, traceID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "list_failed", "message": err.Error()}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": rows})
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	rows, err := events.ListByOrg(c.Request.Context(), orgID, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "list_failed", "message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows})
}

// handleStats 基础统计：事件按动作、trace 按裁决汇总 + open 存量。
// 纯 SQL 聚合（不扫时间列到结构体），days 默认 7、上限 90。
func (a *App) handleStats(c *gin.Context) {
	orgID, ok := a.requireOrg(c)
	if !ok {
		return
	}
	days, _ := strconv.Atoi(c.Query("days"))
	if days <= 0 {
		days = 7
	}
	if days > 90 {
		days = 90
	}
	since := time.Now().AddDate(0, 0, -days)

	ctx := c.Request.Context()
	var byAction []struct {
		Action string `json:"action"`
		Count  int64  `json:"count"`
	}
	if err := a.DB.WithContext(ctx).Raw(
		`SELECT action, COUNT(*) AS count FROM guard_events WHERE org_id = ? AND created_at >= ? GROUP BY action`,
		orgID, since).Scan(&byAction).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "stats_failed", "message": err.Error()}})
		return
	}
	var byVerdict []struct {
		Verdict string `json:"verdict"`
		Count   int64  `json:"count"`
	}
	if err := a.DB.WithContext(ctx).Raw(
		`SELECT verdict, COUNT(*) AS count FROM guard_traces WHERE org_id = ? AND created_at >= ? GROUP BY verdict`,
		orgID, since).Scan(&byVerdict).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "stats_failed", "message": err.Error()}})
		return
	}
	var open int64
	a.DB.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM guard_traces WHERE org_id = ? AND status = ?`, orgID, model.GuardTraceOpen).Scan(&open)

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"window_days": days,
		"events":      gin.H{"by_action": byAction},
		"traces":      gin.H{"by_verdict": byVerdict, "open": open},
	}})
}

// setAdminCookie 与网关登录同款：HttpOnly cookie 供浏览器控制台，token 明文
// 也在 JSON 里返回供 Bearer 使用（JWT 中间件 cookie 优先、Bearer 兜底）。
func (a *App) setAdminCookie(c *gin.Context, token string) {
	c.SetCookie("admin_token", token, a.Cfg.Admin.TokenExpiry*3600, "/", "", true, true)
}
