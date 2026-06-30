package admin

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
)

// newKeyHandler creates a KeyHandler with all mocks initialized.
// rdb and auditSvc are always nil (not needed for these tests).
func newKeyHandler(
	keySvc *mockKeySvc,
	teamRepo *mockTeamRepo,
) *KeyHandler {
	return &KeyHandler{
		keySvc:   keySvc,
		teamRepo: teamRepo,
		rdb:      nil,
		auditSvc: nil,
	}
}

// defaultKeyMocks returns KeyHandler dependencies with permissive defaults.
func defaultKeyMocks() (*mockKeySvc, *mockTeamRepo) {
	keySvc := &mockKeySvc{
		getByIDFn: func(ctx context.Context, orgID int64, id int64) (*model.APIKey, error) {
			uid := id * 10
			return &model.APIKey{ID: id, Name: "test-key", CreatedByID: testInt64Ptr(uid)}, nil
		},
		listByCreatorFn: func(ctx context.Context, userID int64) ([]model.APIKey, error) {
			return nil, nil
		},
		listFn: func(ctx context.Context, orgID int64) ([]model.APIKey, error) {
			return nil, nil
		},
		listByTeamFn: func(ctx context.Context, teamID int64) ([]model.APIKey, error) {
			return nil, nil
		},
		createFn: func(ctx context.Context, input *service.CreateKeyInput) (*service.CreateKeyResult, error) {
			return &service.CreateKeyResult{APIKey: "cl-testkey", KeyPrefix: "cl-te"}, nil
		},
		updateFn: func(ctx context.Context, key *model.APIKey) error {
			return nil
		},
		deleteFn: func(ctx context.Context, id int64) error {
			return nil
		},
		rotateFn: func(ctx context.Context, apiKeyID int64, gracePeriod time.Duration) (*service.RotateResult, error) {
			return &service.RotateResult{NewKey: "cl-rotated", KeyPrefix: "cl-ro"}, nil
		},
		regenerateFn: func(ctx context.Context, id int64) (*service.CreateKeyResult, error) {
			return &service.CreateKeyResult{APIKey: "cl-regen", KeyPrefix: "cl-re"}, nil
		},
		listHashesFn: func(ctx context.Context, apiKeyID int64) ([]model.APIKeyHash, error) {
			return nil, nil
		},
	}
	teamRepo := &mockTeamRepo{
		getByIDFn: func(ctx context.Context, id int64) (*model.Team, error) {
			return &model.Team{ID: id}, nil
		},
		getMemberFn: func(ctx context.Context, teamID, userID int64) (*model.TeamMember, error) {
			return &model.TeamMember{ID: 1, TeamID: teamID, UserID: userID}, nil
		},
		listByUserIDFn: func(ctx context.Context, userID int64) ([]model.Team, error) {
			return nil, nil
		},
	}
	return keySvc, teamRepo
}

// --- Create tests ---

func TestKeyHandler_Create_InvalidBudgetPeriod(t *testing.T) {
	keySvc, _ := defaultKeyMocks()
	h := newKeyHandler(keySvc, nil)

	body := map[string]any{
		"name":          "test-key",
		"budget_period": "yearly",
	}
	c, w := newTestContext(t, http.MethodPost, "/api/v1/keys", body)
	setAdminContext(c, 1, 1, "admin")

	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if resp["error_code"] != ErrBudgetPeriodInvalid {
		t.Errorf("error_code = %v, want %s", resp["error_code"], ErrBudgetPeriodInvalid)
	}
}

func TestKeyHandler_Create_NegativeBudget(t *testing.T) {
	keySvc, _ := defaultKeyMocks()
	h := newKeyHandler(keySvc, nil)

	body := map[string]any{
		"name":       "test-key",
		"max_budget": -10.0,
	}
	c, w := newTestContext(t, http.MethodPost, "/api/v1/keys", body)
	setAdminContext(c, 1, 1, "admin")

	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if resp["error_code"] != ErrBudgetNegative {
		t.Errorf("error_code = %v, want %s", resp["error_code"], ErrBudgetNegative)
	}
}

func TestKeyHandler_Create_Success(t *testing.T) {
	keySvc, _ := defaultKeyMocks()
	h := newKeyHandler(keySvc, nil)

	body := map[string]any{
		"name":          "test-key",
		"budget_period": "monthly",
		"max_budget":    100.0,
	}
	c, w := newTestContext(t, http.MethodPost, "/api/v1/keys", body)
	setAdminContext(c, 1, 1, "admin")

	h.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusCreated)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatal("response missing 'data' key")
	}
	if data["key"] == nil {
		t.Error("expected key in response data")
	}
}

// --- Update ownership tests ---

func TestKeyHandler_Update_NonOwnerForbidden(t *testing.T) {
	keySvc, _ := defaultKeyMocks()
	// Key created by user 10, but request is from user 42 (non-admin, no team)
	keySvc.getByIDFn = func(ctx context.Context, orgID int64, id int64) (*model.APIKey, error) {
		return &model.APIKey{
			ID:          id,
			Name:        "owned-key",
			CreatedByID: testInt64Ptr(10),
			TeamID:      nil, // no team — ownership is based on CreatedByID
		}, nil
	}
	h := newKeyHandler(keySvc, nil)

	body := map[string]any{
		"tpm_limit": 1000,
	}
	c, w := newTestContext(t, http.MethodPut, "/api/v1/keys/1", body)
	// user_id=42, role=user — not admin, not the creator
	setAdminContext(c, 42, 2, "user")
	setPathParams(c, gin.Params{{Key: "id", Value: "1"}})

	h.Update(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if resp["error_code"] != ErrInsufficientPermissions {
		t.Errorf("error_code = %v, want %s", resp["error_code"], ErrInsufficientPermissions)
	}
}

func TestKeyHandler_Update_OwnerSuccess(t *testing.T) {
	keySvc, _ := defaultKeyMocks()
	// Key created by user 10, request from same user
	keySvc.getByIDFn = func(ctx context.Context, orgID int64, id int64) (*model.APIKey, error) {
		return &model.APIKey{
			ID:          id,
			Name:        "owned-key",
			CreatedByID: testInt64Ptr(10),
			TeamID:      nil,
		}, nil
	}
	h := newKeyHandler(keySvc, nil)

	body := map[string]any{
		"tpm_limit": 1000,
	}
	c, w := newTestContext(t, http.MethodPut, "/api/v1/keys/1", body)
	// user_id=10 — matches CreatedByID
	setAdminContext(c, 10, 2, "user")
	setPathParams(c, gin.Params{{Key: "id", Value: "1"}})

	h.Update(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

// --- Delete ownership tests ---

func TestKeyHandler_Delete_Success(t *testing.T) {
	keySvc, _ := defaultKeyMocks()
	// Key created by user 10, request from same user
	keySvc.getByIDFn = func(ctx context.Context, orgID int64, id int64) (*model.APIKey, error) {
		return &model.APIKey{
			ID:          id,
			Name:        "owned-key",
			CreatedByID: testInt64Ptr(10),
			TeamID:      nil,
		}, nil
	}
	h := newKeyHandler(keySvc, nil)

	c, w := newTestContext(t, http.MethodDelete, "/api/v1/keys/1", nil)
	// user_id=10 — matches CreatedByID
	setAdminContext(c, 10, 2, "user")
	setPathParams(c, gin.Params{{Key: "id", Value: "1"}})

	h.Delete(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if resp["data"] != "deleted" {
		t.Errorf("data = %v, want 'deleted'", resp["data"])
	}
}

func TestKeyHandler_Delete_NonOwnerForbidden(t *testing.T) {
	keySvc, _ := defaultKeyMocks()
	// Key created by user 10, request from user 42 (non-admin)
	keySvc.getByIDFn = func(ctx context.Context, orgID int64, id int64) (*model.APIKey, error) {
		return &model.APIKey{
			ID:          id,
			Name:        "owned-key",
			CreatedByID: testInt64Ptr(10),
			TeamID:      nil,
		}, nil
	}
	h := newKeyHandler(keySvc, nil)

	c, w := newTestContext(t, http.MethodDelete, "/api/v1/keys/1", nil)
	// user_id=42 — not admin, not the creator
	setAdminContext(c, 42, 2, "user")
	setPathParams(c, gin.Params{{Key: "id", Value: "1"}})

	h.Delete(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if resp["error_code"] != ErrInsufficientPermissions {
		t.Errorf("error_code = %v, want %s", resp["error_code"], ErrInsufficientPermissions)
	}
}

func TestKeyHandler_Delete_TeamMemberAllowed(t *testing.T) {
	keySvc, teamRepo := defaultKeyMocks()
	// Key belongs to team 5, user 42 is a member of that team
	teamID := int64(5)
	keySvc.getByIDFn = func(ctx context.Context, orgID int64, id int64) (*model.APIKey, error) {
		return &model.APIKey{
			ID:          id,
			Name:        "team-key",
			CreatedByID: testInt64Ptr(10),
			TeamID:      &teamID,
		}, nil
	}
	// teamRepo.GetMember returns successfully — user 42 is a member
	teamRepo.getMemberFn = func(ctx context.Context, teamID, userID int64) (*model.TeamMember, error) {
		return &model.TeamMember{ID: 1, TeamID: teamID, UserID: userID}, nil
	}
	h := newKeyHandler(keySvc, teamRepo)

	c, w := newTestContext(t, http.MethodDelete, "/api/v1/keys/1", nil)
	setAdminContext(c, 42, 2, "user")
	setPathParams(c, gin.Params{{Key: "id", Value: "1"}})

	h.Delete(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestKeyHandler_Delete_TeamNonMemberForbidden(t *testing.T) {
	keySvc, teamRepo := defaultKeyMocks()
	// Key belongs to team 5, user 42 is NOT a member
	teamID := int64(5)
	keySvc.getByIDFn = func(ctx context.Context, orgID int64, id int64) (*model.APIKey, error) {
		return &model.APIKey{
			ID:          id,
			Name:        "team-key",
			CreatedByID: testInt64Ptr(10),
			TeamID:      &teamID,
		}, nil
	}
	// teamRepo.GetMember returns error — user 42 is not a member
	teamRepo.getMemberFn = func(ctx context.Context, teamID, userID int64) (*model.TeamMember, error) {
		return nil, errors.New("not a member")
	}
	h := newKeyHandler(keySvc, teamRepo)

	c, w := newTestContext(t, http.MethodDelete, "/api/v1/keys/1", nil)
	setAdminContext(c, 42, 2, "user")
	setPathParams(c, gin.Params{{Key: "id", Value: "1"}})

	h.Delete(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if resp["error_code"] != ErrInsufficientPermissions {
		t.Errorf("error_code = %v, want %s", resp["error_code"], ErrInsufficientPermissions)
	}
}

func TestKeyHandler_Delete_NotFound(t *testing.T) {
	keySvc, _ := defaultKeyMocks()
	keySvc.getByIDFn = func(ctx context.Context, orgID int64, id int64) (*model.APIKey, error) {
		return nil, errors.New("not found")
	}
	h := newKeyHandler(keySvc, nil)

	c, w := newTestContext(t, http.MethodDelete, "/api/v1/keys/999", nil)
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "id", Value: "999"}})

	h.Delete(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if resp["error_code"] != ErrKeyNotFound {
		t.Errorf("error_code = %v, want %s", resp["error_code"], ErrKeyNotFound)
	}
}
