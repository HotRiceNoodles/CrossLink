package main

import (
	"time"

	"github.com/crosslink/internal/admin"
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/dialect"
)

// readStatsTimezoneOverride opens a short-lived preliminary DB pool (without
// a DSN timezone — the override lives inside the DB, so it cannot ride in
// the DSN yet) and reads the setup-wizard stats_timezone row. Callers treat
// any error as "no override" and fall back to the config value.
func readStatsTimezoneOverride(cfg *config.Config) (string, error) {
	dia, err := dialect.New(dialect.DBConfig{
		Driver:     cfg.Database.Driver,
		Host:       cfg.Database.Host,
		Port:       cfg.Database.Port,
		User:       cfg.Database.User,
		Password:   cfg.Database.Password,
		DBName:     cfg.Database.DBName,
		SSLMode:    cfg.Database.SSLMode,
		SQLitePath: cfg.Database.SQLitePath,
		Timezone:   "",
	})
	if err != nil {
		return "", err
	}
	db, err := dia.InitDB()
	if err != nil {
		return "", err
	}
	defer func() {
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
	}()
	return admin.LoadStatsTimezone(db), nil
}

// resolveEffectiveTimezone applies the precedence: setup-wizard DB row >
// database.timezone config > process local zone. "Local" (the Windows
// zoneinfo placeholder) never reaches the DSN — an empty result omits the
// parameter and the session keeps the server default.
func resolveEffectiveTimezone(dbRow, cfgValue string) string {
	if dbRow != "" && dbRow != "Local" {
		if _, err := time.LoadLocation(dbRow); err == nil {
			return dbRow
		}
	}
	if cfgValue != "" && cfgValue != "Local" {
		return cfgValue
	}
	if name := time.Local.String(); name != "" && name != "Local" {
		return name
	}
	return ""
}
