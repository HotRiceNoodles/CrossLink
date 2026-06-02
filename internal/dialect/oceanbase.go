package dialect

// OceanBaseDialect implements the Dialect interface for OceanBase (MySQL compatible mode).
//
// OceanBase in MySQL mode speaks the MySQL wire protocol and supports the same
// SQL syntax used by the MySQL dialect (RANGE COLUMNS partitioning, GET_LOCK/RELEASE_LOCK,
// JSON_MERGE_PATCH, etc.). This dialect embeds MySQLDialect and only overrides Name()
// for identification purposes.
type OceanBaseDialect struct {
	*MySQLDialect
}

// NewOceanBaseDialect creates a new OceanBaseDialect with the given config.
func NewOceanBaseDialect(cfg DBConfig) *OceanBaseDialect {
	return &OceanBaseDialect{MySQLDialect: NewMySQLDialect(cfg)}
}

// Name returns the dialect identifier.
func (o *OceanBaseDialect) Name() string { return "oceanbase" }
