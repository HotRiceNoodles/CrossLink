package guardapp

import (
	"fmt"
	"log/slog"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/model"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const bcryptCost = 12 // 与 internal/admin 同强度

// systemRoles 与 postgres 000012_configurable_rbac 的种子一致。sqlite/mysql
// 的压缩基线只建表不种角色（已实测），guard 必须自己补齐——不依赖网关侧
// 未导出的 seed 逻辑。
var systemRoles = []model.Role{
	{Name: model.RoleAdmin, DisplayName: "管理员", IsSystem: true},
	{Name: model.RoleMember, DisplayName: "成员", IsSystem: true},
	{Name: model.RoleViewer, DisplayName: "观察者", IsSystem: true},
}

// Bootstrap 首启动 seed：系统角色、默认 org（guard 语义下的初始租户）、
// 平台管理员（复用 users 表与 bcrypt 机制，仅首次创建）。
//
// 与网关 buildAuth 的差异：
//   - 密码哈希只落 users 表，不写 system_settings 的 admin_password_hash
//     （那是网关 legacy 登录路径的兼容机制，guard 无此历史包袱）；
//   - 不做网关的 org_id 历史回填——guard 是全新库语义，无存量行；
//   - JWT secret 强度检查推迟到 Step 8 接入登录时一并做。
func Bootstrap(db *gorm.DB, cfg *config.AdminConfig) error {
	if cfg.IsDefaultPassword() {
		return fmt.Errorf("insecure default admin password detected: change admin.password in config or set CL_ADMIN_PASSWORD env var")
	}
	if err := seedSystemRoles(db); err != nil {
		return fmt.Errorf("seed system roles: %w", err)
	}
	orgID, err := ensureDefaultOrg(db)
	if err != nil {
		return fmt.Errorf("ensure default org: %w", err)
	}
	if err := ensureAdminUser(db, cfg, orgID); err != nil {
		return fmt.Errorf("ensure admin user: %w", err)
	}
	if err := syncAdminPermissions(db); err != nil {
		return fmt.Errorf("sync admin permissions: %w", err)
	}
	return nil
}

func seedSystemRoles(db *gorm.DB) error {
	for i := range systemRoles {
		var count int64
		if err := db.Model(&model.Role{}).Where("name = ?", systemRoles[i].Name).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		if err := db.Create(&systemRoles[i]).Error; err != nil {
			return err
		}
		slog.Info("seeded system role", "name", systemRoles[i].Name)
	}
	return nil
}

// ensureDefaultOrg 创建 default org（若缺）。guard 中 org 语义是"客户本身"
// （设计 DD-7）——default 即 M1 的初始租户，多租户管理 CRUD 属 M2。
func ensureDefaultOrg(db *gorm.DB) (int64, error) {
	var ids []int64
	if err := db.Model(&model.Organization{}).
		Where("name = ? AND deleted_at IS NULL", "default").
		Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	if len(ids) > 0 {
		return ids[0], nil
	}
	org := model.Organization{Name: "default", DisplayName: "Default Organization", Status: 1}
	if err := db.Create(&org).Error; err != nil {
		return 0, err
	}
	slog.Info("created default organization", "id", org.ID)
	return org.ID, nil
}

// ensureAdminUser 仅在无 admin 角色用户时创建；配置密码只用于首次 seed，
// 之后改配置不会覆盖已有哈希（与网关 ensureAdminUser 语义一致）。
func ensureAdminUser(db *gorm.DB, cfg *config.AdminConfig, orgID int64) error {
	adminRoleID, err := roleIDByName(db, model.RoleAdmin)
	if err != nil {
		return fmt.Errorf("admin role missing: %w", err)
	}
	var count int64
	if err := db.Model(&model.User{}).
		Where("role_id = ? AND deleted_at IS NULL", adminRoleID).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(cfg.Password), bcryptCost)
	if err != nil {
		return err
	}
	user := &model.User{
		Username:            cfg.Username,
		PasswordHash:        string(hash),
		DisplayName:         "Administrator",
		RoleID:              adminRoleID,
		Status:              1,
		OrgID:               &orgID,
		ForcePasswordChange: true,
	}
	// Select 限定插入列：model.User 带 overlay 商业字段（SSOProviderID 等），
	// Community 迁移链的 users 表没有这些列，全字段 INSERT 会撞列。
	if err := db.Select("Username", "PasswordHash", "DisplayName", "RoleID",
		"Status", "OrgID", "ForcePasswordChange", "CreatedAt", "UpdatedAt").
		Create(user).Error; err != nil {
		return err
	}
	slog.Info("seeded default admin user", "username", cfg.Username, "org_id", orgID)
	return nil
}

// roleIDByName 按名取角色 ID。共享表（roles/users/organizations）的时间列
// 在 sqlite 压缩基线里声明为 TEXT，glebarez 驱动不做 TEXT→time.Time 转换，
// 整行 First 扫描会报错——因此这里只 Pluck id，绝不扫时间列（Step 8 的
// 登录查询沿用同一约束）。
func roleIDByName(db *gorm.DB, name string) (int64, error) {
	var ids []int64
	if err := db.Model(&model.Role{}).
		Where("name = ? AND deleted_at IS NULL", name).
		Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, gorm.ErrRecordNotFound
	}
	return ids[0], nil
}

// syncAdminPermissions 给 admin 角色补全 ValidActions（网关侧
// syncAdminPermissions 的本地实现；guard 独有动作位在 Step 8 随管理 API
// 一并加入 model.ValidActions）。role_permissions 表无时间列，可整行扫描。
func syncAdminPermissions(db *gorm.DB) error {
	adminRoleID, err := roleIDByName(db, model.RoleAdmin)
	if err != nil {
		return fmt.Errorf("admin role missing: %w", err)
	}
	var existing []model.RolePermission
	if err := db.Where("role_id = ?", adminRoleID).Find(&existing).Error; err != nil {
		return err
	}
	have := make(map[string]bool, len(existing))
	for _, p := range existing {
		have[p.Action] = true
	}
	for action := range model.ValidActions {
		if have[action] {
			continue
		}
		if err := db.Create(&model.RolePermission{RoleID: adminRoleID, Action: action}).Error; err != nil {
			return err
		}
	}
	return nil
}
