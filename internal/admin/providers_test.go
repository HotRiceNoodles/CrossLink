package admin

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
)

// newProviderHandler creates a ProviderHandler with all mocks initialized.
// registry, cacheSvc, secretResolver, encStore, auditSvc, usageSvc are set to
// safe nil/minimal defaults — only repo and modelRepo are wired for test control.
func newProviderHandler(
	repo *mockProviderRepo,
	modelRepo *mockProviderModelRepo,
) *ProviderHandler {
	return &ProviderHandler{
		repo:      repo,
		modelRepo: modelRepo,
		cache:     &noopCacheInvalidator{},
		registry:  provider.NewRegistry(),
		cacheSvc:  nil,
		auditSvc:  nil,
		usageSvc:  nil,
	}
}

// noopCacheInvalidator is a no-op CacheInvalidator for tests.
type noopCacheInvalidator struct{}

func (noopCacheInvalidator) Invalidate() {}

// defaultProviderMocks returns provider handler dependencies with permissive defaults.
func defaultProviderMocks() (*mockProviderRepo, *mockProviderModelRepo) {
	repo := &mockProviderRepo{
		listFn: func(ctx context.Context, orgID int64) ([]model.Provider, error) {
			return nil, nil
		},
		getByIDFn: func(ctx context.Context, orgID, id int64) (*model.Provider, error) {
			return &model.Provider{ID: id, Name: "test-provider", Status: 1}, nil
		},
		createFn: func(ctx context.Context, p *model.Provider) error {
			p.ID = 1
			return nil
		},
		updateFn: func(ctx context.Context, p *model.Provider) error {
			return nil
		},
		deleteFn: func(ctx context.Context, id int64) error {
			return nil
		},
	}
	modelRepo := &mockProviderModelRepo{
		countByProviderIDFn: func(ctx context.Context, providerID int64) (int64, error) {
			return 0, nil
		},
		firstByProviderIDFn: func(ctx context.Context, providerID int64) (*model.ProviderModel, error) {
			return nil, errors.New("not found")
		},
	}
	return repo, modelRepo
}

// --- Create tests ---

func TestProviderHandler_Create_InvalidURL(t *testing.T) {
	repo, _ := defaultProviderMocks()
	h := newProviderHandler(repo, nil)

	body := map[string]any{
		"name":         "bad-url-provider",
		"display_name": "Bad URL",
		"adapter_type": "openai",
		"base_url":     "ftp://bad.example.com",
	}
	c, w := newTestContext(t, http.MethodPost, "/api/v1/providers", body)
	setAdminContext(c, 1, 1, "admin")

	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if resp["error_code"] != ErrProviderURLInvalid {
		t.Errorf("error_code = %v, want %s", resp["error_code"], ErrProviderURLInvalid)
	}
}

func TestProviderHandler_Create_InvalidRequest(t *testing.T) {
	repo, _ := defaultProviderMocks()
	h := newProviderHandler(repo, nil)

	// Missing required fields: name, display_name, adapter_type
	body := map[string]any{
		"base_url": "https://api.example.com",
	}
	c, w := newTestContext(t, http.MethodPost, "/api/v1/providers", body)
	setAdminContext(c, 1, 1, "admin")

	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if resp["error_code"] != ErrInvalidRequest {
		t.Errorf("error_code = %v, want %s", resp["error_code"], ErrInvalidRequest)
	}
}

// --- Update tests ---

// TestProviderHandler_Update_EmptyAPIKey_PreservesExisting verifies that sending
// an empty api_key on edit (which the frontend does when opening the edit dialog)
// does NOT overwrite the existing key with an empty value.
func TestProviderHandler_Update_EmptyAPIKey_PreservesExisting(t *testing.T) {
	repo, _ := defaultProviderMocks()
	const existingKey = "secret-existing-key"
	repo.getByIDFn = func(ctx context.Context, orgID, id int64) (*model.Provider, error) {
		return &model.Provider{ID: id, Name: "test-provider", Status: 1, AdapterType: "openai_compatible", APIKey: existingKey}, nil
	}
	var saved *model.Provider
	repo.updateFn = func(ctx context.Context, p *model.Provider) error {
		saved = p
		return nil
	}
	h := newProviderHandler(repo, nil)

	body := map[string]any{
		"display_name": "Updated Name",
		"api_key":      "",
	}
	c, w := newTestContext(t, http.MethodPut, "/api/v1/providers/1", body)
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "id", Value: "1"}})

	h.Update(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if saved == nil {
		t.Fatal("updateFn was not called")
	}
	if saved.APIKey != existingKey {
		t.Errorf("APIKey = %q, want %q (existing key should be preserved when api_key is empty)", saved.APIKey, existingKey)
	}
	if saved.DisplayName != "Updated Name" {
		t.Errorf("DisplayName = %q, want %q", saved.DisplayName, "Updated Name")
	}
}

// TestProviderHandler_Update_NewAPIKey_Overwrites verifies that a non-empty
// api_key on edit replaces the existing key.
func TestProviderHandler_Update_NewAPIKey_Overwrites(t *testing.T) {
	repo, _ := defaultProviderMocks()
	repo.getByIDFn = func(ctx context.Context, orgID, id int64) (*model.Provider, error) {
		return &model.Provider{ID: id, Name: "test-provider", Status: 1, AdapterType: "openai_compatible", APIKey: "old-key"}, nil
	}
	var saved *model.Provider
	repo.updateFn = func(ctx context.Context, p *model.Provider) error {
		saved = p
		return nil
	}
	h := newProviderHandler(repo, nil)

	body := map[string]any{
		"api_key": "new-key",
	}
	c, w := newTestContext(t, http.MethodPut, "/api/v1/providers/1", body)
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "id", Value: "1"}})

	h.Update(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if saved == nil {
		t.Fatal("updateFn was not called")
	}
	if saved.APIKey != "new-key" {
		t.Errorf("APIKey = %q, want %q", saved.APIKey, "new-key")
	}
}

// --- Delete tests ---

func TestProviderHandler_Delete_HasModels(t *testing.T) {
	repo, modelRepo := defaultProviderMocks()
	// Provider has 2 model mappings — delete should be rejected
	modelRepo.countByProviderIDFn = func(ctx context.Context, providerID int64) (int64, error) {
		return 2, nil
	}
	h := newProviderHandler(repo, modelRepo)

	c, w := newTestContext(t, http.MethodDelete, "/api/v1/providers/1", nil)
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "id", Value: "1"}})

	h.Delete(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusConflict)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if resp["error_code"] != ErrProviderHasModels {
		t.Errorf("error_code = %v, want %s", resp["error_code"], ErrProviderHasModels)
	}
}

func TestProviderHandler_Delete_Success(t *testing.T) {
	repo, modelRepo := defaultProviderMocks()
	// No model mappings — delete should succeed
	modelRepo.countByProviderIDFn = func(ctx context.Context, providerID int64) (int64, error) {
		return 0, nil
	}
	h := newProviderHandler(repo, modelRepo)

	c, w := newTestContext(t, http.MethodDelete, "/api/v1/providers/1", nil)
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "id", Value: "1"}})

	h.Delete(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if resp["message"] != "deleted" {
		t.Errorf("message = %v, want 'deleted'", resp["message"])
	}
}

func TestProviderHandler_Delete_NotFound(t *testing.T) {
	repo, modelRepo := defaultProviderMocks()
	repo.getByIDFn = func(ctx context.Context, orgID, id int64) (*model.Provider, error) {
		return nil, errors.New("not found")
	}
	h := newProviderHandler(repo, modelRepo)

	c, w := newTestContext(t, http.MethodDelete, "/api/v1/providers/999", nil)
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "id", Value: "999"}})

	h.Delete(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if resp["error_code"] != ErrNotFound {
		t.Errorf("error_code = %v, want %s", resp["error_code"], ErrNotFound)
	}
}

func TestProviderHandler_Delete_InvalidID(t *testing.T) {
	repo, _ := defaultProviderMocks()
	h := newProviderHandler(repo, nil)

	c, w := newTestContext(t, http.MethodDelete, "/api/v1/providers/abc", nil)
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "id", Value: "abc"}})

	h.Delete(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if resp["error_code"] != ErrInvalidID {
		t.Errorf("error_code = %v, want %s", resp["error_code"], ErrInvalidID)
	}
}
