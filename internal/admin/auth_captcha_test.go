package admin

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/crosslink/internal/captcha"
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupLoginTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Role{}))
	return db
}

func seedLoginUser(t *testing.T, db *gorm.DB, username, password string) *model.User {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	role := model.Role{Name: model.RoleAdmin}
	require.NoError(t, db.Create(&role).Error)
	u := &model.User{Username: username, PasswordHash: string(hash), DisplayName: username, RoleID: role.ID, Status: 1}
	require.NoError(t, db.Create(u).Error)
	return u
}

func newTestGate(t *testing.T) (*captcha.Gate, *captcha.RedisStore) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	store := captcha.NewRedisStore(rdb, "captcha:")
	prov := captcha.NewSliderProvider(store, captcha.DefaultSliderConfig())
	g := captcha.NewGate(prov, captcha.CaptchaGateConfig{
		Enabled: true, TrustDays: 7, TrustIPMask: 24, RedisFailOpen: true,
	}, []byte("test-jwt-secret-0123456789abcdef-test"))
	return g, store
}

// passingTrajectory builds a human-like trajectory landing on targetX.
func passingTrajectory(targetX float64) []captcha.Point {
	n := 40
	pts := make([]captcha.Point, n)
	for i := range pts {
		frac := float64(i) / float64(n-1)
		eased := 3*frac*frac - 2*frac*frac*frac
		pts[i] = captcha.Point{X: targetX * eased, Y: math.Sin(frac*9) * 1.5, TMs: int64(1400 * frac)}
	}
	return pts
}

// adminLoginHandlerForTest wraps LoginHandler with nil team/org repos (not
// needed for the captcha-gate assertions here).
func adminLoginHandlerForTest(userRepo *repository.UserRepo, roleRepo *repository.RoleRepo, cfg config.AdminConfig, cp crypto.CryptoProvider, gate *captcha.Gate) gin.HandlerFunc {
	return LoginHandler(userRepo, nil, roleRepo, nil, cfg, nil, cp, gate)
}

func callLogin(t *testing.T, h gin.HandlerFunc, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	c, w := newTestContext(t, http.MethodPost, "/admin/api/auth/login", body)
	c.Request.Header.Set("X-Forwarded-For", "1.2.3.4") // fixed ClientIP for IP binding
	h(c)
	return w
}

func TestLoginHandler_CaptchaRequired_NoEnumeration(t *testing.T) {
	db := setupLoginTestDB(t)
	seedLoginUser(t, db, "alice", "pass1234")
	userRepo := repository.NewUserRepo(db)
	roleRepo := repository.NewRoleRepo(db)
	gate, _ := newTestGate(t)
	cfg := config.AdminConfig{JWTSecret: "test-jwt-secret-0123456789abcdef-test", TokenExpiry: 1}
	cp, err := crypto.NewProvider("standard")
	require.NoError(t, err)

	handler := adminLoginHandlerForTest(userRepo, roleRepo, cfg, cp, gate)

	for _, username := range []string{"alice", "does-not-exist"} {
		w := callLogin(t, handler, map[string]any{"username": username, "password": "pass1234"})
		assert.Equal(t, http.StatusBadRequest, w.Code, "username=%s", username)
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, "captcha_required", body["error_code"],
			"anti-enumeration: identical captcha_required for username=%s", username)
	}
}

func TestLoginHandler_CaptchaSatisfied_LogsInAndSetsTrustCookie(t *testing.T) {
	db := setupLoginTestDB(t)
	seedLoginUser(t, db, "alice", "pass1234")
	userRepo := repository.NewUserRepo(db)
	roleRepo := repository.NewRoleRepo(db)
	gate, store := newTestGate(t)
	require.NoError(t, store.Save(context.Background(), "cap1",
		captcha.StoredChallenge{GapX: 100, IP: "1.2.3.4"}, 5*time.Minute))
	cfg := config.AdminConfig{JWTSecret: "test-jwt-secret-0123456789abcdef-test", TokenExpiry: 1}
	cp, err := crypto.NewProvider("standard")
	require.NoError(t, err)

	handler := adminLoginHandlerForTest(userRepo, roleRepo, cfg, cp, gate)

	w := callLogin(t, handler, map[string]any{
		"username": "alice", "password": "pass1234",
		"captcha_id":     "cap1",
		"captcha_answer": map[string]any{"final_x": 100, "points": passingTrajectory(100)},
	})
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var trustCookie string
	for _, c := range w.Result().Cookies() {
		if c.Name == captcha.TrustCookieName {
			trustCookie = c.Value
		}
	}
	assert.NotEmpty(t, trustCookie, "trust cookie should be set after captcha-gated login")
}

func TestLoginHandler_CaptchaDisabled_OldFlow(t *testing.T) {
	db := setupLoginTestDB(t)
	seedLoginUser(t, db, "alice", "pass1234")
	userRepo := repository.NewUserRepo(db)
	roleRepo := repository.NewRoleRepo(db)
	cfg := config.AdminConfig{JWTSecret: "test-jwt-secret-0123456789abcdef-test", TokenExpiry: 1}
	cp, err := crypto.NewProvider("standard")
	require.NoError(t, err)

	disabledGate := captcha.NewGate(nil, captcha.CaptchaGateConfig{Enabled: false}, []byte(cfg.JWTSecret))
	handler := adminLoginHandlerForTest(userRepo, roleRepo, cfg, cp, disabledGate)

	w := callLogin(t, handler, map[string]any{"username": "alice", "password": "pass1234"})
	assert.Equal(t, http.StatusOK, w.Code, "no captcha fields needed when disabled; body=%s", w.Body.String())
}
