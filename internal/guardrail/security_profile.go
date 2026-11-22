package guardrail

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// ProfileContent 只表达应用长期策略，不能作为当前用户授权。
type ProfileContent struct {
	Name         string `json:"name" binding:"required,max=128"`
	Purpose      string `json:"purpose" binding:"required,max=4000"`
	AllowedTasks string `json:"allowed_tasks" binding:"max=4000"`
	Forbidden    string `json:"forbidden" binding:"max=4000"`
	DataBoundary string `json:"data_boundary" binding:"max=4000"`
}

type SecurityProfile struct {
	ID             int64 `json:"id"`
	OrgID          int64 `json:"org_id"`
	Revision       int64 `json:"revision"`
	Published      int64 `json:"published"`
	ProfileContent `gorm:"embedded"`
}

type SecurityProfileVersion struct {
	ID             int64     `json:"-"`
	ProfileID      int64     `json:"profile_id"`
	Version        int64     `json:"version"`
	Actor          string    `json:"actor"`
	CreatedAt      time.Time `json:"created_at"`
	ProfileContent `gorm:"embedded"`
}

type SecurityProfileBinding struct {
	APIKeyID  int64 `gorm:"primaryKey;autoIncrement:false" json:"api_key_id"`
	ProfileID int64 `json:"profile_id"`
}

func publishedProfile(ctx context.Context, db *gorm.DB, org, key int64) (*SecurityProfileVersion, error) {
	if key == 0 || org == 0 {
		return nil, nil
	}
	var version SecurityProfileVersion
	err := db.WithContext(ctx).Model(&version).Select("security_profile_versions.*").
		Joins("JOIN security_profiles p ON p.id = security_profile_versions.profile_id AND p.published = security_profile_versions.version").
		Joins("JOIN security_profile_bindings b ON b.profile_id = p.id").
		Joins("JOIN api_keys k ON k.id = b.api_key_id AND k.org_id = p.org_id").
		Where("k.id = ? AND p.org_id = ? AND k.status = 1 AND k.deleted_at IS NULL", key, org).Take(&version).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &version, err
}
