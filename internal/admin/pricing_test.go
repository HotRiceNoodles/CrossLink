package admin

import (
	"context"
	"net/http"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func floatPtr(v float64) *float64 { return &v }

// --- Key Create/Update with PriceMultiplier ---

func TestKey_Create_WithPriceMultiplier(t *testing.T) {
	var capturedInput *service.CreateKeyInput
	keySvc, _ := defaultKeyMocks()
	keySvc.createFn = func(_ context.Context, input *service.CreateKeyInput) (*service.CreateKeyResult, error) {
		capturedInput = input
		return &service.CreateKeyResult{APIKey: "cl-test", KeyPrefix: "cl-te"}, nil
	}
	h := newKeyHandler(keySvc, &mockTeamRepo{getByIDFn: func(_ context.Context, _ int64) (*model.Team, error) { return nil, gorm.ErrRecordNotFound }})

	c, w := newTestContext(t, http.MethodPost, "/admin/api/keys", gin.H{
		"name":             "priced-key",
		"price_multiplier": 1.5,
	})
	setAdminContext(c, 1, 1, "admin")
	h.Create(c)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	require.NotNil(t, capturedInput.PriceMultiplier)
	assert.Equal(t, 1.5, *capturedInput.PriceMultiplier)
}

func TestKey_Create_DefaultPriceMultiplier(t *testing.T) {
	var capturedInput *service.CreateKeyInput
	keySvc, _ := defaultKeyMocks()
	keySvc.createFn = func(_ context.Context, input *service.CreateKeyInput) (*service.CreateKeyResult, error) {
		capturedInput = input
		return &service.CreateKeyResult{APIKey: "cl-test", KeyPrefix: "cl-te"}, nil
	}
	h := newKeyHandler(keySvc, &mockTeamRepo{getByIDFn: func(_ context.Context, _ int64) (*model.Team, error) { return nil, gorm.ErrRecordNotFound }})

	c, w := newTestContext(t, http.MethodPost, "/admin/api/keys", gin.H{"name": "default-key"})
	setAdminContext(c, 1, 1, "admin")
	h.Create(c)

	require.Equal(t, http.StatusCreated, w.Code)
	assert.Nil(t, capturedInput.PriceMultiplier, "nil = not provided → service defaults to 1.0")
}

func TestKey_PriceMultiplier_Validation(t *testing.T) {
	cases := []struct {
		name string
		val  float64
	}{
		{"too low", 0.001},
		{"zero", 0},
		{"negative", -1},
		{"too high", 11},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keySvc, _ := defaultKeyMocks()
			h := newKeyHandler(keySvc, &mockTeamRepo{getByIDFn: func(_ context.Context, _ int64) (*model.Team, error) { return nil, gorm.ErrRecordNotFound }})
			c, w := newTestContext(t, http.MethodPost, "/admin/api/keys", gin.H{
				"name":             "x",
				"price_multiplier": tc.val,
			})
			setAdminContext(c, 1, 1, "admin")
			h.Create(c)
			assert.Equal(t, http.StatusBadRequest, w.Code, "val=%v should be rejected", tc.val)
		})
	}
}

func TestKey_PriceMultiplier_ValidRange(t *testing.T) {
	for _, val := range []float64{0.01, 0.5, 1.0, 1.3, 5.0, 10.0} {
		t.Run("", func(t *testing.T) {
			keySvc, _ := defaultKeyMocks()
			h := newKeyHandler(keySvc, &mockTeamRepo{getByIDFn: func(_ context.Context, _ int64) (*model.Team, error) { return nil, gorm.ErrRecordNotFound }})
			c, w := newTestContext(t, http.MethodPost, "/admin/api/keys", gin.H{
				"name":             "x",
				"price_multiplier": val,
			})
			setAdminContext(c, 1, 1, "admin")
			h.Create(c)
			assert.Equal(t, http.StatusCreated, w.Code, "val=%v should be accepted", val)
		})
	}
}

func TestKey_Update_PriceMultiplier(t *testing.T) {
	var updatedKey *model.APIKey
	keySvc, _ := defaultKeyMocks()
	keySvc.getByIDFn = func(_ context.Context, _ int64, id int64) (*model.APIKey, error) {
		return &model.APIKey{ID: id, Name: "k", PriceMultiplier: 1.0, Status: 1}, nil
	}
	keySvc.updateFn = func(_ context.Context, key *model.APIKey) error {
		updatedKey = key
		return nil
	}
	h := newKeyHandler(keySvc, &mockTeamRepo{})

	c, w := newTestContext(t, http.MethodPut, "/admin/api/keys/1", gin.H{
		"price_multiplier": 2.5,
	})
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "id", Value: "1"}})
	h.Update(c)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.NotNil(t, updatedKey)
	assert.Equal(t, 2.5, updatedKey.PriceMultiplier)
}

// --- UsageService billable_cost ---

func TestUsage_LogBillableCost(t *testing.T) {
	db := setupUsagePricingDB(t)
	svc := service.NewUsageService(repository.NewUsageLogRepo(db))

	svc.Log(context.Background(), &service.UsageEntry{
		RouteType:       "openai",
		ModelRequested:  "gpt-4o",
		InputTokens:     1000,
		OutputTokens:    500,
		InputPrice:      0.0014,
		OutputPrice:     0.0028,
		Currency:        "USD",
		PriceMultiplier: 1.5,
	})

	var rows []model.UsageLog
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1)
	// cost = 0.0014*1000/1000 + 0.0028*500/1000 = 0.0014 + 0.0014 = 0.0028
	expectedCost := 0.0028
	assert.InDelta(t, expectedCost, rows[0].Cost, 0.0001, "upstream cost")
	assert.InDelta(t, expectedCost*1.5, rows[0].BillableCost, 0.0001, "billable = cost × 1.5")
}

func TestUsage_BillableCost_DefaultMultiplier(t *testing.T) {
	db := setupUsagePricingDB(t)
	svc := service.NewUsageService(repository.NewUsageLogRepo(db))

	// PriceMultiplier = 0 → service defaults to 1.0 → billable = cost
	svc.Log(context.Background(), &service.UsageEntry{
		RouteType:       "openai",
		ModelRequested:  "m",
		InputTokens:     100,
		InputPrice:      0.01,
		Currency:        "USD",
		PriceMultiplier: 0,
	})

	var rows []model.UsageLog
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1)
	expectedCost := 0.001 // 0.01 * 100/1000
	assert.InDelta(t, expectedCost, rows[0].Cost, 0.0001)
	assert.InDelta(t, expectedCost, rows[0].BillableCost, 0.0001, "default mult → billable = cost")
}

// --- Reconciliation Export ---

func TestUsage_ReconciliationExport_CSV(t *testing.T) {
	db := setupUsagePricingDB(t)
	keyID := int64(1)
	require.NoError(t, db.Create(&model.APIKey{ID: keyID, Name: "cust-a", KeyPrefix: "cl-ab", KeyHash: "h1", Status: 1, PriceMultiplier: 1.5}).Error)
	require.NoError(t, db.Create(&model.UsageLog{
		ModelUsed: "gpt-4o", InputTokens: 100, OutputTokens: 50,
		Cost: 0.01, BillableCost: 0.015, Currency: "USD", APIKeyID: &keyID,
	}).Error)
	require.NoError(t, db.Create(&model.UsageLog{
		ModelUsed: "gpt-4o", InputTokens: 200, OutputTokens: 100,
		Cost: 0.02, BillableCost: 0.03, Currency: "USD", APIKeyID: &keyID,
	}).Error)

	h := NewUsageHandler(db, "")
	c, w := newTestContext(t, http.MethodGet, "/admin/api/usage/reconciliation/export?days=30", nil)
	setAdminContext(c, 1, 1, "admin")
	h.ReconciliationExport(c)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/csv")
	body := w.Body.String()
	assert.Contains(t, body, "cust-a")
	assert.Contains(t, body, "gpt-4o")
	// Aggregated: 2 requests
	assert.Contains(t, body, "0.0300") // upstream_cost = 0.01 + 0.02
	assert.Contains(t, body, "0.0450") // billable_cost = 0.015 + 0.03
}

func TestUsage_ReconciliationExport_KeyFilter(t *testing.T) {
	db := setupUsagePricingDB(t)
	key1, key2 := int64(1), int64(2)
	require.NoError(t, db.Create(&model.APIKey{ID: key1, Name: "a", KeyPrefix: "cl-a", KeyHash: "h1", Status: 1}).Error)
	require.NoError(t, db.Create(&model.APIKey{ID: key2, Name: "b", KeyPrefix: "cl-b", KeyHash: "h2", Status: 1}).Error)
	require.NoError(t, db.Create(&model.UsageLog{ModelUsed: "m1", Cost: 0.01, BillableCost: 0.01, Currency: "USD", APIKeyID: &key1}).Error)
	require.NoError(t, db.Create(&model.UsageLog{ModelUsed: "m2", Cost: 0.02, BillableCost: 0.02, Currency: "USD", APIKeyID: &key2}).Error)

	h := NewUsageHandler(db, "")
	c, w := newTestContext(t, http.MethodGet, "/admin/api/usage/reconciliation/export?key_id=2&days=30", nil)
	setAdminContext(c, 1, 1, "admin")
	h.ReconciliationExport(c)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "m2", "key2's model should appear")
	assert.NotContains(t, body, "m1", "key1's model should NOT appear")
}

// --- helpers ---

func setupUsagePricingDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.APIKey{}, &model.UsageLog{}))
	return db
}
