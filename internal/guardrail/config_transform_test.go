package guardrail

// Config seam 验收(自适应调参 M3,规划 v1.7 D1/D2 + G0-5):
//   - 无注入 = identity:引擎收到原始 Config(行为零变化);
//   - 注入后:transform 在引擎构造时生效,行为可观察地变化;
//   - transform 错误 → 引擎构造失败(走既有 error 路径,medium 规则按
//     默认 skip 策略放行,不 panic);
//   - 缓存引擎不被 transform 变化打穿(下一次构造才生效——调用方用
//     InvalidateCache/reload 控制即时性)。

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newTransformTestService(t *testing.T, cfg string) *GuardrailService {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "ct.db")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB, _ := db.DB(); _ = sqlDB.Close() })
	require.NoError(t, db.Exec(`CREATE TABLE guardrail_rules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		org_id INTEGER,
		name TEXT NOT NULL,
		type TEXT NOT NULL,
		direction TEXT NOT NULL,
		enabled INTEGER DEFAULT 1,
		config TEXT NOT NULL,
		severity TEXT DEFAULT 'medium',
		action TEXT DEFAULT 'block',
		model_filter TEXT,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		version INTEGER NOT NULL DEFAULT 1
	)`).Error)
	require.NoError(t, db.Exec(`INSERT INTO guardrail_rules (name, type, direction, enabled, config, severity, action, version)
		VALUES ('len', 'content_length', 'both', 1, ?, 'medium', 'block', 1)`, cfg).Error)

	svc := NewGuardrailService(db, nil)
	svc.SetEnabled(true)
	require.NoError(t, svc.LoadRules(context.Background()))
	return svc
}

func TestConfigTransformIdentityByDefault(t *testing.T) {
	svc := newTransformTestService(t, `{"max_input_chars":5}`)

	// 无注入:6 字符超 max=5 被拦;3 字符放行(原始 Config 生效)。
	r, err := svc.Check(context.Background(), &CheckRequest{Content: "123456", Direction: DirectionRequest})
	require.NoError(t, err)
	require.NotNil(t, r)
	require.True(t, r.Blocked, "identity: 6 runes over max=5 must block")
	r2, err := svc.Check(context.Background(), &CheckRequest{Content: "123", Direction: DirectionRequest})
	require.NoError(t, err)
	require.False(t, r2.Blocked, "identity: 3 runes under max=5 must pass")
}

func TestConfigTransformAppliedOnNextEngineBuild(t *testing.T) {
	svc := newTransformTestService(t, `{"max_input_chars":5}`)

	// 先构造一次(缓存引擎,transform 尚未注入)。
	r, err := svc.Check(context.Background(), &CheckRequest{Content: "123456", Direction: DirectionRequest})
	require.NoError(t, err)
	require.True(t, r.Blocked)

	// 注入:把 max_input_chars 改成 50(白名单改写的等价模拟)。
	svc.SetConfigTransform(func(_ int64, ruleType string, cfg json.RawMessage) (json.RawMessage, error) {
		if ruleType != "content_length" {
			return cfg, nil
		}
		var m map[string]any
		if err := json.Unmarshal(cfg, &m); err != nil {
			return nil, err
		}
		m["max_input_chars"] = 50
		return json.Marshal(m)
	})
	// 缓存命中:行为不变(打穿缓存才算生效是调用方的 reload 责任)。
	r2, err := svc.Check(context.Background(), &CheckRequest{Content: "123456", Direction: DirectionRequest})
	require.NoError(t, err)
	require.True(t, r2.Blocked, "cached engine must serve the pre-transform config")

	// 失效缓存 → 下一次构造走 transform:6 字符在 max=50 下放行。
	svc.InvalidateCache()
	require.NoError(t, svc.LoadRules(context.Background()))
	r3, err := svc.Check(context.Background(), &CheckRequest{Content: "123456", Direction: DirectionRequest})
	require.NoError(t, err)
	require.False(t, r3.Blocked, "post-transform engine must use max=50 and pass 6 runes")
}

func TestConfigTransformErrorFailsEngineConstruction(t *testing.T) {
	svc := newTransformTestService(t, `{"max_input_chars":5}`)
	svc.SetConfigTransform(func(int64, string, json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("boom")
	})
	// transform 失败 → 引擎构造失败 → Check 走 error 路径:length 是
	// severity=medium 规则,默认 skip 策略放行(不 panic、不拦截)。
	r, err := svc.Check(context.Background(), &CheckRequest{Content: "123456", Direction: DirectionRequest})
	require.NoError(t, err, "medium-severity engine error must follow the skip policy")
	if r != nil && r.Blocked {
		t.Fatal("skip policy must not block")
	}
}
