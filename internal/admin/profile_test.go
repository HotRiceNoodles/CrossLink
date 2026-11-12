package admin

// profile_test.go — 个人中心自助资料端点：GET 白名单输出 / PUT 定向列更新与校验。
// 身份经 c.Set("user_id")（与 auth 中间件同键），不测中间件本身。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
)

func setupProfileTest(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Role{}))

	role := model.Role{Name: model.RoleAdmin}
	require.NoError(t, db.Create(&role).Error)
	email := "admin@crosslink.com"
	lastLogin := time.Now()
	u := &model.User{Username: "admin", DisplayName: "Administrator", RoleID: role.ID, Status: 1, Email: &email, LastLoginAt: &lastLogin}
	require.NoError(t, db.Create(u).Error)

	h := NewProfileHandler(repository.NewUserRepo(db), nil)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("user_id", u.ID) })
	r.GET("/user/profile", h.Get)
	r.PUT("/user/profile", h.Update)
	return r, db
}

func doProfile(t *testing.T, r *gin.Engine, method, path, body string) (int, map[string]any) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestProfileGetWhitelist(t *testing.T) {
	r, _ := setupProfileTest(t)
	code, body := doProfile(t, r, http.MethodGet, "/user/profile", "")
	require.Equal(t, 200, code)
	data := body["data"].(map[string]any)
	// 白名单字段齐备；绝不携带 password_hash
	for _, k := range []string{"id", "username", "display_name", "role_name", "email", "created_at", "last_login_at", "force_password_change"} {
		assert.Contains(t, data, k)
	}
	assert.Equal(t, "admin", data["username"])
	assert.Equal(t, "Administrator", data["display_name"])
	assert.Equal(t, "admin@crosslink.com", data["email"])
	assert.NotContains(t, body, "password_hash")
}

func TestProfileUpdateDisplayNameAndEmail(t *testing.T) {
	r, db := setupProfileTest(t)
	code, body := doProfile(t, r, http.MethodPut, "/user/profile",
		`{"display_name":"平台管理员","email":"new@crosslink.com"}`)
	require.Equal(t, 200, code)
	assert.Equal(t, "平台管理员", body["data"].(map[string]any)["display_name"])
	assert.Equal(t, "new@crosslink.com", body["data"].(map[string]any)["email"])

	var u model.User
	require.NoError(t, db.Preload("Role").First(&u, "username = ?", "admin").Error)
	assert.Equal(t, "平台管理员", u.DisplayName)
	require.NotNil(t, u.Email)
	assert.Equal(t, "new@crosslink.com", *u.Email)
	// 定向列更新不得触碰角色/状态
	assert.EqualValues(t, 1, u.RoleID)
	assert.EqualValues(t, 1, u.Status)
}

func TestProfileUpdateValidation(t *testing.T) {
	r, _ := setupProfileTest(t)
	// display_name 空 / 超长
	code, b := doProfile(t, r, http.MethodPut, "/user/profile", `{"display_name":"  "}`)
	assert.Equal(t, 400, code, "空白名应 400")
	assert.Equal(t, "invalid_request", b["error_code"])
	code, b = doProfile(t, r, http.MethodPut, "/user/profile", `{"display_name":"`+strings.Repeat("x", 129)+`"}`)
	assert.Equal(t, 400, code, "超长名应 400")
	// email 格式
	code, b = doProfile(t, r, http.MethodPut, "/user/profile", `{"email":"not-an-email"}`)
	assert.Equal(t, 400, code, "无 @ 邮箱应 400")
	_ = b
	// 清空邮箱（空串合法：置 NULL）
	code, body := doProfile(t, r, http.MethodPut, "/user/profile", `{"email":""}`)
	require.Equal(t, 200, code)
	assert.NotContains(t, body["data"].(map[string]any), "email", "空邮箱不应输出 email 键")
}
