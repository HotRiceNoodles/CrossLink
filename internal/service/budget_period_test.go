package service

// U-28 companion: PeriodStart must bucket EXACTLY like PeriodKey (same UTC
// semantics), so a calibrate SUM window and the Redis key it feeds always
// describe the same period.

import (
	"testing"
	"time"
)

func TestPeriodStartBucketsLikePeriodKey(t *testing.T) {
	for _, period := range []string{"daily", "weekly", "monthly", "unknown"} {
		start := PeriodStart(period)
		now := time.Now().UTC()
		if start.After(now) {
			t.Fatalf("%s: period start %v is in the future", period, start)
		}
		// The start must be in the same bucket as now per PeriodKey's rules.
		switch period {
		case "daily":
			if start.Year() != now.Year() || start.Month() != now.Month() || start.Day() != now.Day() {
				t.Fatalf("daily start %v not today", start)
			}
		case "weekly":
			if start.Weekday() != time.Monday {
				t.Fatalf("weekly start %v is not a Monday", start)
			}
			if now.Sub(start) >= 7*24*time.Hour {
				t.Fatalf("weekly start %v more than a week ago", start)
			}
		default: // monthly / unknown
			if start.Year() != now.Year() || start.Month() != now.Month() || start.Day() != 1 {
				t.Fatalf("monthly start %v not the first of the current month", start)
			}
		}
	}
}

func TestPeriodStartWeeklyISOBoundaries(t *testing.T) {
	// 2026-01-05 is a Monday (ISO week 2 of 2026); Jan 4 (Sunday) is week 1.
	for _, tc := range []struct {
		now      string // ISO date at noon UTC
		period   string
		wantYMD  string
	}{
		{"2026-01-05", "weekly", "2026-01-05"}, // Monday itself
		{"2026-01-07", "weekly", "2026-01-05"},
		{"2026-01-04", "weekly", "2025-12-29"}, // Sunday belongs to prior Monday
		{"2026-12-31", "monthly", "2026-12-01"},
		{"2027-01-01", "monthly", "2027-01-01"},
	} {
		now, _ := time.Parse(time.RFC3339, tc.now+"T12:00:00Z")
		got := periodStartAt(tc.period, now)
		if got.Format("2006-01-02") != tc.wantYMD {
			t.Fatalf("%s %s: got %v want %s", tc.now, tc.period, got.Format("2006-01-02"), tc.wantYMD)
		}
	}
}
