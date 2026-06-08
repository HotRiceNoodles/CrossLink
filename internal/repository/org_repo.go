package repository

import (
	"context"
	"errors"

	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

var ErrOrgNotFound = errors.New("organization not found")

type OrgRepo struct {
	db *gorm.DB
}

func NewOrgRepo(db *gorm.DB) *OrgRepo {
	return &OrgRepo{db: db}
}

func (r *OrgRepo) Create(ctx context.Context, org *model.Organization) error {
	return r.db.WithContext(ctx).Create(org).Error
}

func (r *OrgRepo) GetByID(ctx context.Context, id int64) (*model.Organization, error) {
	var org model.Organization
	if err := r.db.WithContext(ctx).First(&org, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrOrgNotFound
		}
		return nil, err
	}
	return &org, nil
}

func (r *OrgRepo) GetByName(ctx context.Context, name string) (*model.Organization, error) {
	var org model.Organization
	if err := r.db.WithContext(ctx).Where("name = ?", name).First(&org).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrOrgNotFound
		}
		return nil, err
	}
	return &org, nil
}

func (r *OrgRepo) Update(ctx context.Context, org *model.Organization) error {
	return r.db.WithContext(ctx).Save(org).Error
}

func (r *OrgRepo) Delete(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&model.Organization{}, id).Error
}

func (r *OrgRepo) List(ctx context.Context) ([]model.Organization, error) {
	var orgs []model.Organization
	err := r.db.WithContext(ctx).Order("created_at DESC").Find(&orgs).Error
	return orgs, err
}

// ListWithCounts returns all organizations enriched with member, team and key counts.
// Uses batch count queries (3 queries total) instead of N+1.
func (r *OrgRepo) ListWithCounts(ctx context.Context) ([]model.OrgWithCounts, error) {
	orgs, err := r.List(ctx)
	if err != nil {
		return nil, err
	}

	type countRow struct {
		OrgID int64
		Cnt   int64
	}

	// Batch count members per org
	var memberCounts []countRow
	r.db.WithContext(ctx).Model(&model.OrgMember{}).
		Select("org_id, COUNT(*) as cnt").
		Group("org_id").
		Scan(&memberCounts)

	// Batch count keys per org (org_id is nullable)
	var keyCounts []countRow
	r.db.WithContext(ctx).Model(&model.APIKey{}).
		Select("org_id, COUNT(*) as cnt").
		Where("org_id IS NOT NULL").
		Group("org_id").
		Scan(&keyCounts)

	// Batch count teams per org (org_id is nullable)
	var teamCounts []countRow
	r.db.WithContext(ctx).Model(&model.Team{}).
		Select("org_id, COUNT(*) as cnt").
		Where("org_id IS NOT NULL").
		Group("org_id").
		Scan(&teamCounts)

	// Build lookup maps
	memberMap := make(map[int64]int64, len(memberCounts))
	for _, m := range memberCounts {
		memberMap[m.OrgID] = m.Cnt
	}
	keyMap := make(map[int64]int64, len(keyCounts))
	for _, k := range keyCounts {
		keyMap[k.OrgID] = k.Cnt
	}
	teamMap := make(map[int64]int64, len(teamCounts))
	for _, t := range teamCounts {
		teamMap[t.OrgID] = t.Cnt
	}

	// Merge counts into orgs
	result := make([]model.OrgWithCounts, len(orgs))
	for i, org := range orgs {
		result[i] = model.OrgWithCounts{
			Organization: org,
			MemberCount:  memberMap[org.ID],
			TeamCount:    teamMap[org.ID],
			KeyCount:     keyMap[org.ID],
		}
	}
	return result, nil
}

func (r *OrgRepo) Count(ctx context.Context) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&model.Organization{}).Count(&count).Error
	return count, err
}

// Member operations

func (r *OrgRepo) AddMember(ctx context.Context, m *model.OrgMember) error {
	return r.db.WithContext(ctx).Create(m).Error
}

func (r *OrgRepo) RemoveMember(ctx context.Context, orgID, userID int64) error {
	return r.db.WithContext(ctx).
		Where("org_id = ? AND user_id = ?", orgID, userID).
		Delete(&model.OrgMember{}).Error
}

func (r *OrgRepo) GetMember(ctx context.Context, orgID, userID int64) (*model.OrgMember, error) {
	var m model.OrgMember
	err := r.db.WithContext(ctx).
		Where("org_id = ? AND user_id = ?", orgID, userID).
		First(&m).Error
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *OrgRepo) GetMemberByUserID(ctx context.Context, userID int64) (*model.OrgMember, error) {
	var m model.OrgMember
	err := r.db.WithContext(ctx).
		Where("user_id = ?", userID).
		First(&m).Error
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *OrgRepo) ListMembers(ctx context.Context, orgID int64) ([]model.OrgMember, error) {
	var members []model.OrgMember
	err := r.db.WithContext(ctx).
		Where("org_id = ?", orgID).
		Order("joined_at ASC").
		Find(&members).Error
	return members, err
}

func (r *OrgRepo) UpdateMemberRole(ctx context.Context, orgID, userID int64, role string) error {
	return r.db.WithContext(ctx).Model(&model.OrgMember{}).
		Where("org_id = ? AND user_id = ?", orgID, userID).
		Update("role", role).Error
}
