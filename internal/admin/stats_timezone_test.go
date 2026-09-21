package admin

import (
	"strings"
	"testing"
	"time"
)

func TestDayBucketExpr_DefaultIsPlainDate(t *testing.T) {
	defer SetStatsTimezone(statsTZName) // restore
	if got := dayBucketExpr("postgres", "created_at"); got != "DATE(created_at)" {
		t.Fatalf("no tz configured: got %q, want plain DATE()", got)
	}
}

func TestDayBucketExpr_Postgres(t *testing.T) {
	defer SetStatsTimezone("")
	SetStatsTimezone("Asia/Shanghai")
	got := dayBucketExpr("postgres", "created_at")
	if got != "DATE(created_at AT TIME ZONE 'Asia/Shanghai')" {
		t.Fatalf("postgres bucket expr = %q", got)
	}
}

func TestDayBucketExpr_MySQLUsesNumericOffset(t *testing.T) {
	defer SetStatsTimezone("")
	SetStatsTimezone("Asia/Shanghai")
	got := dayBucketExpr("mysql", "created_at")
	// CONVERT_TZ with named zones needs tz tables; must be a numeric offset.
	if !strings.HasPrefix(got, "DATE(CONVERT_TZ(created_at, '+00:00', '+") {
		t.Fatalf("mysql bucket expr = %q, want numeric-offset CONVERT_TZ", got)
	}
}

func TestDayBucketExpr_SQLiteUnchanged(t *testing.T) {
	defer SetStatsTimezone("")
	SetStatsTimezone("Asia/Shanghai")
	if got := dayBucketExpr("sqlite", "created_at"); got != "DATE(created_at)" {
		t.Fatalf("sqlite bucket expr = %q, want plain DATE()", got)
	}
}

func TestSetStatsTimezone_InvalidFallsBackToLocal(t *testing.T) {
	defer SetStatsTimezone("")
	SetStatsTimezone("") // reset any state from earlier tests
	SetStatsTimezone("Not/ARealZone")
	if statsTZName != "" || statsLoc != time.Local {
		t.Fatalf("invalid zone should keep local defaults, got tz=%q", statsTZName)
	}
}

// TestLocalMidnight_UsesStatsTimezone: 17:00 UTC is already the next calendar
// day in Asia/Shanghai — the day boundary must follow the stats timezone, not
// the process zone.
func TestLocalMidnight_UsesStatsTimezone(t *testing.T) {
	defer SetStatsTimezone("")
	SetStatsTimezone("Asia/Shanghai")

	utc := time.Date(2026, 9, 19, 17, 0, 0, 0, time.UTC) // Beijing 2026-09-20 01:00
	got := localMidnight(utc)
	want := time.Date(2026, 9, 20, 0, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if !got.Equal(want) {
		t.Fatalf("localMidnight(UTC %s) = %s, want %s (Beijing midnight 09-20)", utc, got, want)
	}
}
