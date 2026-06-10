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

func (r *TeamRepo) baseQuery(orgID int64) *gorm.DB {
	q := r.db.Model(&model.Team{})
	if orgID != 0 {
		q = q.Where("org_id = ?", orgID)
	}
	return q
}

func (r *TeamRepo) List(ctx context.Context, orgID int64) ([]model.Team, error) {
	var teams []model.Team
	err := r.baseQuery(orgID).WithContext(ctx).
		Where("status = 1").
		Order("created_at DESC").
		Find(&teams).Error
	if err != nil {
		return teams, err
	}
	r.populateMemberCounts(ctx, teams)
	return teams, nil
}

// ListByUserID returns teams where the user is a member
func (r *TeamRepo) ListByUserID(ctx context.Context, userID int64) ([]model.Team, error) {
	var teams []model.Team
	err := r.db.WithContext(ctx).
		Joins("JOIN team_members ON team_members.team_id = teams.id AND team_members.deleted_at IS NULL").
		Where("team_members.user_id = ? AND teams.status = 1", userID).
		Find(&teams).Error
	if err != nil {
		return teams, err
	}
	r.populateMemberCounts(ctx, teams)
	return teams, nil
}

func (r *TeamRepo) populateMemberCounts(ctx context.Context, teams []model.Team) {
	if len(teams) == 0 {
		return
	}
	ids := make([]int64, len(teams))
	for i, t := range teams {
		ids[i] = t.ID
	}
	type countRow struct {
		TeamID     int64
		MemberCount int64
	}
	var rows []countRow
	r.db.WithContext(ctx).Raw(
		`SELECT tm.team_id AS team_id, COUNT(*) AS member_count
		 FROM team_members tm
		 JOIN users u ON u.id = tm.user_id AND u.deleted_at IS NULL
		 WHERE tm.deleted_at IS NULL AND tm.team_id IN ?
		 GROUP BY tm.team_id`, ids,
	).Scan(&rows)

	countMap := make(map[int64]int64, len(rows))
	for _, row := range rows {
		countMap[row.TeamID] = row.MemberCount
	}
	for i := range teams {
		if cnt, ok := countMap[teams[i].ID]; ok {
			teams[i].MemberCount = cnt
		}
	}
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
	err := r.db.WithContext(ctx).Preload("User.Role").
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
