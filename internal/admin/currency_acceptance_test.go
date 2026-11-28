package admin

// 单币种结算改造的端到端验收：跑真实生产迁移链建库，然后沿着部署生命周期
// 依次验证——
//   1. schema：provider_models/预算/datalens 表无币种列；usage_logs 保留行级
//      血缘列；system_settings 存在（billing_currency 的家）。
//   2. 首次 Commit 必须显式选币种（400），选定后行落库。
//   3. 用量管道写入的行币种 == 平台币种（settings.PlatformCurrency）。
//   4. 统计端点回显平台币种、直接求和、不再有 cost_by_currency。
//   5. 锁定后：onboarding 换币种 409；PUT /system/config 换值 409 / 同值幂等 /
//      null 拒绝。
//
// 不用 t.Parallel：测试中途会翻转进程级 PlatformCurrency，串行 + Cleanup 恢复。

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/dialect"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
	"github.com/crosslink/internal/settings"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCurrencyAcceptance_FullLifecycle(t *testing.T) {
	projectRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	t.Chdir(projectRoot)

	d := dialect.NewSQLiteDialect(dialect.DBConfig{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "currency-acceptance.db")})
	db, err := d.InitDB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Shutdown(db)) })
	require.NoError(t, d.RunMigrations(context.Background()))

	// 恢复进程级默认，避免污染同包其它用例。
	t.Cleanup(func() { settings.SetPlatformCurrency(settings.CurrencyCNY) })
	settings.SetPlatformCurrency(settings.CurrencyCNY)

	// ---- 1. schema 形状 ----
	for _, tbl := range []string{"provider_models", "budget_snapshots", "budget_recommendations", "datalens_hourly_metrics", "datalens_daily_metrics"} {
		assert.False(t, db.Migrator().HasColumn(tbl, "currency"),
			"%s must NOT carry a currency column (single-currency design)", tbl)
	}
	assert.True(t, db.Migrator().HasColumn("usage_logs", "currency"),
		"usage_logs keeps its currency column as row-level lineage")
	assert.True(t, db.Migrator().HasTable("system_settings"),
		"system_settings must exist (home of billing_currency)")

	// ---- 2. 首次 Commit 必须选币种 ----
	h := newOnboardingHandlerForTest(db)
	body := func(currency string) gin.H {
		payload := gin.H{
			"provider": gin.H{"name": "acc-provider", "display_name": "Acc", "adapter_type": "openai_compatible",
				"base_url": "https://8.8.8.8/v1", "api_key": "test-only"},
			"models": []gin.H{{"model_name": "acc-model", "provider_model": "upstream"}},
			"key":    gin.H{"name": "acc-key"},
		}
		if currency != "" {
			payload["currency"] = currency
		}
		return payload
	}

	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", body(""))
	setAdminContext(c, 0, 0, "admin")
	h.Commit(c)
	require.Equal(t, http.StatusBadRequest, w.Code, "first commit without currency must be rejected: %s", w.Body.String())

	c, w = newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", body("CNY"))
	setAdminContext(c, 0, 0, "admin")
	h.Commit(c)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var created struct {
		Data struct {
			BillingCurrency string `json:"billing_currency"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	assert.Equal(t, "CNY", created.Data.BillingCurrency)

	var row model.SystemSetting
	require.NoError(t, db.Where("system_settings.key = ?", settings.KeyBillingCurrency).First(&row).Error)
	assert.Equal(t, "CNY", row.Value)

	// settings 快照从真实库解析出已锁定的币种。
	sp := settings.NewProvider(db, &config.Config{}, nil)
	assert.Equal(t, "CNY", sp.Get().BillingCurrency)
	assert.Equal(t, settings.SourceDB, sp.Get().Source(settings.KeyBillingCurrency))

	// ---- 3. 用量管道写入平台币种 ----
	usageSvc := service.NewUsageService(repository.NewUsageLogRepo(db))
	usageSvc.Log(context.Background(), &service.UsageEntry{
		RouteType: "openai", ModelRequested: "acc-model", ModelUsed: "upstream",
		InputTokens: 1000, OutputTokens: 500, InputPrice: 0.001, OutputPrice: 0.002,
	})
	// 平台翻到 USD 后，新行的币种跟着平台走（写侧永远读全局）。
	settings.SetPlatformCurrency("USD")
	usageSvc.Log(context.Background(), &service.UsageEntry{
		RouteType: "openai", ModelRequested: "acc-model", ModelUsed: "upstream",
		InputTokens: 1000, OutputTokens: 500, InputPrice: 0.001, OutputPrice: 0.002,
	})
	var rows []model.UsageLog
	require.NoError(t, db.Order("id").Find(&rows).Error)
	require.Len(t, rows, 2)
	assert.Equal(t, "CNY", rows[0].Currency, "row currency follows the platform at write time")
	assert.Equal(t, "USD", rows[1].Currency)
	settings.SetPlatformCurrency(settings.CurrencyCNY)

	// ---- 4. 统计端点：平台币种回显 + 直接求和 + 无 cost_by_currency ----
	uh := NewUsageHandler(db, "", nil)
	c, w = newTestContext(t, http.MethodGet, "/admin/api/usage/stats", nil)
	c.Request = c.Request.WithContext(context.Background())
	setAdminContext(c, 0, 0, "admin")
	uh.Stats(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var statsResp struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &statsResp))
	assert.Equal(t, "CNY", statsResp.Data["currency"])
	assert.InDelta(t, 0.004, statsResp.Data["total_cost"].(float64), 1e-9,
		"single-currency sums ALL rows directly (0.002 per request × 2)")
	_, hasBreakdown := statsResp.Data["cost_by_currency"]
	assert.False(t, hasBreakdown, "cost_by_currency is gone under single-currency")

	// ---- 5. 锁定后的三条拒绝路径 ----
	// onboarding 换币种 → 409。
	c, w = newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", func() gin.H {
		b := body("USD")
		b["provider"].(gin.H)["name"] = "acc-provider-2"
		return b
	}())
	setAdminContext(c, 0, 0, "admin")
	h.Commit(c)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), ErrBillingCurrencyLocked)

	// PUT /system/config：换值 409、同值幂等 200、null 拒绝 400。
	sh := NewSystemHandler(db, nil, config.AdminConfig{}, nil, nil, nil, nil, nil)
	sh.SetSettingsProvider(sp)

	c, w = newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{"billing_currency": "USD"})
	sh.UpdateConfig(c)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())

	c, w = newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{"billing_currency": "CNY"})
	sh.UpdateConfig(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	c, w = newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{"billing_currency": nil})
	sh.UpdateConfig(c)
	require.Equal(t, http.StatusBadRequest, w.Code, "null (delete) on a write-once key must be refused: %s", w.Body.String())

	// 行未被动过。
	require.NoError(t, db.Where("system_settings.key = ?", settings.KeyBillingCurrency).First(&row).Error)
	assert.Equal(t, "CNY", row.Value)

	// GET /system/config 暴露 write_once 元数据（UI 只读渲染的依据）。
	c, w = newTestContext(t, http.MethodGet, "/admin/api/system/config", nil)
	sh.GetConfig(c)
	require.Equal(t, http.StatusOK, w.Code)
	var cfgResp struct {
		Data struct {
			Settings map[string]any              `json:"settings"`
			Meta     map[string]settings.APIMeta `json:"meta"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &cfgResp))
	assert.Equal(t, "CNY", cfgResp.Data.Settings["billing_currency"])
	assert.True(t, cfgResp.Data.Meta["billing_currency"].WriteOnce)
	assert.Equal(t, settings.SourceDB, cfgResp.Data.Meta["billing_currency"].Source)

	_ = time.Now // keep time import if assertions above change
}
