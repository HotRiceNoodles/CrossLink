package admin

import (
	"fmt"
	"log/slog"
	"regexp"
	"time"
)

// Statistics timezone: the single timezone that defines "a day" for usage
// statistics (dashboard daily trend, PAT summaries). It must match the
// timezone used by SQL date bucketing — with a session timezone set on the
// connection (DBConfig.Timezone) and dayBucketExpr below, both sides agree.
//
// Default: server process local time (previous behavior).

var (
	statsTZName string
	statsLoc    = time.Local
)

// SetStatsTimezone pins the statistics timezone (IANA name, e.g.
// "Asia/Shanghai"). Empty resets to the process local timezone. Invalid
// names fall back to local with a warning.
func SetStatsTimezone(name string) {
	if name == "" {
		statsTZName = ""
		statsLoc = time.Local
		return
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		slog.Warn("admin: invalid stats timezone, falling back to process local", "timezone", name, "error", err)
		return
	}
	statsTZName = name
	statsLoc = loc
}

var tzNameSanitizer = regexp.MustCompile(`^[A-Za-z0-9_+\-/]+$`)

// dayBucketExpr returns a SQL expression that truncates a timestamp column to
// a calendar date in the statistics timezone instead of the DB session
// default — so early-morning requests land in the user's "today" bucket even
// when the database session runs in UTC.
//
//	postgres: DATE(created_at AT TIME ZONE 'Asia/Shanghai')   (timestamptz → wall clock)
//	mysql:    DATE(CONVERT_TZ(created_at, '+00:00', '+08:00')) (DATETIME stored as UTC wall clock)
//	sqlite:   DATE(created_at)                                 (driver stores with offset)
//
// With no timezone configured the plain DATE(column) is used (server default).
func dayBucketExpr(dialectName, column string) string {
	if statsTZName == "" {
		return "DATE(" + column + ")"
	}
	switch dialectName {
	case "postgres":
		if !tzNameSanitizer.MatchString(statsTZName) {
			return "DATE(" + column + ")"
		}
		return fmt.Sprintf("DATE(%s AT TIME ZONE '%s')", column, statsTZName)
	case "mysql":
		// CONVERT_TZ with named zones needs the MySQL tz tables loaded;
		// a numeric offset always works.
		offset := tzOffsetString(statsTZName)
		if offset == "" {
			return "DATE(" + column + ")"
		}
		return fmt.Sprintf("DATE(CONVERT_TZ(%s, '+00:00', '%s'))", column, offset)
	default:
		return "DATE(" + column + ")"
	}
}

// tzOffsetString renders the timezone's current fixed offset as +HH:MM
// (empty when unknown). Note: DST zones shift — the offset is computed at
// query time, matching the rows being bucketed.
func tzOffsetString(name string) string {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return ""
	}
	_, offset := time.Now().In(loc).Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	return fmt.Sprintf("%s%02d:%02d", sign, offset/3600, (offset%3600)/60)
}
