package guardapp

import (
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/license"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Hooks 是商业版 overlay 的注入缝（镜像 app.go 的 Extensions 模式）：
// Community 构建（cmd/guard 原生入口）不设置任何 hook，行为与 M1 完全
// 一致；商业构建的 overlay cmd/guard/main.go 在 Run 之前 SetHooks——
// 同包合并机制下 overlay 文件与本包编译为一体（设计 DD「guardapp 商业
// 接线：overlay 新文件挂入 internal/guardapp」）。
//
// 全部字段可独立缺省：LicenseInit 缺省走 CommunityInit（NoopGate 全通过）；
// RegisterRoutes/PostSetup 缺省为不执行。
type Hooks struct {
	// LicenseInit 创建授权门（商业版传 license.Init——激活/心跳/JWT 续期）。
	LicenseInit func(cfg config.LicenseConfig, db *gorm.DB, cp crypto.CryptoProvider) *license.Gate

	// RegisterRoutes 在路由装配末段挂商业管理面（orgs/users/roles CRUD、
	// 告警规则与 incident 复核等）。收整棵 engine：商业路由自带
	// /admin/api 组与 JWT 中间件（与 registerAdminRoutes 同构）。
	RegisterRoutes func(a *App, r *gin.Engine)

	// PostSetup 在 App 全量装配后、监听开始前执行：overlay 引擎种子
	// （SeedCommercialDefaultRules 等）与 GuardrailService.AddHandler 事件
	// 接线（告警/incident——emitEvent 在 Check 内派发，Community 构建零
	// handler 即 no-op，见 pipeline.go 事件落库说明）。
	PostSetup func(a *App)
}

// hooks 由商业入口 SetHooks 设置；Run 装配期读取。包级而非参数传递：
// Run 的调用契约（migrateOnly, configDir）保持不变，Community 入口零改动。
var hooks Hooks

// SetHooks 注册商业构建注入缝；必须在 Run 之前调用（Run 读取后不再变更）。
func SetHooks(h Hooks) { hooks = h }
