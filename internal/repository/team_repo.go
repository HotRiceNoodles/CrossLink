package repository

import (
	"context"
	"errors"

	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

var ErrTeamNotFound = errors.New("team not found")

type TeamRepo struct {
	db *gorm.DB
}

func NewTeamRepo(db *gorm.DB) *TeamRepo {
	return &TeamRepo{db: db}
}

func (r *TeamRepo) Create(ctx context.Context, t *model.Team) error {
	return r.db.WithContext(ctx).Create(t).Error
}

func (r *TeamRepo) GetByID(ctx context.Context, id int64) (*model.Team, error) {
	var t model.Team
	if err := r.db.WithContext(ctx).First(&t, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTeamNotFound
		}
		return nil, err
	}
	return &t, nil
}

func (r *TeamRepo) Update(ctx context.Context, t *model.Team) error {
	return r.db.WithContext(ctx).Save(t).Error
}

func (r *TeamRepo) Delete(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&model.Team{}, id).Error
}

func (r *TeamRepo) List(ctx context.Context) ([]model.Team, error) {
	var teams []model.Team
	err := r.db.WithContext(ctx).Where("status = 1").Order("created_at DESC").Find(&teams).Error
	return teams, err
}

// ListByUserID returns teams where the user is a member
func (r *TeamRepo) ListByUserID(ctx context.Context, userID int64) ([]model.Team, error) {
	var teams []model.Team
	err := r.db.WithContext(ctx).
		Joins("JOIN team_members ON team_members.team_id = teams.id").
		Where("team_members.user_id = ? AND teams.status = 1", userID).
		Find(&teams).Error
	return teams, err
}

// Member operations
func (r *TeamRepo) AddMember(ctx context.Context, tm *model.TeamMember) error {
	return r.db.WithContext(ctx).Create(tm).Error
}

func (r *TeamRepo) RemoveMember(ctx context.Context, teamID, userID int64) error {
	return r.db.WithContext(ctx).
		Where("team_id = ? AND user_id = ?", teamID, userID).
		Delete(&model.TeamMember{}).Error
}

func (r *TeamRepo) ListMembers(ctx context.Context, teamID int64) ([]model.TeamMember, error) {
	var members []model.TeamMember
	err := r.db.WithContext(ctx).Preload("User").
		Where("team_id = ?", teamID).
		Order("joined_at ASC").
		Find(&members).Error
	return members, err
}

func (r *TeamRepo) GetMember(ctx context.Context, teamID, userID int64) (*model.TeamMember, error) {
	var m model.TeamMember
	err := r.db.WithContext(ctx).
		Where("team_id = ? AND user_id = ?", teamID, userID).
		First(&m).Error
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *TeamRepo) UpdateMemberRole(ctx context.Context, teamID, userID int64, role string) error {
	return r.db.WithContext(ctx).Model(&model.TeamMember{}).
		Where("team_id = ? AND user_id = ?", teamID, userID).
		Update("role", role).Error
}
