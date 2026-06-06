package model

import "time"

// SSOProvider stores the OAuth2/OIDC SSO configuration.
// Single-provider design: at most one enabled row in production.
type SSOProvider struct {
	ID              int64   `gorm:"primaryKey" json:"id"`
	Name            string  `gorm:"size:64;not null" json:"name"`
	Type            string  `gorm:"size:16;not null;default:'oidc'" json:"type"`
	Issuer          string  `gorm:"size:512;not null" json:"issuer"`
	ClientID        string  `gorm:"size:256;not null" json:"client_id"`
	ClientSecret    string  `gorm:"not null" json:"-"`                          // AES-encrypted, never serialized
	Scopes          string  `gorm:"size:256;default:'openid profile email'" json:"scopes"`
	DefaultOrgID    *int64  `gorm:"index" json:"default_org_id,omitempty"`
	DefaultRoleID   *int64  `gorm:"index" json:"default_role_id,omitempty"`
	AutoCreate      bool    `gorm:"not null;default:true" json:"auto_create"`
	RedirectBaseURL *string `gorm:"size:512" json:"redirect_base_url,omitempty"`
	// Manual endpoint overrides (skip OIDC discovery when set)
	AuthURL     *string `gorm:"size:512" json:"auth_url,omitempty"`
	TokenURL    *string `gorm:"size:512" json:"token_url,omitempty"`
	UserinfoURL *string `gorm:"size:512" json:"userinfo_url,omitempty"`
	Enabled     bool    `gorm:"not null;default:true" json:"enabled"`
	CreatedAt   time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt   time.Time `gorm:"not null" json:"updated_at"`
}

func (SSOProvider) TableName() string { return "sso_providers" }
