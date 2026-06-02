package dialect

import "fmt"

// New creates a Dialect implementation based on the driver specified in cfg.
func New(cfg DBConfig) (Dialect, error) {
	switch cfg.Driver {
	case "postgres":
		return NewPostgresDialect(cfg), nil
	case "kingbase", "kingbasees":
		return NewKingbaseDialect(cfg), nil
	case "sqlite":
		return NewSQLiteDialect(cfg), nil
	case "mysql":
		return NewMySQLDialect(cfg), nil
	case "oceanbase":
		return NewOceanBaseDialect(cfg), nil
	default:
		return nil, fmt.Errorf("unsupported database driver: %q (supported: postgres, kingbasees, mysql, oceanbase, sqlite)", cfg.Driver)
	}
}
