package admin

import (
	"context"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
)

// UserRepository defines the repository methods needed by UserHandler.
type UserRepository interface {
	GetByID(ctx context.Context, id int64) (*model.User, error)
	List(ctx context.Context) ([]model.User, error)
	Create(ctx context.Context, user *model.User) error
	Update(ctx context.Context, user *model.User) error
	Delete(ctx context.Context, id int64) error
	CountByRoleName(ctx context.Context, roleName string) (int64, error)
}

// RoleRepository defines the role repository methods needed by UserHandler.
type RoleRepository interface {
	GetByID(ctx context.Context, id int64) (*model.Role, error)
}

// BudgetAlertRepository defines the repository methods needed by BudgetHandler.
type BudgetAlertRepository interface {
	Create(ctx context.Context, alert *model.BudgetAlert) error
	List(ctx context.Context) ([]model.BudgetAlert, error)
	GetByID(ctx context.Context, id int64) (*model.BudgetAlert, error)
	Delete(ctx context.Context, id int64) error
}

// TeamRepository defines the team repository methods needed by BudgetHandler.
type TeamRepository interface {
	GetByID(ctx context.Context, id int64) (*model.Team, error)
	GetMember(ctx context.Context, teamID, userID int64) (*model.TeamMember, error)
	ListByUserID(ctx context.Context, userID int64) ([]model.Team, error)
}

// BudgetService defines the budget service methods needed by BudgetHandler.
type BudgetService interface {
	GetCurrentSpent(ctx context.Context, scope, targetID, period string) float64
}

// KeyService defines the key service methods needed by BudgetHandler and KeyHandler.
type KeyService interface {
	GetByID(ctx context.Context, id int64) (*model.APIKey, error)
	ListByCreator(ctx context.Context, userID int64) ([]model.APIKey, error)
	List(ctx context.Context) ([]model.APIKey, error)
	ListByTeam(ctx context.Context, teamID int64) ([]model.APIKey, error)
	Create(ctx context.Context, input *service.CreateKeyInput) (*service.CreateKeyResult, error)
	Update(ctx context.Context, key *model.APIKey) error
	Delete(ctx context.Context, id int64) error
	Rotate(ctx context.Context, apiKeyID int64, gracePeriod time.Duration) (*service.RotateResult, error)
	Regenerate(ctx context.Context, id int64) (*service.CreateKeyResult, error)
	ListHashes(ctx context.Context, apiKeyID int64) ([]model.APIKeyHash, error)
}

// CalibrateService defines the calibration service methods needed by BudgetHandler.
type CalibrateService interface {
	CalibrateOnce(ctx context.Context) error
}

// ProviderRepository defines the repository methods needed by ProviderHandler.
type ProviderRepository interface {
	List(ctx context.Context) ([]model.Provider, error)
	GetByID(ctx context.Context, id int64) (*model.Provider, error)
	Create(ctx context.Context, provider *model.Provider) error
	Update(ctx context.Context, provider *model.Provider) error
	Delete(ctx context.Context, id int64) error
}

// ProviderModelRepository defines the model repository methods needed by ProviderHandler.
type ProviderModelRepository interface {
	CountByProviderID(ctx context.Context, providerID int64) (int64, error)
	FirstByProviderID(ctx context.Context, providerID int64) (*model.ProviderModel, error)
}
