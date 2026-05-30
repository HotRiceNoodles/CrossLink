package repository

import (
	"context"
	"fmt"

	"github.com/crosslink/internal/license"
	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

type RoleRepo struct {
	db *gorm.DB
}

func NewRoleRepo(db *gorm.DB) *RoleRepo {
	return &RoleRepo{db: db}
}

func (r *RoleRepo) baseQuery(orgID int64) *gorm.DB {
	q := r.db.Model(&model.Role{})
	if orgID != 0 {
		q = q.Where("org_id = ? OR org_id IS NULL", orgID)
	}
	return q
}

func dedupActions(actions []string) []string {
	seen := make(map[string]bool, len(actions))
	deduped := actions[:0]
	for _, a := range actions {
		if !seen[a] {
			seen[a] = true
			deduped = append(deduped, a)
		}
	}
	return deduped
}

func (r *RoleRepo) List(ctx context.Context, orgID int64) ([]model.Role, error) {
	var roles []model.Role
	err := r.baseQuery(orgID).WithContext(ctx).Order("is_system DESC, created_at ASC").Find(&roles).Error
	return roles, err
}

func (r *RoleRepo) GetByID(ctx context.Context, id int64) (*model.Role, error) {
	var role model.Role
	err := r.db.WithContext(ctx).First(&role, id).Error
	return &role, err
}

func (r *RoleRepo) GetByIDForOrg(ctx context.Context, id int64, orgID int64) (*model.Role, error) {
	var role model.Role
	err := r.baseQuery(orgID).WithContext(ctx).Where("id = ?", id).First(&role).Error
	return &role, err
}

func (r *RoleRepo) GetByName(ctx context.Context, name string) (*model.Role, error) {
	var role model.Role
	err := r.db.WithContext(ctx).Where("name = ?", name).First(&role).Error
	return &role, err
}

func (r *RoleRepo) Create(ctx context.Context, role *model.Role) error {
	return r.db.WithContext(ctx).Create(role).Error
}

func (r *RoleRepo) Update(ctx context.Context, role *model.Role) error {
	return r.db.WithContext(ctx).Model(role).Update("display_name", role.DisplayName).Error
}

func (r *RoleRepo) Delete(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&model.Role{}, id).Error
}

// DeleteIfNoUsers atomically deletes a role only if no users reference it.
// Returns true if deleted, false if users exist.
func (r *RoleRepo) DeleteIfNoUsers(ctx context.Context, id int64) (bool, error) {
	var deleted bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var count int64
		tx.Model(&model.User{}).Where("role_id = ?", id).Count(&count)
		if count > 0 {
			return fmt.Errorf("role has %d assigned users", count)
		}
		result := tx.Delete(&model.Role{}, id)
		if result.Error != nil {
			return result.Error
		}
		deleted = result.RowsAffected == 1
		return nil
	})
	return deleted, err
}

// UserCountsByRoles returns user counts grouped by role_id in a single query.
func (r *RoleRepo) UserCountsByRoles(ctx context.Context) (map[int64]int64, error) {
	type row struct {
		RoleID int64
		Count  int64
	}
	var rows []row
	err := r.db.WithContext(ctx).Model(&model.User{}).
		Select("role_id, COUNT(*) as count").
		Group("role_id").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	result := make(map[int64]int64, len(rows))
	for _, r := range rows {
		result[r.RoleID] = r.Count
	}
	return result, nil
}

type RoleCreateInput struct {
	Name        string
	DisplayName string
	OrgID       *int64
	Permissions []string
}

func (r *RoleRepo) CreateWithPermissions(ctx context.Context, input *RoleCreateInput) (*model.Role, error) {
	// Validate and deduplicate permissions
	input.Permissions = dedupActions(input.Permissions)
	for _, a := range input.Permissions {
		if !model.IsValidAction(a) {
			return nil, fmt.Errorf("invalid permission action: %s", a)
		}
		if !license.TierAllowsAction(a) {
			return nil, fmt.Errorf("action %s is not available in current tier", a)
		}
	}

	role := &model.Role{
		Name:        input.Name,
		DisplayName: input.DisplayName,
		OrgID:       input.OrgID,
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(role).Error; err != nil {
			return err
		}
		if len(input.Permissions) > 0 {
			perms := make([]model.RolePermission, len(input.Permissions))
			for i, a := range input.Permissions {
				perms[i] = model.RolePermission{RoleID: role.ID, Action: a}
			}
			if err := tx.Create(&perms).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return role, err
}

func (r *RoleRepo) GetPermissions(ctx context.Context, roleID int64) ([]string, error) {
	var perms []model.RolePermission
	err := r.db.WithContext(ctx).Where("role_id = ?", roleID).Find(&perms).Error
	if err != nil {
		return nil, err
	}
	actions := make([]string, len(perms))
	for i, p := range perms {
		actions[i] = p.Action
	}
	return actions, nil
}

func (r *RoleRepo) SetPermissions(ctx context.Context, roleID int64, actions []string) error {
	actions = dedupActions(actions)
	for _, a := range actions {
		if !model.IsValidAction(a) {
			return fmt.Errorf("invalid permission action: %s", a)
		}
		if !license.TierAllowsAction(a) {
			return fmt.Errorf("action %s is not available in current tier", a)
		}
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		tierActions := license.TierAllowedActions()
		if err := tx.Where("role_id = ? AND action IN ?", roleID, tierActions).
			Delete(&model.RolePermission{}).Error; err != nil {
			return err
		}
		if len(actions) == 0 {
			return nil
		}
		perms := make([]model.RolePermission, len(actions))
		for i, a := range actions {
			perms[i] = model.RolePermission{RoleID: roleID, Action: a}
		}
		return tx.Create(&perms).Error
	})
}

func (r *RoleRepo) UserCountByRole(ctx context.Context, roleID int64) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&model.User{}).Where("role_id = ?", roleID).Count(&count).Error
	return count, err
}

func (r *RoleRepo) GetAllPermissions(ctx context.Context) (map[int64][]string, error) {
	var perms []model.RolePermission
	err := r.db.WithContext(ctx).Find(&perms).Error
	if err != nil {
		return nil, err
	}
	result := make(map[int64][]string)
	for _, p := range perms {
		result[p.RoleID] = append(result[p.RoleID], p.Action)
	}
	return result, nil
}
