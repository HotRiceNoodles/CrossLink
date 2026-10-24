package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crosslink/internal/dialect"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 使用生产迁移器建表，防止 AutoMigrate 自动补列掩盖接入向导的迁移遗漏。
func TestOnboarding_Commit_SQLiteMigrationSchema(t *testing.T) {
	projectRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	for _, upgrade := range []bool{false, true} {
		name := "fresh"
		if upgrade {
			name = "upgrade"
		}
		t.Run(name, func(t *testing.T) {
			t.Chdir(projectRoot)
			d := dialect.NewSQLiteDialect(dialect.DBConfig{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "onboarding.db")})
			db, err := d.InitDB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, d.Shutdown(db)) })
			ctx := context.Background()

			if upgrade {
				// 模拟已部署到 000008 的旧库，并保留一条已有密钥。
				oldRoot := t.TempDir()
				oldDir := filepath.Join(oldRoot, "migrations", "sqlite")
				require.NoError(t, os.MkdirAll(oldDir, 0755))
				entries, err := os.ReadDir(filepath.Join(projectRoot, "migrations", "sqlite"))
				require.NoError(t, err)
				for _, entry := range entries {
					if strings.HasSuffix(entry.Name(), ".up.sql") && entry.Name() < "000009" {
						data, err := os.ReadFile(filepath.Join(projectRoot, "migrations", "sqlite", entry.Name()))
						require.NoError(t, err)
						require.NoError(t, os.WriteFile(filepath.Join(oldDir, entry.Name()), data, 0644))
					}
				}
				t.Chdir(oldRoot)
				require.NoError(t, d.RunMigrations(ctx))
				require.NoError(t, db.Exec("INSERT INTO api_keys (name, key_hash, key_prefix) VALUES ('legacy', 'legacy-hash', 'cl-old')").Error)
				t.Chdir(projectRoot)
			}
			require.NoError(t, d.RunMigrations(ctx))

			h := newOnboardingHandlerForTest(db)
			c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", gin.H{
				"provider": gin.H{"name": "migration-provider", "display_name": "迁移接入测试", "adapter_type": "openai_compatible", "base_url": "https://8.8.8.8/v1", "api_key": "test-only"},
				"models":   []gin.H{{"model_name": "migration-model", "provider_model": "upstream-model"}},
				"key":      gin.H{"name": "migration-key"},
			})
			// 不引用不存在的用户；本测试只验证真实迁移后的接入事务。
			setAdminContext(c, 0, 0, "admin")
			h.Commit(c)
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			var response struct {
				Data struct {
					ProviderID int64   `json:"provider_id"`
					ModelIDs   []int64 `json:"model_ids"`
					Key        string  `json:"key"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			require.Positive(t, response.Data.ProviderID)
			require.Len(t, response.Data.ModelIDs, 1)
			require.True(t, strings.HasPrefix(response.Data.Key, "cl-"))
			var hashes int64
			require.NoError(t, db.Table("api_key_hashes").Count(&hashes).Error)
			require.EqualValues(t, 1, hashes)

			var metadata []struct {
				KeySource       string
				SourceVendor    string
				PriceMultiplier float64
			}
			require.NoError(t, db.Table("api_keys").Select("key_source, source_vendor, price_multiplier").Scan(&metadata).Error)
			expectedKeys := 1
			if upgrade {
				expectedKeys++
			}
			require.Len(t, metadata, expectedKeys)
			for _, key := range metadata {
				require.Equal(t, "native", key.KeySource)
				require.Empty(t, key.SourceVendor)
				require.Equal(t, 1.0, key.PriceMultiplier)
			}

			// 重复启动不会重复加列，也不会丢失旧密钥或新建密钥。
			require.NoError(t, d.RunMigrations(ctx))
			var keyCount int64
			require.NoError(t, db.Table("api_keys").Count(&keyCount).Error)
			require.EqualValues(t, expectedKeys, keyCount)

			// 验证真实 down/up 文件可回滚再升级，保留密钥与哈希主体。
			down, err := os.ReadFile(filepath.Join(projectRoot, "migrations", "sqlite", "000009_api_key_model_parity.down.sql"))
			require.NoError(t, err)
			for _, stmt := range strings.Split(string(down), ";") {
				if strings.TrimSpace(stmt) != "" {
					require.NoError(t, db.Exec(stmt).Error)
				}
			}
			for _, column := range []string{"key_source", "source_vendor", "price_multiplier"} {
				require.False(t, db.Migrator().HasColumn("api_keys", column))
			}
			require.NoError(t, db.Exec("DELETE FROM schema_migrations WHERE version = '000009'").Error)
			require.NoError(t, d.RunMigrations(ctx))
			require.NoError(t, db.Table("api_keys").Select("key_source, source_vendor, price_multiplier").Scan(&metadata).Error)
			require.Len(t, metadata, expectedKeys)
			for _, key := range metadata {
				require.Equal(t, "native", key.KeySource)
				require.Empty(t, key.SourceVendor)
				require.Equal(t, 1.0, key.PriceMultiplier)
			}
			require.NoError(t, db.Table("api_key_hashes").Count(&hashes).Error)
			require.EqualValues(t, 1, hashes)
		})
	}
}
