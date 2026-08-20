package repository

import (
	"gorm.io/gorm"
)

// Repos holds all repository instances used by the application.
type Repos struct {
	ProviderModelRepo     *ProviderModelRepo
	ProviderModelCRUDRepo *ProviderModelCRUDRepo
	APIKeyRepo            *APIKeyRepo
	APIKeyHashRepo        *APIKeyHashRepo
	ProviderRepo          *ProviderRepo
	UsageLogRepo          *UsageLogRepo
	UserRepo              *UserRepo
	TeamRepo              *TeamRepo
	RoleRepo              *RoleRepo
	BudgetAlertRepo       *BudgetAlertRepo
	OrgRepo               *OrgRepo
	ErrorRuleRepo         *ErrorRuleRepo
	PatTokenRepo          *PatTokenRepo
	DataLensStore         MetricsStore // interface — set in FullSetup where dialect is available
	DataLensRepo          *DataLensRepository
}

// ProvideRepos constructs all repository instances.
func ProvideRepos(db *gorm.DB) *Repos {
	return &Repos{
		ProviderModelRepo:     NewProviderModelRepo(db),
		ProviderModelCRUDRepo: NewProviderModelCRUDRepo(db),
		APIKeyRepo:            NewAPIKeyRepo(db),
		APIKeyHashRepo:        NewAPIKeyHashRepo(db),
		ProviderRepo:          NewProviderRepo(db),
		UsageLogRepo:          NewUsageLogRepo(db),
		UserRepo:              NewUserRepo(db),
		TeamRepo:              NewTeamRepo(db),
		RoleRepo:              NewRoleRepo(db),
		BudgetAlertRepo:       NewBudgetAlertRepo(db),
		OrgRepo:               NewOrgRepo(db),
		ErrorRuleRepo:         NewErrorRuleRepo(db),
		PatTokenRepo:          NewPatTokenRepo(db),
		DataLensRepo:          NewDataLensRepository(db),
	}
}
