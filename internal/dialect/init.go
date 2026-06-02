package dialect

import "fmt"

// New creates a Dialect implementation based on the driver specified in cfg.
func New(cfg DBConfig) (Dialect, error) {
	switch cfg.Driver {
	case "postgres":
		return NewPostgresDialect(cfg), nil
	case "sqlite":
		return NewSQLiteDialect(cfg), nil
	case "mysql":
		return NewMySQLDialect(cfg), nil
	default:
		return nil, fmt.Errorf("unsupported database driver: %q", cfg.Driver)
	}
}
