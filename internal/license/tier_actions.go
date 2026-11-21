package license

import "sort"

// communityActions defines the base set of actions available to the Community tier.
var communityActions = map[string]bool{
	"provider:list":   true,
	"provider:create": true,
	"provider:update": true,
	"provider:delete": true,
	"provider:test":   true,
	"model:list":      true,
	"model:create":    true,
	"model:update":    true,
	"model:delete":    true,
	"key:list":        true,
	"key:create":      true,
	"key:update":      true,
	"key:delete":      true,
	"key:regenerate":  true,
	"key:import":      true,
	"usage:list":      true,
	"usage:stats":     true,
	"routing:stats":   true,
	"system:password": true,
	"license:view":    true,
	"license:manage":  true,
	"mcp:list":        true,
	"mcp:view":        true,
	"mcp:create":      true,
	"mcp:update":      true,
	"mcp:delete":      true,
	"mcp:permission":  true,
	"mcp:logs":        true,
	"mcp:stats":       true,
	// System settings — community-core. Powers the Settings page (sidebar +
	// route guard use system:view) and the dedicated /system/content-log
	// endpoint. The aggregate /system/settings endpoint itself stays Pro
	// (commercial overlay).
	"system:view":   true,
	"system:update": true,
	// Prompt templates (context engineering) — community-core feature.
	"template:list":   true,
	"template:create": true,
	"template:update": true,
	"template:delete": true,
	// 护栏 CRUD 与容灾错误分类已上移至 proExtraActions（产品决策 2026-11-09：
	// 社区版整体隐藏「安全与治理」模块）。
	// crosslink-guard 管理动作位：guard 独立部署运行 Community 档，
	// 其管理 API 必须在 Community 档可用（网关路由不引用这些动作位）。
	"guard:token_manage": true,
	"guard:event_view":   true,
	// Scoped PAT support — community-core.
	"budget:read": true,
	"health:read": true,
	"pat:manage":  true,
	// Playground (interactive testing, all modes) — community-core.
	"playground:use": true,
}

// proExtraActions defines additional actions available to the Pro tier (on top of Community).
var proExtraActions = map[string]bool{
	"debug:list":   true,
	"debug:clear":  true,
	"key:rotate":   true,
	"key:hashes":   true,
	"usage:export": true,
	// Capability alias admin CRUD is Pro-gated end-to-end: the resolver, repo,
	// and admin handler all live in the commercial overlay, which is only built
	// into Pro/Enterprise. Runtime usage is additionally gated via the alias
	// resolver's RequirePro check.
	"capability:list":        true,
	"capability:create":      true,
	"capability:update":      true,
	"capability:delete":      true,
	"guardrail_alert:list":   true,
	"guardrail_alert:create": true,
	"guardrail_alert:update": true,
	"guardrail_alert:delete": true,
	"guardrail_alert:logs":   true,
	// 护栏 CRUD 与容灾错误分类 —— Pro 及以上（产品决策 2026-11-09：社区版整体
	// 隐藏「安全与治理」模块；护栏 CRUD 与 error_rule:* 原为 community-core，
	// 自该决策起上移至 Pro）。
	"guardrail:list":              true,
	"guardrail:create":            true,
	"guardrail:update":            true,
	"guardrail:delete":            true,
	"guardrail:test":              true,
	"error_rule:list":             true,
	"error_rule:create":           true,
	"error_rule:update":           true,
	"error_rule:delete":           true,
	"secret:test":                 true,
	"secret:manage":               true,
	"agent_shield:view":           true,
	"agent_shield:manage":         true,
	"fingerprint:list":            true,
	"fingerprint:view":            true,
	"fingerprint:manage":          true,
	"datalens:view":               true,
	"datalens:create":             true,
	"datalens:update":             true,
	"datalens:delete":             true,
	"datalens:export":             true,
	"datalens:schedule":           true,
	"datalens:manage_aggregation": true,
	// 审计日志（审计中心列表/统计/导出）—— Pro 及以上（产品决策 2026-11-12：
	// 自 Enterprise 下放，Pro 团队同样有操作留痕与导出取证诉求）。
	"audit:list":   true,
	"audit:export": true,
	// 内容合规（content_compliance 引擎、事件与人工复核）—— Pro 及以上。
	"compliance:policy": true,
	"compliance:check":  true,
	"compliance:logs":   true,
	"compliance:review": true,
	// Agent 工具集画像（按 key 观测 tools 声明与 tool 调用）—— Pro 及以上。
	// 采集器与查询端点均在商业 overlay（社区仅留 ToolObserver seam 与空表）；
	// 迁移侧已把该动作种子进 admin/member/viewer 角色。
	"toolset:list": true,
	// 工具集变化告警规则管理（M2，端点在商业 overlay）—— Pro 及以上。
	"toolset_alert:list":   true,
	"toolset_alert:create": true,
	"toolset_alert:update": true,
	"toolset_alert:delete": true,
	// RAG 知识库投毒防御台账（M4，端点在商业 overlay）—— Pro 及以上。
	// role.go 已注册动作位；审批/重扫等 Enterprise 专属端点由路由闭包
	// RequireEnterprise 再加一层门（与 fingerprint 同模式：动作位定档，
	// 路由闭包收紧）。
	"rag:view":   true,
	"rag:manage": true,
	// 实体行为基线防护台账（M2，端点在社区 app.go，冻结 v4.0 D4：台账 CRUD
	// 属 Community 机制）—— Pro 及以上；observe-only，不含处置动作（D2）。
	"behavior:view":   true,
	"behavior:manage": true,
	// 护栏误报仲裁（自适应调参 M1，端点在商业 overlay，规划 v1.7 D16）——
	// Pro 及以上。窄于规则编辑：可读告警详情 + 标裁决（false_positive/
	// confirmed），不可改规则/乘子/确认候选；与 guardrail:tuning_confirm
	// 职责分离（标误报的人不确认放宽）。
	"guardrail:arbitrate": true,
	// 调参候选人工确认(自适应调参 M2,D16 v1.4 拆分):确认 awaiting_confirm
	// 候选应用 / 人工废弃;不可标仲裁、不可改规则——与仲裁职责分离,防单人
	// 闭环(仲裁员制造信号→自己确认放宽)。
	"guardrail:tuning_confirm": true,
}

// enterpriseExtraActions defines additional actions available to the Enterprise tier (on top of Pro).
var enterpriseExtraActions = map[string]bool{
	"team:list":                   true,
	"team:create":                 true,
	"team:update":                 true,
	"team:delete":                 true,
	"team:manage_members":         true,
	"user:list":                   true,
	"user:create":                 true,
	"user:update":                 true,
	"user:delete":                 true,
	"budget:manage":               true,
	"insight:manage":              true,
	"provider_guard_alert:list":   true,
	"provider_guard_alert:create": true,
	"provider_guard_alert:update": true,
	"provider_guard_alert:delete": true,
	"org:list":                    true,
	"org:create":                  true,
	"org:update":                  true,
	"org:delete":                  true,
	"org:manage_members":          true,
	"org:view_billing":            true,
	"org:manage_billing":          true,
	"role:list":                   true,
	"role:create":                 true,
	"role:update":                 true,
	"role:delete":                 true,
}

// TierActionSet maps each tier name to its fully enumerated set of allowed actions.
var TierActionSet map[string]map[string]bool

func init() {
	// Community: just the base set.
	community := make(map[string]bool, len(communityActions))
	for k := range communityActions {
		community[k] = true
	}
	TierActionSet = map[string]map[string]bool{
		TierCommunity: community,
	}

	// Pro: Community + pro extras.
	pro := make(map[string]bool, len(communityActions)+len(proExtraActions))
	for k := range communityActions {
		pro[k] = true
	}
	for k := range proExtraActions {
		pro[k] = true
	}
	TierActionSet[TierPro] = pro

	// Enterprise: Pro's full set + enterprise extras.
	enterprise := make(map[string]bool, len(pro)+len(enterpriseExtraActions))
	for k := range pro {
		enterprise[k] = true
	}
	for k := range enterpriseExtraActions {
		enterprise[k] = true
	}
	TierActionSet[TierEnterprise] = enterprise
}

// TierAllowsAction checks whether the current tier permits the given action.
func TierAllowsAction(action string) bool {
	actions, ok := TierActionSet[G().CurrentTier()]
	if !ok {
		return false
	}
	return actions[action]
}

// EffectiveActions returns the intersection of dbActions and the current tier's
// allowed actions, sorted alphabetically.
func EffectiveActions(dbActions []string) []string {
	tierActions := TierActionSet[G().CurrentTier()]
	var result []string
	for _, a := range dbActions {
		if tierActions[a] {
			result = append(result, a)
		}
	}
	sort.Strings(result)
	return result
}

// TierAllowedActions returns all actions allowed for the current tier, sorted alphabetically.
func TierAllowedActions() []string {
	tierActions := TierActionSet[G().CurrentTier()]
	result := make([]string, 0, len(tierActions))
	for a := range tierActions {
		result = append(result, a)
	}
	sort.Strings(result)
	return result
}
