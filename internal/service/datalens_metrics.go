package service

// AggregateLevel defines one aggregation dimension set.
type AggregateLevel struct {
	Name       string   // "global", "by_model", "by_team", etc.
	Dimensions []string // column names from usage_logs: "model_requested", "team_id", etc.
}

// DefaultLevels returns the 7 standard aggregation levels.
func DefaultLevels() []AggregateLevel {
	return []AggregateLevel{
		{Name: "global", Dimensions: nil},
		{Name: "by_model", Dimensions: []string{"model_requested"}},
		{Name: "by_team", Dimensions: []string{"team_id"}},
		{Name: "by_provider", Dimensions: []string{"provider_id"}},
		{Name: "by_key", Dimensions: []string{"api_key_id"}},
		{Name: "team_model", Dimensions: []string{"team_id", "model_requested"}},
		{Name: "key_model", Dimensions: []string{"api_key_id", "model_requested"}},
	}
}

// StatusGroup maps a status_code to a bucket: 200, 400, 429, 500, or 0 (unknown).
func StatusGroup(code int) int {
	switch {
	case code >= 200 && code < 300:
		return 200
	case code == 429:
		return 429
	case code >= 400 && code < 500:
		return 400
	case code >= 500:
		return 500
	default:
		return 0
	}
}

// LatencyBucket returns the index (0-7) for a given latency in ms.
// Buckets: 0-49, 50-99, 100-199, 200-499, 500-999, 1000-1999, 2000-4999, 5000+.
func LatencyBucket(ms int) int {
	switch {
	case ms < 50:
		return 0
	case ms < 100:
		return 1
	case ms < 200:
		return 2
	case ms < 500:
		return 3
	case ms < 1000:
		return 4
	case ms < 2000:
		return 5
	case ms < 5000:
		return 6
	default:
		return 7
	}
}

// ApproxPercentile estimates the latency percentile from histogram buckets.
// buckets is [count_0, count_1, ..., count_7] for the 8 thresholds.
// p is in [0, 100]. Returns estimated latency in ms.
func ApproxPercentile(buckets []int, p float64) int {
	if len(buckets) != 8 {
		return 0
	}
	total := 0
	for _, b := range buckets {
		total += b
	}
	if total == 0 {
		return 0
	}
	// Representative values for each bucket (midpoint)
	thresholds := [8]int{25, 75, 150, 350, 750, 1500, 3500, 7500}
	target := float64(total) * p / 100.0
	cumulative := 0
	for i, count := range buckets {
		prevCum := cumulative
		cumulative += count
		if float64(cumulative) >= target && count > 0 {
			if i == 0 {
				return thresholds[0]
			}
			fraction := (target - float64(prevCum)) / float64(count)
			return thresholds[i-1] + int(fraction*float64(thresholds[i]-thresholds[i-1]))
		}
	}
	return thresholds[7]
}
