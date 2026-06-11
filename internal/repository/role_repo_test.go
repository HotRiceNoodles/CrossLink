package repository

import (
	"context"
	"strings"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestDedupActions(t *testing.T) {
	tests := []struct {
		name    string
		actions []string
		want    []string
	}{
		{"empty", nil, nil},
		{"no duplicates", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"with duplicates", []string{"a", "b", "a", "c", "b"}, []string{"a", "b", "c"}},
		{"all same", []string{"x", "x", "x"}, []string{"x"}},
		{"single", []string{"only"}, []string{"only"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dedupActions(tt.actions)
			if len(got) != len(tt.want) {
				t.Fatalf("dedupActions() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("dedupActions()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// --- Integration test helpers ---

func setupRoleTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db = db.Debug()
	if err := db.AutoMigrate(&model.Role{}, &model.RolePermission{}, &model.User{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func seedSystemRoles(t *testing.T, db *gorm.DB) {
	t.Helper()
	roles := []model.Role{
		{Name: model.RoleAdmin, DisplayName: "Super Admin", IsSystem: true},
		{Name: model.RoleMember, DisplayName: "Member", IsSystem: true},
		{Name: model.RoleViewer, DisplayName: "Viewer", IsSystem: true},
	}
	for i := range roles {
		if err := db.Create(&roles[i]).Error; err != nil {
			t.Fatalf("seed role %s: %v", roles[i].Name, err)
		}
	}
}

// communityActions returns valid actions available in Community tier for testing.
func communityActions() []string {
	return []string{"provider:list", "provider:create", "model:list", "model:create"}
}

// --- Integration tests ---

func TestCreateWithPermissions(t *testing.T) {
	db := setupRoleTestDB(t)
	repo := NewRoleRepo(db)
	ctx := context.Background()

	actions := communityActions()
	role, err := repo.CreateWithPermissions(ctx, &RoleCreateInput{
		Name:        "test-role-1",
		DisplayName: "Test Role 1",
		Permissions: actions,
	})
	require.NoError(t, err)
	require.NotNil(t, role)

	assert.NotZero(t, role.ID)
	assert.Equal(t, "test-role-1", role.Name)
	assert.Equal(t, "Test Role 1", role.DisplayName)
	assert.Nil(t, role.OrgID)

	// Verify stored permissions
	stored, err := repo.GetPermissions(ctx, role.ID)
	require.NoError(t, err)
	assert.Equal(t, actions, stored)
}

func TestCreateWithPermissions_InvalidAction(t *testing.T) {
	db := setupRoleTestDB(t)
	repo := NewRoleRepo(db)
	ctx := context.Background()

	_, err := repo.CreateWithPermissions(ctx, &RoleCreateInput{
		Name:        "bad-action-role",
		DisplayName: "Bad Action",
		Permissions: []string{"provider:list", "not:a:real:action"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid permission action")
}

func TestCreateWithPermissions_Dedup(t *testing.T) {
	db := setupRoleTestDB(t)
	repo := NewRoleRepo(db)
	ctx := context.Background()

	// Pass duplicate actions — repo should deduplicate them.
	input := []string{"provider:list", "provider:create", "provider:list", "model:create", "provider:create"}
	role, err := repo.CreateWithPermissions(ctx, &RoleCreateInput{
		Name:        "dedup-role",
		DisplayName: "Dedup Role",
		Permissions: input,
	})
	require.NoError(t, err)

	stored, err := repo.GetPermissions(ctx, role.ID)
	require.NoError(t, err)

	// The 5 input actions with duplicates should reduce to 3 unique ones.
	assert.Equal(t, 3, len(stored))
	for _, a := range stored {
		assert.True(t, model.IsValidAction(a), "stored action %q should be valid", a)
	}
	// Verify no duplicates in stored result.
	seen := make(map[string]bool)
	for _, a := range stored {
		assert.False(t, seen[a], "duplicate action %q found in stored permissions", a)
		seen[a] = true
	}
}

func TestSetPermissions(t *testing.T) {
	db := setupRoleTestDB(t)
	repo := NewRoleRepo(db)
	ctx := context.Background()

	// Create role with initial permissions.
	role, err := repo.CreateWithPermissions(ctx, &RoleCreateInput{
		Name:        "setperm-role",
		DisplayName: "SetPerm Role",
		Permissions: []string{"provider:list", "provider:create"},
	})
	require.NoError(t, err)

	// Replace with a different set of permissions.
	newActions := []string{"model:list", "model:create", "model:update", "model:delete"}
	err = repo.SetPermissions(ctx, role.ID, newActions)
	require.NoError(t, err)

	stored, err := repo.GetPermissions(ctx, role.ID)
	require.NoError(t, err)
	assert.Equal(t, newActions, stored)

	// Also verify old permissions are fully removed.
	for _, old := range []string{"provider:list", "provider:create"} {
		for _, s := range stored {
			assert.NotEqual(t, old, s, "old action %q should have been removed", old)
		}
	}
}

// TestSetPermissions_TierScopedDelete verifies that SetPermissions only deletes
// permissions within the current tier, preserving out-of-tier permissions.
// Under Community tier, SetPermissions must NOT touch Pro-only actions.
func TestSetPermissions_TierScopedDelete(t *testing.T) {
	db := setupRoleTestDB(t)
	repo := NewRoleRepo(db)
	ctx := context.Background()

	// Step 1: Create a role with Community-tier actions.
	role, err := repo.CreateWithPermissions(ctx, &RoleCreateInput{
		Name:        "tier-scoped-role",
		DisplayName: "Tier Scoped",
		Permissions: communityActions(),
	})
	require.NoError(t, err)

	// Step 2: Directly insert a Pro-only permission bypassing tier validation.
	proAction := "debug:list"
	require.NoError(t, db.Create(&model.RolePermission{
		RoleID: role.ID,
		Action: proAction,
	}).Error, "seed Pro-only permission via GORM")

	// Verify setup: role should have community actions + debug:list.
	beforeSet, err := repo.GetPermissions(ctx, role.ID)
	require.NoError(t, err)
	assert.Equal(t, len(communityActions())+1, len(beforeSet), "should have community + pro actions before SetPermissions")

	// Step 3: Call SetPermissions with only Community-tier actions.
	commActions := communityActions()[:2] // e.g. "provider:list", "provider:create"
	err = repo.SetPermissions(ctx, role.ID, commActions)
	require.NoError(t, err)

	// Step 4: Get all stored permissions.
	stored, err := repo.GetPermissions(ctx, role.ID)
	require.NoError(t, err)

	// Step 5: The Pro-only action must survive the tier-scoped delete.
	assert.Contains(t, stored, proAction, "Pro-only action %q should NOT be deleted by Community-tier SetPermissions", proAction)

	// Also verify the requested Community actions are present.
	for _, a := range commActions {
		assert.Contains(t, stored, a, "requested action %q should be in stored permissions", a)
	}

	// Verify the community actions NOT in the replacement set were removed.
	for _, a := range communityActions()[2:] {
		assert.NotContains(t, stored, a, "non-requested community action %q should have been removed", a)
	}
}

func TestDeleteIfNoUsers_CanDelete(t *testing.T) {
	db := setupRoleTestDB(t)
	repo := NewRoleRepo(db)
	ctx := context.Background()

	role, err := repo.CreateWithPermissions(ctx, &RoleCreateInput{
		Name:        "deletable-role",
		DisplayName: "Deletable",
		Permissions: []string{"provider:list"},
	})
	require.NoError(t, err)

	deleted, err := repo.DeleteIfNoUsers(ctx, role.ID)
	require.NoError(t, err)
	assert.True(t, deleted)

	// Verify role is actually gone.
	_, err = repo.GetByID(ctx, role.ID)
	assert.Error(t, err, "expected role to be deleted")
}

func TestDeleteIfNoUsers_HasUsers(t *testing.T) {
	db := setupRoleTestDB(t)
	repo := NewRoleRepo(db)
	ctx := context.Background()

	role, err := repo.CreateWithPermissions(ctx, &RoleCreateInput{
		Name:        "has-users-role",
		DisplayName: "Has Users",
		Permissions: []string{"provider:list"},
	})
	require.NoError(t, err)

	// Assign a user to this role.
	user := &model.User{
		Username:     "testuser1",
		PasswordHash: "hash",
		RoleID:       role.ID,
	}
	require.NoError(t, db.Create(user).Error)

	deleted, err := repo.DeleteIfNoUsers(ctx, role.ID)
	require.Error(t, err)
	assert.False(t, deleted)
	assert.Contains(t, err.Error(), "assigned users")

	// Verify role still exists.
	found, err := repo.GetByID(ctx, role.ID)
	require.NoError(t, err)
	assert.Equal(t, role.ID, found.ID)
}

func TestUserCountsByRoles(t *testing.T) {
	db := setupRoleTestDB(t)
	seedSystemRoles(t, db)
	repo := NewRoleRepo(db)
	ctx := context.Background()

	// Retrieve seeded system role IDs.
	adminRole, err := repo.GetByName(ctx, model.RoleAdmin)
	require.NoError(t, err)
	memberRole, err := repo.GetByName(ctx, model.RoleMember)
	require.NoError(t, err)
	viewerRole, err := repo.GetByName(ctx, model.RoleViewer)
	require.NoError(t, err)

	// Seed users with different roles.
	users := []model.User{
		{Username: "admin1", PasswordHash: "h", RoleID: adminRole.ID},
		{Username: "admin2", PasswordHash: "h", RoleID: adminRole.ID},
		{Username: "member1", PasswordHash: "h", RoleID: memberRole.ID},
		{Username: "viewer1", PasswordHash: "h", RoleID: viewerRole.ID},
		{Username: "viewer2", PasswordHash: "h", RoleID: viewerRole.ID},
		{Username: "viewer3", PasswordHash: "h", RoleID: viewerRole.ID},
	}
	for i := range users {
		require.NoError(t, db.Create(&users[i]).Error, "create user %s", users[i].Username)
	}

	counts, err := repo.UserCountsByRoles(ctx)
	require.NoError(t, err)

	assert.Equal(t, int64(2), counts[adminRole.ID], "admin role user count")
	assert.Equal(t, int64(1), counts[memberRole.ID], "member role user count")
	assert.Equal(t, int64(3), counts[viewerRole.ID], "viewer role user count")
}

func TestReassignAndDelete(t *testing.T) {
	db := setupRoleTestDB(t)
	seedSystemRoles(t, db)
	repo := NewRoleRepo(db)

	// Create custom role and assign users
	role, err := repo.CreateWithPermissions(context.Background(), &RoleCreateInput{
		Name: "to-delete", DisplayName: "ToDelete", Permissions: communityActions(),
	})
	require.NoError(t, err)

	db.Create(&model.User{Username: "u1", PasswordHash: "x", RoleID: role.ID})
	db.Create(&model.User{Username: "u2", PasswordHash: "x", RoleID: role.ID})

	// Get member role ID (target for migration)
	memberRole, err := repo.GetByName(context.Background(), model.RoleMember)
	require.NoError(t, err)

	err = repo.ReassignAndDelete(context.Background(), role.ID, memberRole.ID)
	require.NoError(t, err)

	// Verify users migrated
	var count int64
	db.Model(&model.User{}).Where("role_id = ?", memberRole.ID).Count(&count)
	assert.Equal(t, int64(2), count)

	// Verify role is soft-deleted
	_, err = repo.GetByID(context.Background(), role.ID)
	assert.Error(t, err)
}

func TestGetByIDForOrg(t *testing.T) {
	db := setupRoleTestDB(t)
	repo := NewRoleRepo(db)
	ctx := context.Background()

	orgID := int64(100)

	// Create a global role (org_id = nil).
	globalRole, err := repo.CreateWithPermissions(ctx, &RoleCreateInput{
		Name:        "global-for-org-test",
		DisplayName: "Global",
		Permissions: []string{"provider:list"},
	})
	require.NoError(t, err)
	assert.Nil(t, globalRole.OrgID)

	// Create an org-scoped role (org_id = 100).
	orgRole, err := repo.CreateWithPermissions(ctx, &RoleCreateInput{
		Name:        "org100-role",
		DisplayName: "Org 100 Role",
		OrgID:       &orgID,
		Permissions: []string{"provider:create"},
	})
	require.NoError(t, err)
	assert.Equal(t, &orgID, orgRole.OrgID)

	// org 100 can see its own role AND the global role.
	found, err := repo.GetByIDForOrg(ctx, orgRole.ID, orgID)
	require.NoError(t, err)
	assert.Equal(t, orgRole.ID, found.ID)

	found, err = repo.GetByIDForOrg(ctx, globalRole.ID, orgID)
	require.NoError(t, err)
	assert.Equal(t, globalRole.ID, found.ID)

	// org 999 cannot see org 100's scoped role.
	_, err = repo.GetByIDForOrg(ctx, orgRole.ID, 999)
	assert.Error(t, err, "org 999 should not see org 100's role")
	// GORM returns gorm.ErrRecordNotFound when no match.
	assert.True(t, strings.Contains(err.Error(), "record not found"),
		"expected record not found error, got: %v", err)

	// org 999 CAN still see the global role.
	found, err = repo.GetByIDForOrg(ctx, globalRole.ID, 999)
	require.NoError(t, err)
	assert.Equal(t, globalRole.ID, found.ID)
}