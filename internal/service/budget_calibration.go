package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type BudgetCalibrationService struct {
	db  *gorm.DB
	rdb *redis.Client
	mu  sync.Mutex
}

func NewBudgetCalibrationService(db *gorm.DB, rdb *redis.Client) *BudgetCalibrationService {
	return &BudgetCalibrationService{db: db, rdb: rdb}
}

func (s *BudgetCalibrationService) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.calibrateAll(ctx)
		}
	}
}

func (s *BudgetCalibrationService) CalibrateOnce(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.calibrateTeams(ctx); err != nil {
		return err
	}
	if err := s.calibrateOrgs(ctx); err != nil {
		return err
	}
	return s.calibrateKeys(ctx)
}

func (s *BudgetCalibrationService) calibrateAll(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.calibrateTeams(ctx); err != nil {
		slog.Warn("budget calibration: teams failed", "error", err)
	}
	if err := s.calibrateOrgs(ctx); err != nil {
		slog.Warn("budget calibration: orgs failed", "error", err)
	}
	if err := s.calibrateKeys(ctx); err != nil {
		slog.Warn("budget calibration: keys failed", "error", err)
	}
}

func (s *BudgetCalibrationService) calibrateTeams(ctx context.Context) error {
	var teams []model.Team
	if err := s.db.WithContext(ctx).Where("budget_limit > 0").Find(&teams).Error; err != nil {
		return fmt.Errorf("query teams: %w", err)
	}
	if len(teams) == 0 {
		return nil
	}

	// Single GROUP BY query instead of N+1
	type spendRow struct {
		TeamID int64
		Spent  float64
	}
	// Aggregate per-period: group teams by period, then one query per period
	byPeriod := make(map[string][]model.Team)
	for _, t := range teams {
		pk := PeriodKey(t.BudgetPeriod)
		byPeriod[pk+"|"+t.BudgetPeriod] = append(byPeriod[pk+"|"+t.BudgetPeriod], t)
	}
	for key, periodTeams := range byPeriod {
		parts := strings.SplitN(key, "|", 2)
		pk, period := parts[0], parts[1]
		start, end := periodBoundaries(period)
		ids := make([]int64, len(periodTeams))
		for i, t := range periodTeams {
			ids[i] = t.ID
		}
		qCtx, qCancel := context.WithTimeout(ctx, 30*time.Second)
		var periodRows []spendRow
		err := s.db.WithContext(qCtx).Model(&model.UsageLog{}).
			Select("team_id, COALESCE(SUM(cost), 0) as spent").
			Where("team_id IN ? AND created_at >= ? AND created_at < ?", ids, start, end).
			Group("team_id").
			Scan(&periodRows).Error
		qCancel()
		if err != nil {
			slog.Warn("budget calibration: failed to aggregate team spend", "period", period, "error", err)
			continue
		}
		spendMap := make(map[int64]float64, len(periodRows))
		for _, r := range periodRows {
			spendMap[r.TeamID] = r.Spent
		}
		for _, team := range periodTeams {
			spent := spendMap[team.ID]
			s.syncRedisAndSnapshot(ctx, "team", team.ID, pk, period, spent, team.BudgetLimit)
		}
	}
	return nil
}

func (s *BudgetCalibrationService) calibrateOrgs(ctx context.Context) error {
	var orgs []model.Organization
	if err := s.db.WithContext(ctx).Where("budget_limit > 0 AND deleted_at IS NULL").Find(&orgs).Error; err != nil {
		return fmt.Errorf("query orgs: %w", err)
	}
	if len(orgs) == 0 {
		return nil
	}

	type spendRow struct {
		OrgID int64 `gorm:"column:org_id"`
		Spent float64
	}
	byPeriod := make(map[string][]model.Organization)
	for _, o := range orgs {
		pk := PeriodKey(o.BudgetPeriod)
		byPeriod[pk+"|"+o.BudgetPeriod] = append(byPeriod[pk+"|"+o.BudgetPeriod], o)
	}
	for key, periodOrgs := range byPeriod {
		parts := strings.SplitN(key, "|", 2)
		pk, period := parts[0], parts[1]
		start, end := periodBoundaries(period)
		ids := make([]int64, len(periodOrgs))
		for i, o := range periodOrgs {
			ids[i] = o.ID
		}
		qCtx, qCancel := context.WithTimeout(ctx, 30*time.Second)
		var periodRows []spendRow
		err := s.db.WithContext(qCtx).Model(&model.UsageLog{}).
			Select("org_id, COALESCE(SUM(cost), 0) as spent").
			Where("org_id IN ? AND created_at >= ? AND created_at < ?", ids, start, end).
			Group("org_id").
			Scan(&periodRows).Error
		qCancel()
		if err != nil {
			slog.Warn("budget calibration: failed to aggregate org spend", "period", period, "error", err)
			continue
		}
		spendMap := make(map[int64]float64, len(periodRows))
		for _, r := range periodRows {
			spendMap[r.OrgID] = r.Spent
		}
		for _, org := range periodOrgs {
			spent := spendMap[org.ID]
			s.syncRedisAndSnapshot(ctx, "org", org.ID, pk, period, spent, org.BudgetLimit)
		}
	}
	return nil
}

func (s *BudgetCalibrationService) calibrateKeys(ctx context.Context) error {
	var keys []model.APIKey
	if err := s.db.WithContext(ctx).Where("max_budget > 0").Find(&keys).Error; err != nil {
		return fmt.Errorf("query keys: %w", err)
	}
	if len(keys) == 0 {
		return nil
	}

	type spendRow struct {
		APIKeyID int64 `gorm:"column:api_key_id"`
		Spent    float64
	}
	byPeriod := make(map[string][]model.APIKey)
	for _, k := range keys {
		pk := PeriodKey(k.BudgetPeriod)
		byPeriod[pk+"|"+k.BudgetPeriod] = append(byPeriod[pk+"|"+k.BudgetPeriod], k)
	}
	for key, periodKeys := range byPeriod {
		parts := strings.SplitN(key, "|", 2)
		pk, period := parts[0], parts[1]
		start, end := periodBoundaries(period)
		ids := make([]int64, len(periodKeys))
		for i, k := range periodKeys {
			ids[i] = k.ID
		}
		qCtx, qCancel := context.WithTimeout(ctx, 30*time.Second)
		var periodRows []spendRow
		err := s.db.WithContext(qCtx).Model(&model.UsageLog{}).
			Select("api_key_id, COALESCE(SUM(cost), 0) as spent").
			Where("api_key_id IN ? AND created_at >= ? AND created_at < ?", ids, start, end).
			Group("api_key_id").
			Scan(&periodRows).Error
		qCancel()
		if err != nil {
			slog.Warn("budget calibration: failed to aggregate key spend", "period", period, "error", err)
			continue
		}
		spendMap := make(map[int64]float64, len(periodRows))
		for _, r := range periodRows {
			spendMap[r.APIKeyID] = r.Spent
		}
		for _, k := range periodKeys {
			spent := spendMap[k.ID]
			s.syncRedisAndSnapshot(ctx, "key", k.ID, pk, period, spent, k.MaxBudget)
		}
	}
	return nil
}

func (s *BudgetCalibrationService) syncRedisAndSnapshot(ctx context.Context, scope string, targetID int64, pk, period string, spent, budget float64) {
	key := fmt.Sprintf("budget:%s:%d:%s", scope, targetID, pk)


	current, err := s.rdb.Get(ctx, key).Float64()
	if err != nil {
		// Key doesn't exist yet — set the authoritative value
		s.rdb.Set(ctx, key, spent, PeriodTTL(period))
	} else {
		delta := spent - current
		if delta != 0 {
			s.rdb.IncrByFloat(ctx, key, delta)
		}
		s.rdb.Expire(ctx, key, PeriodTTL(period))
	}

	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "target_type"}, {Name: "target_id"}, {Name: "period_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"spent", "budget"}),
	}).Create(&model.BudgetSnapshot{
		TargetType: scope,
		TargetID:   targetID,
		PeriodKey:  pk,
		Spent:      spent,
		Budget:     budget,
	}).Error; err != nil {
		slog.Warn("budget calibration: failed to save snapshot", "scope", scope, "id", targetID, "error", err)
	}
}

func periodBoundaries(period string) (start, end time.Time) {
	now := time.Now().UTC()
	switch period {
	case "daily":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 0, 1)
	case "weekly":
		weekday := int(now.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		start = time.Date(now.Year(), now.Month(), now.Day()-weekday+1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 0, 7)
	case "monthly":
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, 0)
	default:
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, 0)
	}
	return
}
