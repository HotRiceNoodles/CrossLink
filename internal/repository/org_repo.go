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
