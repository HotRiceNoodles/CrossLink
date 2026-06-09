package repository

import (
	"context"
	"errors"

	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

var ErrUserNotFound = errors.New("user not found")

type UserRepo struct {
	db *gorm.DB
}

func NewUserRepo(db *gorm.DB) *UserRepo {
	return &UserRepo{db: db}
}

func (r *UserRepo) GetByUsername(ctx context.Context, username string) (*model.User, error) {
	var u model.User
	if err := r.db.WithContext(ctx).Preload("Role").Where("username = ? AND status = 1", username).First(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &u, nil
}

func (r *UserRepo) GetByID(ctx context.Context, id int64) (*model.User, error) {
	var u model.User
	if err := r.db.WithContext(ctx).Preload("Role").First(&u, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &u, nil
}

func (r *UserRepo) Create(ctx context.Context, u *model.User) error {
	return r.db.WithContext(ctx).Create(u).Error
}

func (r *UserRepo) Update(ctx context.Context, u *model.User) error {
	return r.db.WithContext(ctx).Save(u).Error
}

func (r *UserRepo) List(ctx context.Context) ([]model.User, error) {
	var users []model.User
	err := r.db.WithContext(ctx).Preload("Role").Where("status = 1").Order("created_at DESC").Find(&users).Error
	if err != nil {
		return users, err
	}

	// Populate team info via secondary query
	if len(users) > 0 {
		ids := make([]int64, len(users))
		for i, u := range users {
			ids[i] = u.ID
		}
		type teamRow struct {
			UserID   int64
			TeamID   int64
			TeamName string
			TeamRole string
		}
		var rows []teamRow
		r.db.WithContext(ctx).Raw(
			`SELECT tm.user_id, tm.team_id, t.display_name AS team_name, tm.role AS team_role
			 FROM team_members tm
			 JOIN teams t ON t.id = tm.team_id AND t.deleted_at IS NULL
			 WHERE tm.deleted_at IS NULL AND tm.user_id IN ?`,
			ids,
		).Scan(&rows)

		teamMap := make(map[int64]*teamRow, len(rows))
		for i := range rows {
			teamMap[rows[i].UserID] = &rows[i]
		}
		for i := range users {
			if tr, ok := teamMap[users[i].ID]; ok {
				users[i].TeamID = &tr.TeamID
				users[i].TeamName = &tr.TeamName
				users[i].TeamRole = &tr.TeamRole
			}
		}
	}

	return users, nil
}

func (r *UserRepo) Delete(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&model.User{}, id).Error
}

func (r *UserRepo) CountByRoleName(ctx context.Context, roleName string) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&model.User{}).
		Joins("JOIN roles ON roles.id = users.role_id").
		Where("roles.name = ? AND users.status = 1", roleName).
		Count(&count).Error
	return count, err
}
