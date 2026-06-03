package dialect

import "testing"

func TestDateTrunc(t *testing.T) {
	pg := &PostgresDialect{}
	sqlite := &SQLiteDialect{}
	mysql := &MySQLDialect{}

	tests := []struct {
		name        string
		granularity string
		column      string
		pgWant      string
		sqliteWant  string
		mysqlWant   string
	}{
		{
			name:        "day granularity",
			granularity: "day",
			column:      "created_at",
			pgWant:      "date_trunc('day', created_at)",
			sqliteWant:  "DATE(created_at)",
			mysqlWant:   "DATE(created_at)",
		},
		{
			name:        "hour granularity",
			granularity: "hour",
			column:      "created_at",
			pgWant:      "date_trunc('hour', created_at)",
			sqliteWant:  "strftime('%Y-%m-%d %H:00:00', created_at)",
			mysqlWant:   "DATE_FORMAT(created_at, '%Y-%m-%d %H:00:00')",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pg.DateTrunc(tt.granularity, tt.column); got != tt.pgWant {
				t.Errorf("PostgresDialect.DateTrunc() = %q, want %q", got, tt.pgWant)
			}
			if got := sqlite.DateTrunc(tt.granularity, tt.column); got != tt.sqliteWant {
				t.Errorf("SQLiteDialect.DateTrunc() = %q, want %q", got, tt.sqliteWant)
			}
			if got := mysql.DateTrunc(tt.granularity, tt.column); got != tt.mysqlWant {
				t.Errorf("MySQLDialect.DateTrunc() = %q, want %q", got, tt.mysqlWant)
			}
		})
	}
}

func TestDateFormat(t *testing.T) {
	pg := &PostgresDialect{}
	sqlite := &SQLiteDialect{}
	mysql := &MySQLDialect{}

	tests := []struct {
		name       string
		column     string
		format     string
		pgWant     string
		sqliteWant string
		mysqlWant  string
	}{
		{
			name:       "year-month format",
			column:     "created_at",
			format:     "%Y-%m",
			pgWant:     "TO_CHAR(created_at, 'YYYY-MM')",
			sqliteWant: "strftime('%Y-%m', created_at)",
			mysqlWant:  "DATE_FORMAT(created_at, '%Y-%m')",
		},
		{
			name:       "full date format",
			column:     "ts",
			format:     "%Y-%m-%d",
			pgWant:     "TO_CHAR(ts, 'YYYY-MM-DD')",
			sqliteWant: "strftime('%Y-%m-%d', ts)",
			mysqlWant:  "DATE_FORMAT(ts, '%Y-%m-%d')",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pg.DateFormat(tt.column, tt.format); got != tt.pgWant {
				t.Errorf("PostgresDialect.DateFormat() = %q, want %q", got, tt.pgWant)
			}
			if got := sqlite.DateFormat(tt.column, tt.format); got != tt.sqliteWant {
				t.Errorf("SQLiteDialect.DateFormat() = %q, want %q", got, tt.sqliteWant)
			}
			if got := mysql.DateFormat(tt.column, tt.format); got != tt.mysqlWant {
				t.Errorf("MySQLDialect.DateFormat() = %q, want %q", got, tt.mysqlWant)
			}
		})
	}
}

func TestILike(t *testing.T) {
	pg := &PostgresDialect{}
	sqlite := &SQLiteDialect{}
	mysql := &MySQLDialect{}

	pgGot := pg.ILike("name", "?")
	if pgGot != "name ILIKE ?" {
		t.Errorf("PostgresDialect.ILike() = %q, want %q", pgGot, "name ILIKE ?")
	}

	sqliteGot := sqlite.ILike("name", "?")
	if sqliteGot != "name LIKE ?" {
		t.Errorf("SQLiteDialect.ILike() = %q, want %q", sqliteGot, "name LIKE ?")
	}

	mysqlGot := mysql.ILike("name", "?")
	if mysqlGot != "name LIKE ?" {
		t.Errorf("MySQLDialect.ILike() = %q, want %q", mysqlGot, "name LIKE ?")
	}
}

func TestJSONMergePatch(t *testing.T) {
	pg := &PostgresDialect{}
	sqlite := &SQLiteDialect{}
	mysql := &MySQLDialect{}

	pgGot := pg.JSONMergePatch("extra_config", "'{\"key\":\"val\"}'")
	wantPG := "COALESCE(extra_config::jsonb, '{}') || '{\"key\":\"val\"}'::jsonb"
	if pgGot != wantPG {
		t.Errorf("PostgresDialect.JSONMergePatch() = %q, want %q", pgGot, wantPG)
	}

	sqliteGot := sqlite.JSONMergePatch("extra_config", "'{\"key\":\"val\"}'")
	wantSQLite := "json_patch(COALESCE(extra_config, '{}'), '{\"key\":\"val\"}')"
	if sqliteGot != wantSQLite {
		t.Errorf("SQLiteDialect.JSONMergePatch() = %q, want %q", sqliteGot, wantSQLite)
	}

	mysqlGot := mysql.JSONMergePatch("extra_config", "'{\"key\":\"val\"}'")
	wantMySQL := "JSON_MERGE_PATCH(COALESCE(extra_config, '{}'), '{\"key\":\"val\"}')"
	if mysqlGot != wantMySQL {
		t.Errorf("MySQLDialect.JSONMergePatch() = %q, want %q", mysqlGot, wantMySQL)
	}
}

// TestDialectInterface ensures all dialects satisfy the Dialect interface
// with the new helper methods.
func TestDialectInterface(t *testing.T) {
	var _ Dialect = &PostgresDialect{}
	var _ Dialect = &SQLiteDialect{}
	var _ Dialect = &MySQLDialect{}
	var _ Dialect = &KingbaseDialect{pg: &PostgresDialect{}}
	var _ Dialect = &OceanBaseDialect{MySQLDialect: &MySQLDialect{}}
}

func TestConditionalCount(t *testing.T) {
	pg := &PostgresDialect{}
	sqlite := &SQLiteDialect{}
	mysql := &MySQLDialect{}
	kingbase := &KingbaseDialect{pg: pg}
	oceanbase := &OceanBaseDialect{MySQLDialect: mysql}

	tests := []struct {
		name         string
		column       string
		value        string
		pgWant       string
		sqliteWant   string
		mysqlWant    string
	}{
		{
			name:       "status column",
			column:     "status",
			value:      "'success'",
			pgWant:     "COUNT(*) FILTER (WHERE status = 'success')",
			sqliteWant: "SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END)",
			mysqlWant:  "SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END)",
		},
		{
			name:       "blocked_by column",
			column:     "blocked_by",
			value:      "'guardrail'",
			pgWant:     "COUNT(*) FILTER (WHERE blocked_by = 'guardrail')",
			sqliteWant: "SUM(CASE WHEN blocked_by = 'guardrail' THEN 1 ELSE 0 END)",
			mysqlWant:  "SUM(CASE WHEN blocked_by = 'guardrail' THEN 1 ELSE 0 END)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pg.ConditionalCount(tt.column, tt.value); got != tt.pgWant {
				t.Errorf("PostgresDialect.ConditionalCount() = %q, want %q", got, tt.pgWant)
			}
			if got := sqlite.ConditionalCount(tt.column, tt.value); got != tt.sqliteWant {
				t.Errorf("SQLiteDialect.ConditionalCount() = %q, want %q", got, tt.sqliteWant)
			}
			if got := mysql.ConditionalCount(tt.column, tt.value); got != tt.mysqlWant {
				t.Errorf("MySQLDialect.ConditionalCount() = %q, want %q", got, tt.mysqlWant)
			}
			// KingbaseES delegates to PG
			if got := kingbase.ConditionalCount(tt.column, tt.value); got != tt.pgWant {
				t.Errorf("KingbaseDialect.ConditionalCount() = %q, want %q", got, tt.pgWant)
			}
			// OceanBase delegates to MySQL
			if got := oceanbase.ConditionalCount(tt.column, tt.value); got != tt.mysqlWant {
				t.Errorf("OceanBaseDialect.ConditionalCount() = %q, want %q", got, tt.mysqlWant)
			}
		})
	}
}

func TestCastFloat(t *testing.T) {
	pg := &PostgresDialect{}
	sqlite := &SQLiteDialect{}
	mysql := &MySQLDialect{}
	kingbase := &KingbaseDialect{pg: pg}
	oceanbase := &OceanBaseDialect{MySQLDialect: mysql}

	tests := []struct {
		name       string
		expr       string
		pgWant     string
		sqliteWant string
		mysqlWant  string
	}{
		{
			name:       "simple expression",
			expr:       "SUM(duration)",
			pgWant:     "SUM(duration)::float",
			sqliteWant: "CAST(SUM(duration) AS DOUBLE)",
			mysqlWant:  "CAST(SUM(duration) AS DOUBLE)",
		},
		{
			name:       "division expression",
			expr:       "total_count / 100.0",
			pgWant:     "total_count / 100.0::float",
			sqliteWant: "CAST(total_count / 100.0 AS DOUBLE)",
			mysqlWant:  "CAST(total_count / 100.0 AS DOUBLE)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pg.CastFloat(tt.expr); got != tt.pgWant {
				t.Errorf("PostgresDialect.CastFloat() = %q, want %q", got, tt.pgWant)
			}
			if got := sqlite.CastFloat(tt.expr); got != tt.sqliteWant {
				t.Errorf("SQLiteDialect.CastFloat() = %q, want %q", got, tt.sqliteWant)
			}
			if got := mysql.CastFloat(tt.expr); got != tt.mysqlWant {
				t.Errorf("MySQLDialect.CastFloat() = %q, want %q", got, tt.mysqlWant)
			}
			// KingbaseES delegates to PG
			if got := kingbase.CastFloat(tt.expr); got != tt.pgWant {
				t.Errorf("KingbaseDialect.CastFloat() = %q, want %q", got, tt.pgWant)
			}
			// OceanBase delegates to MySQL
			if got := oceanbase.CastFloat(tt.expr); got != tt.mysqlWant {
				t.Errorf("OceanBaseDialect.CastFloat() = %q, want %q", got, tt.mysqlWant)
			}
		})
	}
}
