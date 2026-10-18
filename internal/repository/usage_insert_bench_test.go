package repository

// U-07: benchmark-FIRST evidence for the gorm.Config question. The audit
// proposed globally enabling PrepareStmt+SkipDefaultTransaction; the
// supervisor correction requires measuring before changing anything. This
// benchmark compares insert batches under the current bare &gorm.Config{}
// versus the proposed config on the same workload. Numbers land in the
// audit matrix; NO production config is changed by this file.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/crosslink/internal/model"
)

func benchInsertBatch(b *testing.B, cfg *gorm.Config) {
	// :memory: single-connection: no Windows file-lock on cleanup.
	db, err := gorm.Open(sqlite.Open(":memory:"), cfg)
	if err != nil {
		b.Fatal(err)
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	db.Logger = logger.Default.LogMode(logger.Silent)
	if err := db.AutoMigrate(&model.UsageLog{}); err != nil {
		b.Fatal(err)
	}
	repo := NewUsageLogRepo(db)
	ctx := context.Background()

	const batch = 50
	var nextID int64 = 1
	entries := make([]*model.UsageLog, batch)
	for i := range entries {
		eid := fmt.Sprintf("bench-%06d", i)
		id := nextID
		nextID++
		entries[i] = &model.UsageLog{ID: id, EventID: &eid, InputTokens: 100, OutputTokens: 50, Currency: "CNY", CreatedAt: time.Now().UTC()}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range entries {
			eid := fmt.Sprintf("bench-%06d-%06d", i, j)
			entries[j].EventID = &eid
			id := nextID
			nextID++
			entries[j].ID = id
		}
		if err := repo.CreateBatch(ctx, entries); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUsageInsert_CurrentConfig(b *testing.B) {
	benchInsertBatch(b, &gorm.Config{})
}

func BenchmarkUsageInsert_PrepareStmtSkipTx(b *testing.B) {
	// The PROPOSED config — measured, not applied. Note SQLite's driver
	// caches statements differently from PG/MySQL; treat results as a
	// lower-bound signal, then re-measure on the target dialect before any
	// rollout decision.
	benchInsertBatch(b, &gorm.Config{PrepareStmt: true, SkipDefaultTransaction: true})
}
