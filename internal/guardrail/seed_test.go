package guardrail

import (
	"testing"

	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupSeedDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	// Create the table with sqlite-compatible DDL. The production schema is a
	// SQL migration (golang-migrate), so GORM AutoMigrate is never used at
	// runtime; calling it here would choke on the model's gorm:"type:jsonb" tag,
	// which is invalid sqlite syntax. Mirrors the production column types.
	require.NoError(t, db.Exec(`CREATE TABLE guardrail_rules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		org_id INTEGER,
		name TEXT NOT NULL,
		type TEXT NOT NULL,
		direction TEXT NOT NULL,
		enabled INTEGER DEFAULT 1,
		config TEXT NOT NULL,
		severity TEXT DEFAULT 'medium',
		action TEXT DEFAULT 'block',
		model_filter TEXT,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`).Error)
	return db
}

func TestSeedDefaultRules_InsertsOneCredRule(t *testing.T) {
	db := setupSeedDB(t)

	require.NoError(t, SeedDefaultRules(db))

	var rules []GuardrailRule
	db.Find(&rules)
	require.Len(t, rules, 1, "exactly one rule seeded")

	r := rules[0]
	assert.Equal(t, "credential_detection", r.Type)
	assert.Equal(t, "both", r.Direction)
	assert.Equal(t, "log", r.Action, "default must be log, not block (false-positive safety)")
	assert.True(t, r.Enabled)
	assert.Equal(t, "medium", r.Severity)
	assert.Nil(t, r.OrgID, "global default (NULL org)")
	assert.Nil(t, r.ModelFilter, "applies to all models")
	assert.Equal(t, "Default - Credential Leak Detection", r.Name)

	// Config must be the empty sentinel: the seed does NOT hardcode categories.
	// The engine derives all builtins from an empty config (credential_detection.go),
	// and the engine's own tests cover that derivation — the seed test only
	// verifies that the seed produced the empty sentinel.
	assert.Equal(t, `{}`, string(r.Config), "seed config must be empty so the engine derives all builtin categories")
}

func TestSeedDefaultRules_Idempotent(t *testing.T) {
	db := setupSeedDB(t)

	require.NoError(t, SeedDefaultRules(db))
	require.NoError(t, SeedDefaultRules(db)) // second call

	var n int64
	db.Model(&GuardrailRule{}).Where("type = ?", "credential_detection").Count(&n)
	assert.Equal(t, int64(1), n, "second call must not duplicate the rule")
}

func TestSeedDefaultRules_NoDuplicateIfPreexisting(t *testing.T) {
	db := setupSeedDB(t)

	// Simulate an operator-created credential_detection rule.
	preexisting := GuardrailRule{
		Name:      "ops custom cred rule",
		Type:      "credential_detection",
		Direction: "request",
		Enabled:   true,
		Action:    "block",
		Severity:  "high",
		Config:    datatypes.JSON(`{"categories":["jwt"]}`),
	}
	require.NoError(t, db.Create(&preexisting).Error)

	require.NoError(t, SeedDefaultRules(db))

	var n int64
	db.Model(&GuardrailRule{}).Where("type = ?", "credential_detection").Count(&n)
	assert.Equal(t, int64(1), n, "must not seed default when any credential_detection rule already exists")

	var got GuardrailRule
	require.NoError(t, db.Where("type = ?", "credential_detection").First(&got).Error)
	assert.Equal(t, "ops custom cred rule", got.Name, "operator's rule left untouched")
}
