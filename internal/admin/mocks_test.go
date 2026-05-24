package admin

import (
	"context"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
)

// mockTeamRepo implements TeamRepository for testing.
type mockTeamRepo struct {
	getByIDFn      func(ctx context.Context, id int64) (*model.Team, error)
	getMemberFn    func(ctx context.Context, teamID, userID int64) (*model.TeamMember, error)
	listByUserIDFn func(ctx context.Context, userID int64) ([]model.Team, error)
}

func (m *mockTeamRepo) GetByID(ctx context.Context, id int64) (*model.Team, error) {
	return m.getByIDFn(ctx, id)
}

func (m *mockTeamRepo) GetMember(ctx context.Context, teamID, userID int64) (*model.TeamMember, error) {
	return m.getMemberFn(ctx, teamID, userID)
}

func (m *mockTeamRepo) ListByUserID(ctx context.Context, userID int64) ([]model.Team, error) {
	return m.listByUserIDFn(ctx, userID)
}

// mockKeySvc implements KeyService for testing.
type mockKeySvc struct {
	getByIDFn       func(ctx context.Context, id int64) (*model.APIKey, error)
	listByCreatorFn func(ctx context.Context, userID int64) ([]model.APIKey, error)
	listFn          func(ctx context.Context) ([]model.APIKey, error)
	listByTeamFn    func(ctx context.Context, teamID int64) ([]model.APIKey, error)
	createFn        func(ctx context.Context, input *service.CreateKeyInput) (*service.CreateKeyResult, error)
	updateFn        func(ctx context.Context, key *model.APIKey) error
	deleteFn        func(ctx context.Context, id int64) error
	rotateFn        func(ctx context.Context, apiKeyID int64, gracePeriod time.Duration) (*service.RotateResult, error)
	regenerateFn    func(ctx context.Context, id int64) (*service.CreateKeyResult, error)
	listHashesFn    func(ctx context.Context, apiKeyID int64) ([]model.APIKeyHash, error)
}

func (m *mockKeySvc) GetByID(ctx context.Context, id int64) (*model.APIKey, error) {
	return m.getByIDFn(ctx, id)
}

func (m *mockKeySvc) ListByCreator(ctx context.Context, userID int64) ([]model.APIKey, error) {
	return m.listByCreatorFn(ctx, userID)
}

func (m *mockKeySvc) List(ctx context.Context) ([]model.APIKey, error) {
	return m.listFn(ctx)
}

func (m *mockKeySvc) ListByTeam(ctx context.Context, teamID int64) ([]model.APIKey, error) {
	return m.listByTeamFn(ctx, teamID)
}

func (m *mockKeySvc) Create(ctx context.Context, input *service.CreateKeyInput) (*service.CreateKeyResult, error) {
	return m.createFn(ctx, input)
}

func (m *mockKeySvc) Update(ctx context.Context, key *model.APIKey) error {
	return m.updateFn(ctx, key)
}

func (m *mockKeySvc) Delete(ctx context.Context, id int64) error {
	return m.deleteFn(ctx, id)
}

func (m *mockKeySvc) Rotate(ctx context.Context, apiKeyID int64, gracePeriod time.Duration) (*service.RotateResult, error) {
	return m.rotateFn(ctx, apiKeyID, gracePeriod)
}

func (m *mockKeySvc) Regenerate(ctx context.Context, id int64) (*service.CreateKeyResult, error) {
	return m.regenerateFn(ctx, id)
}

func (m *mockKeySvc) ListHashes(ctx context.Context, apiKeyID int64) ([]model.APIKeyHash, error) {
	return m.listHashesFn(ctx, apiKeyID)
}

// mockProviderRepo implements ProviderRepository for testing.
type mockProviderRepo struct {
	listFn    func(ctx context.Context) ([]model.Provider, error)
	getByIDFn func(ctx context.Context, id int64) (*model.Provider, error)
	createFn  func(ctx context.Context, provider *model.Provider) error
	updateFn  func(ctx context.Context, provider *model.Provider) error
	deleteFn  func(ctx context.Context, id int64) error
}

func (m *mockProviderRepo) List(ctx context.Context) ([]model.Provider, error) {
	return m.listFn(ctx)
}

func (m *mockProviderRepo) GetByID(ctx context.Context, id int64) (*model.Provider, error) {
	return m.getByIDFn(ctx, id)
}

func (m *mockProviderRepo) Create(ctx context.Context, provider *model.Provider) error {
	return m.createFn(ctx, provider)
}

func (m *mockProviderRepo) Update(ctx context.Context, provider *model.Provider) error {
	return m.updateFn(ctx, provider)
}

func (m *mockProviderRepo) Delete(ctx context.Context, id int64) error {
	return m.deleteFn(ctx, id)
}

// mockProviderModelRepo implements ProviderModelRepository for testing.
type mockProviderModelRepo struct {
	countByProviderIDFn  func(ctx context.Context, providerID int64) (int64, error)
	firstByProviderIDFn  func(ctx context.Context, providerID int64) (*model.ProviderModel, error)
}

func (m *mockProviderModelRepo) CountByProviderID(ctx context.Context, providerID int64) (int64, error) {
	return m.countByProviderIDFn(ctx, providerID)
}

func (m *mockProviderModelRepo) FirstByProviderID(ctx context.Context, providerID int64) (*model.ProviderModel, error) {
	return m.firstByProviderIDFn(ctx, providerID)
}
