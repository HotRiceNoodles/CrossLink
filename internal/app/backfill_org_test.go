package app

import (
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// F-9 (supervisor source-lead, verified live): the boot-time backfill used to
// stamp EVERY usage_logs row with org_id IS NULL to the default org on EVERY
// start. In the ladder campaign two boots flipped 381k storm rows and 193k
// replayed rows to org 1, mis-attributing unauthenticated/config-key traffic
// (whose org observation is legitimately NULL) and corrupting per-tenant
// reporting. Event logs must NEVER be auto-backfilled; org-scoped entities
// keep their small-table backfill.
func setupBackfillDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	for _, m := range []any{&model.Organization{}, &model.Team{}, &model.User{}, &model.Role{},
		&model.APIKey{}, &model.Provider{}, &model.OrgMember{}, &model.BudgetAlert{},
		&model.UsageLog{}} {
		if err := db.AutoMigrate(m); err != nil {
			t.Fatalf("migrate %T: %v", m, err)
		}
	}
	return db
}

func TestBootBackfill_NeverStampsEventLogNulls(t *testing.T) {
	db := setupBackfillDB(t)

	// Legitimate NULL-org events: unauthenticated/config-key traffic observed
	// with no tenant, exactly what the WAL spool replay writes.
	rows := []model.UsageLog{
		{RouteType: "openai", ModelRequested: "mock", StatusCode: 503, ErrorType: "resolve_error"},
		{RouteType: "openai", ModelRequested: "mock", StatusCode: 200},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed usage_log: %v", err)
		}
	}
	if err := db.Exec("UPDATE usage_logs SET org_id = NULL WHERE id IS NOT NULL").Error; err != nil {
		t.Logf("usage seed: %v", err)
	}

	// Two boots (restart semantics).
	ensureDefaultOrganization(db)
	ensureDefaultOrganization(db)

	var stamped int64
	db.Model(&model.UsageLog{}).Where("org_id IS NOT NULL").Count(&stamped)
	if stamped != 0 {
		t.Fatalf("usage_logs NULL-org events were force-stamped (org set on %d rows) — event logs must never be auto-backfilled", stamped)
	}

	// Org-scoped ENTITIES still get the legacy-schema backfill: a NULL-org
	// team becomes a member of the default org.
	team := model.Team{Name: "legacy-team"}
	if err := db.Create(&team).Error; err != nil {
		t.Fatalf("seed team: %v", err)
	}
	db.Exec("UPDATE teams SET org_id = NULL WHERE id = ?", team.ID)
	ensureDefaultOrganization(db)
	var got model.Team
	if err := db.First(&got, team.ID).Error; err != nil {
		t.Fatalf("reload team: %v", err)
	}
	var def model.Organization
	db.Where("name = ?", "default").First(&def)
	if def.ID == 0 || got.OrgID == nil || *got.OrgID != def.ID {
		t.Fatalf("NULL-org entity should be backfilled to the default org: team.org=%v default=%d", got.OrgID, def.ID)
	}
}

// Restart + WAL-replay attribution consistency: rows written NULL by the
// replay path keep NULL across subsequent boots (the exact scenario that
// flipped 193k rows live: replay writes NULL at 11:03, next boot at 11:27
// stamped them org 1).
func TestBootBackfill_ReplayedRowsKeepNullOrgAcrossRestarts(t *testing.T) {
	db := setupBackfillDB(t)
	ensureDefaultOrganization(db) // boot 1: default org exists

	// Replay-path row: canonical NULL for OrgID=0 (usage_service.go:213).
	evt := "replay-event-1"
	row := model.UsageLog{RouteType: "openai", ModelRequested: "mock", StatusCode: 503, EventID: &evt}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	ensureDefaultOrganization(db) // boot 2 (restart)
	ensureDefaultOrganization(db) // boot 3

	var after model.UsageLog
	if err := db.First(&after, row.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.OrgID != nil {
		t.Fatalf("replayed NULL-org event flipped to org %d by restart backfill — attribution must be stable across restarts", *after.OrgID)
	}
}
