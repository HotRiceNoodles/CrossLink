package model

import "testing"

func TestUser_TableName(t *testing.T) {
	u := User{}
	if u.TableName() != "users" {
		t.Errorf("expected 'users', got %s", u.TableName())
	}
}

func TestTeam_TableName(t *testing.T) {
	tm := Team{}
	if tm.TableName() != "teams" {
		t.Errorf("expected 'teams', got %s", tm.TableName())
	}
}

func TestTeamMember_TableName(t *testing.T) {
	tm := TeamMember{}
	if tm.TableName() != "team_members" {
		t.Errorf("expected 'team_members', got %s", tm.TableName())
	}
}

func TestRoleConstants(t *testing.T) {
	if RoleAdmin != "admin" {
		t.Errorf("expected 'admin', got %s", RoleAdmin)
	}
	if RoleMember != "member" {
		t.Errorf("expected 'member', got %s", RoleMember)
	}
	if RoleViewer != "viewer" {
		t.Errorf("expected 'viewer', got %s", RoleViewer)
	}
}
